package raftstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrew01234567890/vbdb/internal/storage"
	"github.com/andrew01234567890/vbdb/pkg/uuidv7"
	raft "go.etcd.io/raft/v3"
	pb "go.etcd.io/raft/v3/raftpb"
	"google.golang.org/protobuf/proto"
)

func deterministicUUIDs() func() (uuidv7.UUID, error) {
	var n byte
	return func() (uuidv7.UUID, error) {
		n++
		return uuidv7.Generator{
			Now:  func() time.Time { return time.UnixMilli(100 + int64(n)) },
			Rand: bytes.NewReader(bytes.Repeat([]byte{n}, 10)),
		}.New()
	}
}

func TestRF3SingleNodeApplyRestartAndDedupe(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "replica")
	r, err := Open(Options{ID: 1, Dir: dir, Peers: []raft.Peer{{ID: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.Status().Applied < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := r.Campaign(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for r.Status().Leader != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if r.Status().Leader != 1 {
		t.Fatalf("single node did not become leader: %#v", r.Status())
	}
	command, err := NewPut("users", "ada", []byte(`{"name":"Ada"}`), storage.Condition{}, deterministicUUIDs())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	result, err := r.Propose(ctx, command)
	cancel()
	if err != nil || result.Outcome != OutcomeSuccess {
		t.Fatalf("propose = %#v, %v", result, err)
	}
	row, err := r.GetLocal("users", "ada")
	if err != nil || row.Sequence == 0 || !bytes.Equal(row.Value, command.Value) {
		t.Fatalf("local row = %#v, %v", row, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r, err = Open(Options{ID: 1, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	replayed, ok, err := r.LookupResult(command.OperationID)
	if err != nil || !ok || replayed.Command.OperationID != command.OperationID {
		t.Fatalf("lookup after restart = %#v, %v, %v", replayed, ok, err)
	}
	duplicate, err := r.Propose(context.Background(), command)
	if err != nil || duplicate.Outcome != OutcomeSuccess || duplicate.Applied.Row.Version != command.Version {
		t.Fatalf("duplicate proposal = %#v, %v", duplicate, err)
	}
	conflict := command
	conflict.Value = []byte(`{"name":"Grace"}`)
	if _, err := r.Propose(context.Background(), conflict); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("operation conflict = %v", err)
	}
}

func TestRejectObsoletePebbleDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CURRENT"), []byte("obsolete"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{ID: 1, Dir: dir, Peers: []raft.Peer{{ID: 1}}}); err == nil || !strings.Contains(err.Error(), "Pebble") {
		t.Fatalf("obsolete directory result = %v", err)
	}
}

type rf3Envelope struct {
	from, to uint64
	message  *pb.Message
}

// rf3Transport is deliberately deterministic: tests control delivery order
// and link partitions without introducing a production transport claim.
type rf3Transport struct {
	mu           sync.Mutex
	nodes        map[uint64]*Replica
	queue        []*rf3Envelope
	blocked      map[[2]uint64]bool
	blockedSends map[[2]uint64]int
	readyByNode  map[uint64]int
	readyNotice  chan struct{}
	reverse      bool
}

const (
	maxRF3ConditionWait      = 5 * time.Second
	maxRF3QuiescenceMessages = 512
	maxRF3QuiescenceWait     = time.Second
	maxRF3ReadyPollWait      = 10 * time.Millisecond
)

func newRF3Transport() *rf3Transport {
	return &rf3Transport{
		nodes:        make(map[uint64]*Replica),
		blocked:      make(map[[2]uint64]bool),
		blockedSends: make(map[[2]uint64]int),
		readyByNode:  make(map[uint64]int),
		readyNotice:  make(chan struct{}),
	}
}

func (t *rf3Transport) signalReady(id uint64) {
	t.mu.Lock()
	t.readyByNode[id]++
	close(t.readyNotice)
	t.readyNotice = make(chan struct{})
	t.mu.Unlock()
}

func (t *rf3Transport) waitNodesReady(ctx context.Context, nodes []*Replica) error {
	for {
		t.mu.Lock()
		ready := true
		for _, node := range nodes {
			if t.readyByNode[node.id] == 0 {
				ready = false
				break
			}
		}
		notice := t.readyNotice
		t.mu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-notice:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (t *rf3Transport) readyCount(id uint64) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.readyByNode[id]
}

func (t *rf3Transport) waitReady(ctx context.Context, id uint64, before int) error {
	for {
		t.mu.Lock()
		after := t.readyByNode[id]
		notice := t.readyNotice
		t.mu.Unlock()
		if after > before {
			return nil
		}
		select {
		case <-notice:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (t *rf3Transport) readyCounts(nodes []*Replica) map[uint64]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	counts := make(map[uint64]int, len(nodes))
	for _, node := range nodes {
		counts[node.id] = t.readyByNode[node.id]
	}
	return counts
}

// waitAnyReadyBounded observes Ready progress opportunistically, but never
// turns a silent Tick into a wait until the phase deadline. The caller's
// condition is checked while waiting, and the short polling timer bounds how
// long a phase waits before issuing another logical Tick.
func (t *rf3Transport) waitAnyReadyBounded(ctx context.Context, nodes []*Replica, before map[uint64]int, condition func() bool) bool {
	wait := maxRF3ReadyPollWait
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
	}
	if wait <= 0 {
		return condition()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		if condition() {
			return true
		}
		t.mu.Lock()
		ready := false
		for _, node := range nodes {
			if t.readyByNode[node.id] > before[node.id] {
				ready = true
				break
			}
		}
		notice := t.readyNotice
		t.mu.Unlock()
		if ready {
			return condition()
		}
		select {
		case <-notice:
			continue
		case <-timer.C:
			return condition()
		case <-ctx.Done():
			return condition()
		}
	}
}

func rf3MessageProducesReady(messageType pb.MessageType) bool {
	switch messageType {
	case pb.MsgApp, pb.MsgHeartbeat, pb.MsgVote, pb.MsgPreVote, pb.MsgReadIndex:
		return true
	default:
		return false
	}
}

func (t *rf3Transport) Send(ctx context.Context, to uint64, message *pb.Message) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	link := [2]uint64{message.GetFrom(), to}
	if t.blocked[link] {
		t.blockedSends[link]++
		return nil
	}
	t.queue = append(t.queue, &rf3Envelope{from: message.GetFrom(), to: to, message: proto.Clone(message).(*pb.Message)})
	return nil
}

func (t *rf3Transport) SendSnapshot(ctx context.Context, to uint64, message *pb.Message) error {
	t.mu.Lock()
	blocked := t.blocked[[2]uint64{message.GetFrom(), to}]
	peer := t.nodes[to]
	t.mu.Unlock()
	if blocked || peer == nil {
		return errors.New("rf3: snapshot link unavailable")
	}
	if err := peer.Step(ctx, message); err != nil {
		return err
	}
	return peer.waitApplied(ctx, message.GetSnapshot().GetMetadata().GetIndex())
}

func (t *rf3Transport) setPartition(a, b uint64, blocked bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.blocked[[2]uint64{a, b}] = blocked
	t.blocked[[2]uint64{b, a}] = blocked
}

func (t *rf3Transport) deliverOne(ctx context.Context, index int) (bool, error) {
	t.mu.Lock()
	if len(t.queue) == 0 {
		t.mu.Unlock()
		return false, nil
	}
	if index < 0 || index >= len(t.queue) {
		if t.reverse {
			index = len(t.queue) - 1
		} else {
			index = 0
		}
	}
	envelope := t.queue[index]
	t.queue = append(t.queue[:index], t.queue[index+1:]...)
	link := [2]uint64{envelope.from, envelope.to}
	blocked := t.blocked[link]
	if blocked {
		t.blockedSends[link]++
	}
	peer := t.nodes[envelope.to]
	t.mu.Unlock()
	if blocked || peer == nil {
		return true, nil
	}
	beforeReady := t.readyCount(peer.id)
	if err := peer.Step(ctx, envelope.message); err != nil {
		return true, fmt.Errorf("rf3: deliver %d -> %d %s: %w", envelope.from, envelope.to, envelope.message.GetType(), err)
	}
	if rf3MessageProducesReady(envelope.message.GetType()) {
		if err := t.waitReady(ctx, peer.id, beforeReady); err != nil {
			return true, fmt.Errorf("rf3: wait for Ready after delivering %d -> %d %s: %w", envelope.from, envelope.to, envelope.message.GetType(), err)
		}
	}
	return true, nil
}

// quiesce drains all currently available transport work and yields between
// empty observations so an asynchronous Ready loop can publish its messages.
// The bounded message budget is a safety limit, not a progress assumption; a
// caller waits on a substantive condition in driveUntil.
func (t *rf3Transport) quiesce(ctx context.Context, nodes []*Replica) error {
	for message := 0; message < maxRF3QuiescenceMessages; message++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		delivered, err := t.deliverOne(ctx, -1)
		if err != nil {
			return err
		}
		if delivered {
			runtime.Gosched()
			continue
		}
		// A Ready loop may be between persistence/apply and Send. Give it a
		// scheduler handoff, then confirm the queue is still empty before
		// declaring this drive quiescent.
		runtime.Gosched()
		delivered, err = t.deliverOne(ctx, -1)
		if err != nil {
			return err
		}
		if delivered {
			runtime.Gosched()
			continue
		}
		t.mu.Lock()
		queued := len(t.queue)
		t.mu.Unlock()
		if queued == 0 {
			return nil
		}
	}
	return fmt.Errorf("rf3: transport did not quiesce after %d messages (%s)", maxRF3QuiescenceMessages, t.diagnostics(nodes))
}

func (t *rf3Transport) drive(ctx context.Context, nodes ...*Replica) error {
	for _, node := range nodes {
		node.Tick()
		// Status completes a round trip through the raft node, preventing a
		// silent tick from being outrun by an unbounded loop of logical ticks.
		// It does not require that every tick emit a Ready.
		_ = node.Status()
	}
	return t.quiesce(ctx, nodes)
}

func (t *rf3Transport) diagnostics(nodes []*Replica) string {
	t.mu.Lock()
	queued := len(t.queue)
	blocked := make(map[[2]uint64]int, len(t.blockedSends))
	for link, count := range t.blockedSends {
		blocked[link] = count
	}
	t.mu.Unlock()
	statuses := make([]Diagnostic, len(nodes))
	for i, node := range nodes {
		statuses[i] = node.Status()
	}
	return fmt.Sprintf("statuses=%#v queued=%d blocked=%v", statuses, queued, blocked)
}

func (t *rf3Transport) blockedCount(from, to uint64) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.blockedSends[[2]uint64{from, to}]
}

func (t *rf3Transport) partitionTrafficCount() int {
	return t.blockedCount(1, 2) + t.blockedCount(2, 1) + t.blockedCount(1, 3) + t.blockedCount(3, 1)
}

func (t *rf3Transport) driveUntil(tb testing.TB, label string, timeout time.Duration, nodes []*Replica, condition func() bool) {
	t.driveUntilMode(tb, label, timeout, nodes, false, condition)
}

// driveUntilReady keeps the historical name for phases that drive a node
// clock, but a Tick is allowed to produce no Ready. Recheck the substantive
// condition after every bounded drive instead of waiting for one Ready for
// the entire remaining deadline.
func (t *rf3Transport) driveUntilReady(tb testing.TB, label string, timeout time.Duration, nodes []*Replica, condition func() bool) {
	t.driveUntilMode(tb, label, timeout, nodes, true, condition)
}

func (t *rf3Transport) driveUntilMode(tb testing.TB, label string, timeout time.Duration, nodes []*Replica, awaitReady bool, condition func() bool) {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	drives := 0
	for {
		if condition() {
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			tb.Fatalf("RF3 %s condition not met after %d bounded drives in %s: %s", label, drives, timeout, t.diagnostics(nodes))
		}
		ctx, cancel := context.WithTimeout(context.Background(), remaining)
		beforeReady := t.readyCounts(nodes)
		drives++
		err := t.drive(ctx, nodes...)
		cancel()
		if err != nil {
			tb.Fatalf("RF3 %s quiescence failed at drive %d: %v", label, drives, err)
		}
		if awaitReady {
			ctx, cancel = context.WithTimeout(context.Background(), time.Until(deadline))
			done := t.waitAnyReadyBounded(ctx, nodes, beforeReady, condition)
			cancel()
			if done {
				return
			}
		}
		runtime.Gosched()
	}
}

func (t *rf3Transport) driveSetup(tb testing.TB, nodes []*Replica) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), maxRF3QuiescenceWait)
	err := t.waitNodesReady(ctx, nodes)
	cancel()
	if err != nil {
		tb.Fatalf("RF3 bootstrap Ready completion failed: %v (%s)", err, t.diagnostics(nodes))
	}
	ctx, cancel = context.WithTimeout(context.Background(), maxRF3QuiescenceWait)
	err = t.quiesce(ctx, nodes)
	cancel()
	if err != nil {
		tb.Fatalf("RF3 bootstrap quiescence failed: %v", err)
	}
}

func newRF3TestCluster(tb testing.TB) (*rf3Transport, []*Replica) {
	tb.Helper()
	transport := newRF3Transport()
	peers := []raft.Peer{{ID: 1}, {ID: 2}, {ID: 3}}
	nodes := make([]*Replica, 3)
	for i := range nodes {
		id := uint64(i + 1)
		node, err := Open(Options{ID: id, Dir: filepath.Join(tb.TempDir(), "node"), Peers: peers, Transport: transport, ElectionTick: 100, HeartbeatTick: 1, readyHook: func() { transport.signalReady(id) }})
		if err != nil {
			tb.Fatal(err)
		}
		nodes[i] = node
		transport.nodes[uint64(i+1)] = node
	}
	tb.Cleanup(func() {
		for _, node := range nodes {
			_ = node.Close()
		}
	})
	return transport, nodes
}

func TestDeterministicRF3CampaignWaitsForLeader(t *testing.T) {
	transport, nodes := newRF3TestCluster(t)
	transport.driveSetup(t, nodes)
	if err := nodes[0].Campaign(context.Background()); err != nil {
		t.Fatal(err)
	}
	transport.driveUntilReady(t, "leader election", maxRF3ConditionWait, nodes[:1], func() bool { return nodes[0].Status().Leader == 1 })
}

func TestDeterministicRF3PartitionReorderAndRetry(t *testing.T) {
	transport, nodes := newRF3TestCluster(t)
	transport.driveSetup(t, nodes)
	if err := nodes[0].Campaign(context.Background()); err != nil {
		t.Fatal(err)
	}
	transport.driveUntilReady(t, "leader election", maxRF3ConditionWait, nodes[:1], func() bool { return nodes[0].Status().Leader == 1 })
	command, err := NewPut("users", "rf3", []byte(`{"n":1}`), storage.Condition{}, deterministicUUIDs())
	if err != nil {
		t.Fatal(err)
	}
	leaderBeforePartition := nodes[0].Status()
	if leaderBeforePartition.Leader != 1 {
		t.Fatalf("RF3 proposal leader = %#v, want node 1", leaderBeforePartition)
	}
	// Isolate one follower while the other follower remains connected, so the
	// proposal commits on a quorum without pre-populating node 2's result.
	transport.setPartition(1, 2, true)
	partitionStart := transport.blockedCount(1, 2)
	transport.reverse = true
	resultCh := make(chan error, 1)
	go func() { _, err := nodes[0].Propose(context.Background(), command); resultCh <- err }()
	var proposalErr error
	transport.driveUntilReady(t, "proposal commit", maxRF3ConditionWait, nodes[:1], func() bool {
		select {
		case proposalErr = <-resultCh:
			return true
		default:
			return false
		}
	})
	if proposalErr != nil {
		t.Fatal(proposalErr)
	}
	transport.driveUntil(t, "partition", maxRF3ConditionWait, nodes[:1], func() bool {
		return transport.blockedCount(1, 2) > partitionStart
	})
	if _, ok, err := nodes[1].LookupResult(command.OperationID); err != nil {
		t.Fatalf("RF3 partitioned follower lookup = %v", err)
	} else if ok {
		t.Fatal("RF3 partitioned follower already has the committed result")
	}
	leaderDuringPartition := nodes[0].Status()
	if leaderDuringPartition.Leader != leaderBeforePartition.Leader || leaderDuringPartition.Term != leaderBeforePartition.Term {
		t.Fatalf("RF3 leader changed during partition: before=%#v during=%#v", leaderBeforePartition, leaderDuringPartition)
	}
	transport.setPartition(1, 2, false)
	var retry Result
	var retryOK bool
	var retryErr error
	transport.driveUntilReady(t, "follower retry lookup", maxRF3ConditionWait, nodes[:1], func() bool {
		retry, retryOK, retryErr = nodes[1].LookupResult(command.OperationID)
		return retryErr != nil || retryOK
	})
	if retryErr != nil || !retryOK {
		t.Fatalf("follower retry lookup = %v, %v", retryOK, retryErr)
	}
	if retry.Command.OperationID != command.OperationID || retry.Command.Table != command.Table || retry.Command.Key != command.Key || !bytes.Equal(retry.Command.Value, command.Value) || retry.Command.Version != command.Version || !retry.Succeeded() {
		t.Fatalf("follower retry result = %#v, want successful command %#v", retry, command)
	}
	leaderAfterHeal := nodes[0].Status()
	if leaderAfterHeal.Leader != leaderBeforePartition.Leader || leaderAfterHeal.Term != leaderBeforePartition.Term {
		t.Fatalf("RF3 leader changed after partition heal: before=%#v after=%#v", leaderBeforePartition, leaderAfterHeal)
	}
	transport.reverse = false
}

func TestBoundedSnapshotRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "snapshot")
	r, err := Open(Options{ID: 1, Dir: dir, Peers: []raft.Peer{{ID: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.Status().Applied < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := r.Campaign(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for r.Status().Leader != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	command, err := NewPut("users", "snapshot", []byte(`{"value":"bounded"}`), storage.Condition{}, deterministicUUIDs())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Propose(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateSnapshot(); err != nil {
		t.Fatal(err)
	}
	data, err := r.ExportState()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := r.disk.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Data = data
	r.stateMu.Lock()
	err = r.state.installSnapshot(r.disk, snapshot)
	r.stateMu.Unlock()
	if err != nil {
		t.Fatalf("generation snapshot install = %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = Open(Options{ID: 1, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	row, err := r.GetLocal("users", "snapshot")
	if err != nil || !bytes.Equal(row.Value, command.Value) {
		t.Fatalf("snapshot restart row = %#v, %v", row, err)
	}
	if got := r.Status().Applied; got == 0 {
		t.Fatalf("snapshot restart lost applied index: %d", got)
	}
}
