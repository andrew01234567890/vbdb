package raftstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
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
	mu      sync.Mutex
	nodes   map[uint64]*Replica
	queue   []*rf3Envelope
	blocked map[[2]uint64]bool
	reverse bool
}

const maxRF3ElectionDrives = 128

func newRF3Transport() *rf3Transport {
	return &rf3Transport{nodes: make(map[uint64]*Replica), blocked: make(map[[2]uint64]bool)}
}

func (t *rf3Transport) Send(ctx context.Context, to uint64, message *pb.Message) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.blocked[[2]uint64{message.GetFrom(), to}] {
		t.queue = append(t.queue, &rf3Envelope{from: message.GetFrom(), to: to, message: proto.Clone(message).(*pb.Message)})
	}
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

func (t *rf3Transport) deliverOne(index int) bool {
	t.mu.Lock()
	if len(t.queue) == 0 {
		t.mu.Unlock()
		return false
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
	blocked := t.blocked[[2]uint64{envelope.from, envelope.to}]
	peer := t.nodes[envelope.to]
	t.mu.Unlock()
	if blocked || peer == nil {
		return true
	}
	_ = peer.Step(context.Background(), envelope.message)
	return true
}

func (t *rf3Transport) drive(nodes ...*Replica) {
	for _, node := range nodes {
		node.Tick()
	}
	time.Sleep(time.Millisecond)
	for t.deliverOne(-1) {
	}
}

func (t *rf3Transport) driveUntil(tb testing.TB, maxDrives int, nodes []*Replica, condition func() bool) {
	tb.Helper()
	for drive := 0; drive < maxDrives; drive++ {
		t.drive(nodes...)
		if condition() {
			return
		}
	}
	t.mu.Lock()
	queued := len(t.queue)
	t.mu.Unlock()
	statuses := make([]Diagnostic, len(nodes))
	for i, node := range nodes {
		statuses[i] = node.Status()
	}
	tb.Fatalf("condition not met after %d deterministic drives: statuses=%#v queued=%d", maxDrives, statuses, queued)
}

func newRF3TestCluster(tb testing.TB) (*rf3Transport, []*Replica) {
	tb.Helper()
	transport := newRF3Transport()
	peers := []raft.Peer{{ID: 1}, {ID: 2}, {ID: 3}}
	nodes := make([]*Replica, 3)
	for i := range nodes {
		node, err := Open(Options{ID: uint64(i + 1), Dir: filepath.Join(tb.TempDir(), "node"), Peers: peers, Transport: transport, ElectionTick: 100, HeartbeatTick: 1})
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
	for i := 0; i < 4; i++ {
		transport.drive(nodes...)
	}
	if err := nodes[0].Campaign(context.Background()); err != nil {
		t.Fatal(err)
	}
	transport.driveUntil(t, maxRF3ElectionDrives, nodes, func() bool { return nodes[0].Status().Leader == 1 })
}

func TestDeterministicRF3PartitionReorderAndRetry(t *testing.T) {
	transport, nodes := newRF3TestCluster(t)
	for i := 0; i < 4; i++ {
		transport.drive(nodes...)
	}
	if err := nodes[0].Campaign(context.Background()); err != nil {
		t.Fatal(err)
	}
	transport.driveUntil(t, maxRF3ElectionDrives, nodes, func() bool { return nodes[0].Status().Leader == 1 })
	command, err := NewPut("users", "rf3", []byte(`{"n":1}`), storage.Condition{}, deterministicUUIDs())
	if err != nil {
		t.Fatal(err)
	}
	transport.reverse = true
	resultCh := make(chan error, 1)
	go func() { _, err := nodes[0].Propose(context.Background(), command); resultCh <- err }()
	for i := 0; i < 80; i++ {
		transport.drive(nodes...)
		select {
		case err := <-resultCh:
			if err != nil {
				t.Fatal(err)
			}
			goto committed
		default:
		}
	}
	t.Fatal("RF3 proposal did not commit")
committed:
	transport.setPartition(1, 2, true)
	transport.setPartition(1, 3, true)
	for i := 0; i < 20; i++ {
		transport.drive(nodes[1], nodes[2])
	}
	transport.setPartition(1, 2, false)
	transport.setPartition(1, 3, false)
	for i := 0; i < 40; i++ {
		transport.drive(nodes...)
	}
	if _, ok, err := nodes[1].LookupResult(command.OperationID); err != nil || !ok {
		t.Fatalf("follower retry lookup = %v, %v", ok, err)
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
