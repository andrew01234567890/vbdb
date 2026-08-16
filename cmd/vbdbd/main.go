// Command vbdbd serves the explicitly development-only Milestone 2 gateway.
// Metadata and storage roles remain reserved for later distributed milestones.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/andrew01234567890/vbdb/internal/httpapi"
	"github.com/andrew01234567890/vbdb/internal/storage"
)

const version = "0.2.0-m2"

const (
	maxGatewayConnections    = 256
	maxGatewayActiveRequests = 128
	gracefulShutdownTimeout  = 5 * time.Second
	hardShutdownDeadline     = 10 * time.Second
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("vbdbd", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	role := fs.String("role", "", "process role: gateway, metadata, or storage")
	dataDir := fs.String("data-dir", "", "development gateway data directory")
	listen := fs.String("listen", "", "development gateway listen address")
	showVersion := fs.Bool("version", false, "print the vbdbd version")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	if *showVersion {
		if *role != "" {
			return errors.New("--version cannot be combined with --role")
		}
		fmt.Println(version)
		return nil
	}
	if *role != "gateway" && *role != "metadata" && *role != "storage" {
		return errors.New("--role is required and must be one of gateway, metadata, storage")
	}
	if *role != "gateway" {
		return fmt.Errorf("vbdbd role %q is not implemented in milestone 2", *role)
	}
	if *dataDir == "" || *listen == "" {
		return errors.New("gateway requires --data-dir and --listen")
	}
	return serveGateway(*dataDir, *listen)
}

func serveGateway(dataDir, listen string) (retErr error) {
	return serveGatewayWithExit(dataDir, listen, os.Exit)
}

func serveGatewayWithExit(dataDir, listen string, exit func(int)) (retErr error) {
	return serveGatewayWithReady(dataDir, listen, exit, nil)
}

// serveGatewayWithReady is the same gateway lifecycle with an optional
// readiness callback used by process-level tests. The callback runs only
// after signal.NotifyContext is installed and the Serve goroutine is live.
func serveGatewayWithReady(dataDir, listen string, exit func(int), ready func()) (retErr error) {
	if exit == nil {
		exit = os.Exit
	}
	shutdown := newShutdownLifecycle(exit)
	// Register this defer before the storage-close defer below. Deferred calls
	// run LIFO, so a shutdown timer remains armed while the storage engine closes.
	defer shutdown.stop()
	store, err := storage.Open(dataDir, storage.Options{})
	if err != nil {
		return err
	}
	tracked := newTrackedHandler(httpapi.New(store).Handler())
	defer func() {
		if shutdown.hardExitStarted.Load() {
			// Production hard-exit callbacks do not return. Tests may inject a
			// returning callback; never close storage beneath a stuck handler.
			return
		}
		if closeErr := shutdown.closeStore(store.Close); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("gateway storage close: %w", closeErr))
		}
	}()
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("gateway listen: %w", err)
	}
	limitedListener := newBoundedListener(listener, maxGatewayConnections)
	server := &http.Server{
		Addr:              listen,
		Handler:           tracked,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		WriteTimeout:      30 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(limitedListener) }()
	if ready != nil {
		ready()
	}
	select {
	case <-ctx.Done():
		deadline, triggerHardExit := shutdown.begin(hardShutdownDeadline)
		// Stop admitting requests before beginning either graceful or forced
		// shutdown. This closes the race where a handler starts after Shutdown
		// returns and before the store is closed.
		tracked.stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
		shutdownErr := server.Shutdown(shutdownCtx)
		cancel()
		if shutdownErr != nil {
			// Shutdown leaves active handlers running when its context expires.
			// Force-close connections, drain the listener goroutine, and wait for
			// every handler before the deferred storage-engine close.
			closeErr := server.Close()
			listenErr := <-errCh
			if !waitForTracked(tracked, time.Until(deadline), triggerHardExit) {
				return errors.New("gateway shutdown exceeded hard deadline")
			}
			if shutdown.hardExitStarted.Load() {
				return errors.New("gateway shutdown exceeded hard deadline")
			}
			return errors.Join(
				fmt.Errorf("gateway shutdown: %w", shutdownErr),
				serverError("gateway listen", listenErr),
				serverError("gateway close", closeErr),
			)
		}
		listenErr := <-errCh
		if !waitForTracked(tracked, time.Until(deadline), triggerHardExit) {
			return errors.New("gateway shutdown exceeded hard deadline")
		}
		if shutdown.hardExitStarted.Load() {
			return errors.New("gateway shutdown exceeded hard deadline")
		}
		return serverError("gateway listen", listenErr)
	case err := <-errCh:
		deadline, triggerHardExit := shutdown.begin(hardShutdownDeadline)
		tracked.stop()
		closeErr := server.Close()
		if !waitForTracked(tracked, time.Until(deadline), triggerHardExit) {
			return errors.New("gateway shutdown exceeded hard deadline")
		}
		if shutdown.hardExitStarted.Load() {
			return errors.New("gateway shutdown exceeded hard deadline")
		}
		return errors.Join(serverError("gateway listen", err), serverError("gateway close", closeErr))
	}
}

