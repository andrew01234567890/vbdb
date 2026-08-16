package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunVersion(t *testing.T) {
	if err := run([]string{"--version"}); err != nil {
		t.Fatal(err)
	}
}

func TestRunHelp(t *testing.T) {
	if err := run([]string{"--help"}); err != nil {
		t.Fatalf("run --help returned %v", err)
	}
}

func TestRunRequiresStrictRole(t *testing.T) {
	for _, args := range [][]string{
		{}, {"--role", "unknown"}, {"--role", "gateway", "extra"}, {"--version", "--role", "gateway"},
	} {
		if err := run(args); err == nil {
			t.Errorf("run(%v) unexpectedly succeeded", args)
		}
	}
}

func TestRunRejectsUnimplementedRoleHonestly(t *testing.T) {
	err := run([]string{"--role", "storage"})
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("run returned %v, want explicit not-implemented error", err)
	}
}

func TestGatewayRequiresDataDirectoryAndListenAddress(t *testing.T) {
	for _, args := range [][]string{{"--role", "gateway"}, {"--role", "gateway", "--data-dir", "/tmp/vbdb-test"}} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "requires --data-dir and --listen") {
			t.Errorf("run(%v) = %v, want gateway flag validation", args, err)
		}
	}
}

func TestTrackedHandlerDrainsAndRejectsDuringShutdown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handler := newTrackedHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	}))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	activeResponse := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(activeResponse, request)
		close(done)
	}()
	<-started
	handler.stop()
	waitDone := make(chan struct{})
	go func() {
		handler.wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		t.Fatal("wait returned while a handler was active")
	default:
	}
	close(release)
	<-done
	<-waitDone

	rejected := httptest.NewRecorder()
	handler.ServeHTTP(rejected, httptest.NewRequest(http.MethodGet, "/", nil))
	if rejected.Code != http.StatusServiceUnavailable || rejected.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("request after stop = %d, want %d", rejected.Code, http.StatusServiceUnavailable)
	}
	var problem struct {
		Code   string `json:"code"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rejected.Body.Bytes(), &problem); err != nil {
		t.Fatalf("shutdown problem JSON = %v, body=%s", err, rejected.Body)
	}
	if problem.Code != "server_shutting_down" || problem.Title != "Service Unavailable" || problem.Status != http.StatusServiceUnavailable || problem.Detail != "The server is shutting down." {
		t.Fatalf("shutdown problem = %#v", problem)
	}
}

func TestTrackedHandlerRejectsWhenActiveRequestBudgetIsFull(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handler := newTrackedHandlerWithLimit(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	}), 1)
	go handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/users/a", strings.NewReader(strings.Repeat("x", 1024))))
	<-started
	rejected := httptest.NewRecorder()
	handler.ServeHTTP(rejected, httptest.NewRequest(http.MethodPut, "/users/b", strings.NewReader("true")))
	if rejected.Code != http.StatusServiceUnavailable || rejected.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("busy request = %d content-type=%q body=%s", rejected.Code, rejected.Header().Get("Content-Type"), rejected.Body)
	}
	var problem map[string]any
	if err := json.Unmarshal(rejected.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem["code"] != "server_busy" || problem["status"] != float64(http.StatusServiceUnavailable) {
		t.Fatalf("busy problem = %#v", problem)
	}
	close(release)
	if !waitForTracked(handler, time.Second, func() { t.Fatal("active request did not drain") }) {
		t.Fatal("active request wait timed out")
	}
}

func TestWaitForTrackedHasHardDeadline(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handler := newTrackedHandlerWithLimit(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	}), 1)
	go handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	<-started
	exited := make(chan int, 1)
	if waitForTracked(handler, 10*time.Millisecond, func() { exited <- 1 }) {
		t.Fatal("stuck handler unexpectedly drained")
	}
	select {
	case code := <-exited:
		if code != 1 {
			t.Fatalf("hard-exit code = %d", code)
		}
	default:
		t.Fatal("hard-exit callback was not invoked")
	}
	close(release)
	if !waitForTracked(handler, time.Second, func() { t.Fatal("released request did not drain") }) {
		t.Fatal("released request wait timed out")
	}
}

func TestHardDeadlineCoversBlockedCloseAndDisarmsAfterSuccess(t *testing.T) {
	exited := make(chan struct{})
	lifecycle := newShutdownLifecycle(func(int) { close(exited) })
	defer lifecycle.stop()
	lifecycle.begin(10 * time.Millisecond)
	started := make(chan struct{})
	release := make(chan struct{})
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- lifecycle.closeStore(func() error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("blocked close did not trigger hard exit")
	}
	close(release)
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}

	successfulExited := make(chan struct{})
	successful := newShutdownLifecycle(func(int) { close(successfulExited) })
	successful.begin(10 * time.Millisecond)
	if err := successful.closeStore(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	successful.stop()
	select {
	case <-successfulExited:
		t.Fatal("successful close triggered hard exit")
	case <-time.After(30 * time.Millisecond):
	}
}

func TestBoundedListenerLimitsConnections(t *testing.T) {
	base := newPipeListener()
	limited := newBoundedListener(base, 1)
	defer limited.Close()

	client1, server1 := net.Pipe()
	base.conns <- server1
	conn1, err := limited.Accept()
	if err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := limited.Accept()
		if err == nil {
			accepted <- conn
		}
		close(blocked)
	}()
	select {
	case <-blocked:
		t.Fatal("second connection was accepted before capacity was released")
	case <-time.After(10 * time.Millisecond):
	}
	_ = conn1.Close()
	_ = client1.Close()
	client2, server2 := net.Pipe()
	base.conns <- server2
	select {
	case conn2 := <-accepted:
		_ = conn2.Close()
		_ = client2.Close()
	case <-time.After(time.Second):
		t.Fatal("second connection was not accepted after capacity was released")
	}
}

type pipeListener struct {
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn, 2), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestServeGatewayShutdownWiring(t *testing.T) {
	const childEnv = "VBDB_SERVE_GATEWAY_SHUTDOWN_CHILD"
	if os.Getenv(childEnv) == "1" {
		readyFile := os.NewFile(uintptr(3), "serve-gateway-ready")
		if readyFile == nil {
			fmt.Fprintln(os.Stderr, "missing readiness pipe")
			os.Exit(1)
		}
		defer readyFile.Close()
		ready := func() {
			if _, err := readyFile.Write([]byte{1}); err != nil {
				fmt.Fprintln(os.Stderr, "write readiness signal:", err)
				os.Exit(1)
			}
		}
		if err := serveGatewayWithReady(os.Getenv("VBDB_SERVE_GATEWAY_DATA"), "127.0.0.1:0", os.Exit, ready); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	_ = probe.Close()
	dir := filepath.Join(t.TempDir(), "data")
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestServeGatewayShutdownWiring$", "-test.v")
	command.Env = append(os.Environ(), childEnv+"=1", "VBDB_SERVE_GATEWAY_DATA="+dir)
	command.ExtraFiles = []*os.File{readyWriter}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		_ = readyReader.Close()
		_ = readyWriter.Close()
		t.Fatal(err)
	}
	_ = readyWriter.Close()
	waited := false
	t.Cleanup(func() {
		_ = readyReader.Close()
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	readyResult := make(chan error, 1)
	go func() {
		_, readErr := io.ReadFull(readyReader, []byte{0})
		readyResult <- readErr
	}()
	select {
	case err := <-readyResult:
		if err != nil {
			t.Fatalf("serveGateway child readiness: %v output=%s", err, output.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("serveGateway child did not become ready; output=%s", output.String())
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("serveGateway child = %v output=%s", err, output.String())
	}
	waited = true
}