func serverError(prefix string, err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

type shutdownLifecycle struct {
	hardExitStarted atomic.Bool
	hardExitOnce    sync.Once
	exit            func(int)
	timer           *time.Timer
}

func newShutdownLifecycle(exit func(int)) *shutdownLifecycle {
	return &shutdownLifecycle{exit: exit}
}

func (s *shutdownLifecycle) begin(timeout time.Duration) (time.Time, func()) {
	deadline := time.Now().Add(timeout)
	s.timer = time.AfterFunc(timeout, s.triggerHardExit)
	return deadline, s.triggerHardExit
}

func (s *shutdownLifecycle) triggerHardExit() {
	s.hardExitOnce.Do(func() {
		s.hardExitStarted.Store(true)
		s.exit(1)
	})
}

func (s *shutdownLifecycle) closeStore(closeFn func() error) error {
	if s.hardExitStarted.Load() {
		return nil
	}
	return closeFn()
}

func (s *shutdownLifecycle) stop() {
	if s.timer != nil {
		s.timer.Stop()
	}
}

func waitForTracked(handler *trackedHandler, timeout time.Duration, onTimeout func()) bool {
	done := make(chan struct{})
	go func() {
		handler.wait()
		close(done)
	}()
	if timeout <= 0 {
		onTimeout()
		return false
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		onTimeout()
		return false
	}
}

// boundedListener applies a fixed process-side connection admission limit.
// When full, Accept waits without creating application goroutines or an
// unbounded queue; closing the listener wakes that wait during shutdown.
type boundedListener struct {
	net.Listener
	slots     chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func newBoundedListener(listener net.Listener, limit int) *boundedListener {
	if limit < 1 {
		panic("gateway connection limit must be positive")
	}
	return &boundedListener{
		Listener: listener,
		slots:    make(chan struct{}, limit),
		closed:   make(chan struct{}),
	}
}

func (l *boundedListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.closed:
		return nil, net.ErrClosed
	}
	conn, err := l.Listener.Accept()
	if err != nil {
		l.release()
		return nil, err
	}
	return &boundedConn{Conn: conn, release: l.release}, nil
}

func (l *boundedListener) release() {
	select {
	case <-l.slots:
	default:
	}
}

func (l *boundedListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closed)
		l.closeErr = l.Listener.Close()
	})
	return l.closeErr
}

type boundedConn struct {
	net.Conn
	releaseOnce sync.Once
	release     func()
}

func (c *boundedConn) Close() error {
	err := c.Conn.Close()
	c.releaseOnce.Do(c.release)
	return err
}

// trackedHandler prevents the storage engine from being closed while a
// handler is still using it. stop rejects requests that race with shutdown;
// in-flight requests are drained by wait. The request slot is acquired before
// the downstream handler can buffer a request body.
type trackedHandler struct {
	next         http.Handler
	requestSlots chan struct{}
	mu           sync.Mutex
	cond         *sync.Cond
	open         int
	stopping     bool
}

func newTrackedHandler(next http.Handler) *trackedHandler {
	return newTrackedHandlerWithLimit(next, maxGatewayActiveRequests)
}

func newTrackedHandlerWithLimit(next http.Handler, limit int) *trackedHandler {
	if limit < 1 {
		panic("gateway active request limit must be positive")
	}
	handler := &trackedHandler{next: next, requestSlots: make(chan struct{}, limit)}
	handler.cond = sync.NewCond(&handler.mu)
	return handler
}

func (h *trackedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		httpapi.WriteProblem(w, http.StatusServiceUnavailable, "server_shutting_down", "The server is shutting down.")
		return
	}
	if h.requestSlots != nil {
		select {
		case h.requestSlots <- struct{}{}:
		default:
			h.mu.Unlock()
			httpapi.WriteProblem(w, http.StatusServiceUnavailable, "server_busy", "The server is at its active request limit.")
			return
		}
	}
	h.open++
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.open--
		if h.requestSlots != nil {
			<-h.requestSlots
		}
		h.cond.Broadcast()
		h.mu.Unlock()
	}()
	h.next.ServeHTTP(w, r)
}

func (h *trackedHandler) stop() {
	h.mu.Lock()
	h.stopping = true
	h.mu.Unlock()
}

func (h *trackedHandler) wait() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for h.open != 0 {
		h.cond.Wait()
	}
}
