package raftstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"sync"

	"github.com/andrew01234567890/vbdb/internal/storage"
	"github.com/andrew01234567890/vbdb/pkg/uuidv7"
	raft "go.etcd.io/raft/v3"
	pb "go.etcd.io/raft/v3/raftpb"
	"google.golang.org/protobuf/proto"
)

var (
	ErrNotLeader = errors.New("raftstore: not leader")
	ErrAmbiguous = errors.New("raftstore: proposal outcome is ambiguous")
	ErrStopped   = errors.New("raftstore: replica stopped")
)

// Transport is deliberately injectable. This milestone does not claim a
// production RPC protocol; tests provide a deterministic in-process link.
// Send must honor ctx cancellation: Replica.Close cancels this context before
// waiting for the Ready loop, so a transport cannot keep shutdown blocked.
type Transport interface {
	Send(context.Context, uint64, *pb.Message) error
}

// SnapshotTransport is the bounded durable-ack extension used by the
// in-process harness. SendSnapshot must return only after the receiver has
// durably installed and applied the snapshot. A transport that cannot provide
// this boundary is deliberately reported as SnapshotFailure rather than being
// treated as a successful enqueue.
type SnapshotTransport interface {
	SendSnapshot(context.Context, uint64, *pb.Message) error
}

type TransportFunc func(context.Context, uint64, *pb.Message) error

func (f TransportFunc) Send(ctx context.Context, to uint64, message *pb.Message) error {
	return f(ctx, to, message)
}

type Options struct {
	ID            uint64
	Dir           string
	Peers         []raft.Peer
	Transport     Transport
	SyncFailure   func(string) error
	ElectionTick  int
	HeartbeatTick int
	// readyHook is an internal test-harness completion signal. It runs only
	// after a Ready has been persisted, applied, sent, and advanced; it does
	// not participate in Raft state transitions.
	readyHook func()
}

type Outcome uint8

const (
	OutcomeSuccess Outcome = iota + 1
	OutcomePreconditionFailed
	OutcomeVersionConflict
	OutcomeNotLeader
	OutcomeAmbiguous
)

type ProposalResult struct {
	Outcome Outcome
	Applied Result
}

type Diagnostic struct {
	ID, Term, Leader, Commit, Applied uint64
	State                             raft.StateType
	Fatal                             error
}

type Replica struct {
	disk      *diskStore
	state     *logicalState
	node      raft.Node
	id        uint64
	transport Transport

	stateMu      sync.RWMutex
	catalogMu    sync.RWMutex
	mu           sync.Mutex
	rangeCatalog *RangeCatalog
	pending      map[uuidv7.UUID]*pendingProposal
	pendingBytes int
	// admissionEpoch fences proposal registration against a SoftState
	// transition. Node.Status must be called without r.mu; this epoch closes
	// the resulting check/register window without holding r.mu across Raft.
	admissionEpoch  uint64
	closed          bool
	fatal           error
	done            chan struct{}
	readyDone       chan struct{}
	stopLoop        chan struct{}
	stopLoopOnce    sync.Once
	nodeStopOnce    sync.Once
	fatalStopOnce   sync.Once
	transportCtx    context.Context
	transportCancel context.CancelFunc
	appliedCh       chan struct{}
	readyHook       func()
}

type proposalDelivery struct {
	result Result
	err    error
}

type pendingProposal struct {
	encoded    []byte
	waiters    map[uint64]chan proposalDelivery
	nextWaiter uint64
	submitted  bool
}

const (
	maxPendingProposals       = 1024
	maxUncommittedEntriesSize = maxReadyBytes
	// Keep client-side retained command bytes within the same finite envelope
	// as Raft's uncommitted-entry budget while a partition prevents results.
	maxPendingProposalBytes = maxUncommittedEntriesSize
)

func Open(options Options) (*Replica, error) {
	if options.ID == 0 {
		return nil, errors.New("raftstore: node id is required")
	}
	if options.Dir == "" {
		return nil, errors.New("raftstore: data directory is required")
	}
	if options.ElectionTick < 0 || options.HeartbeatTick < 0 {
		return nil, errors.New("raftstore: tick values cannot be negative")
	}
	if options.ElectionTick == 0 {
		options.ElectionTick = 10
	}
	if options.HeartbeatTick == 0 {
		options.HeartbeatTick = 1
	}
	if options.ElectionTick <= options.HeartbeatTick {
		return nil, errors.New("raftstore: election tick must be greater than heartbeat tick")
	}
	if len(options.Peers) != 0 {
		peers, err := canonicalPeers(options.ID, options.Peers)
		if err != nil {
			return nil, err
		}
		options.Peers = peers
		if len(options.Peers) > 1 && options.Transport == nil {
			return nil, errors.New("raftstore: multi-peer bootstrap requires transport")
		}
	}
	disk, err := openDisk(filepath.Clean(options.Dir), options.ID, options.SyncFailure)
	if err != nil {
		return nil, err
	}
	state := newLogicalState()
	if err := state.load(disk); err != nil {
		_ = disk.close()
		return nil, err
	}
	// A Ready snapshot is persisted before the logical state batch. If the
	// process dies in that interval, the durable snapshot is intentionally
	// ahead of the old state metadata. Install it before constructing Raft so
	// restart cannot serve the stale state or participate with a mismatched
	// applied index.
	snapshot, err := disk.Snapshot()
	if err != nil {
		_ = disk.close()
		return nil, err
	}
	if snapshot.GetMetadata().GetIndex() > state.lastApplied {
		if err := state.installSnapshot(disk, snapshot); err != nil {
			_ = disk.close()
			return nil, err
		}
	}
	last, err := disk.LastIndex()
	if err != nil {
		_ = disk.close()
		return nil, err
	}
	fresh := !disk.hadNodeID
	incompleteBootstrap := disk.bootstrapIncomplete
	if (fresh || incompleteBootstrap) && len(options.Peers) == 0 {
		_ = disk.close()
		return nil, errors.New("raftstore: initial replica requires peers")
	}
	if !fresh && !incompleteBootstrap && len(options.Peers) != 0 {
		_ = disk.close()
		return nil, errors.New("raftstore: bootstrap peers are only valid on a new store")
	}
	if fresh || incompleteBootstrap {
		if err := validatePeers(options.ID, options.Peers); err != nil {
			_ = disk.close()
			return nil, err
		}
		if len(disk.bootstrapPeers) != 0 && !sameBootstrapPeers(disk.bootstrapPeers, options.Peers) {
			_ = disk.close()
			return nil, errors.New("raftstore: bootstrap peers do not match identity-only store")
		}
		if len(options.Peers) > 1 && options.Transport == nil {
			_ = disk.close()
			return nil, errors.New("raftstore: multi-peer bootstrap requires transport")
		}
	} else if len(disk.confState.GetVoters())+len(disk.confState.GetVotersOutgoing())+len(disk.confState.GetLearners())+len(disk.confState.GetLearnersNext()) > 1 && options.Transport == nil {
		_ = disk.close()
		return nil, errors.New("raftstore: multi-peer restart requires transport")
	}
	if fresh {
		if err := disk.persistIdentityAndBootstrap(options.Peers); err != nil {
			_ = disk.close()
			return nil, err
		}
	}
	config := &raft.Config{ID: options.ID, ElectionTick: options.ElectionTick, HeartbeatTick: options.HeartbeatTick, Storage: disk, Applied: disk.applied, MaxSizePerMsg: 1 << 20, MaxInflightMsgs: 128, MaxUncommittedEntriesSize: maxUncommittedEntriesSize, CheckQuorum: true, PreVote: true, StepDownOnRemoval: true, DisableProposalForwarding: true, AsyncStorageWrites: false}
	transportCtx, transportCancel := context.WithCancel(context.Background())
	r := &Replica{disk: disk, state: state, id: options.ID, transport: options.Transport, pending: make(map[uuidv7.UUID]*pendingProposal), done: make(chan struct{}), readyDone: make(chan struct{}), stopLoop: make(chan struct{}), transportCtx: transportCtx, transportCancel: transportCancel, appliedCh: make(chan struct{}), readyHook: options.readyHook}
	if encodedCatalog := disk.catalogCopy(); len(encodedCatalog) != 0 {
		catalog, err := UnmarshalRangeCatalog(encodedCatalog)
		if err != nil {
			_ = disk.close()
			return nil, fmt.Errorf("raftstore: load range catalog: %w", err)
		}
		if err := catalog.ValidateAgainstVoters(disk.confStateCopy().GetVoters()); err != nil {
			_ = disk.close()
			return nil, fmt.Errorf("raftstore: persisted range catalog membership: %w", err)
		}
		r.rangeCatalog = catalog
	}
	if (fresh || incompleteBootstrap) && last == 0 {
		r.node = raft.StartNode(config, options.Peers)
	} else {
		r.node = raft.RestartNode(config)
	}
	go r.readyLoop()
	return r, nil
}

func validatePeers(self uint64, peers []raft.Peer) error {
	_, err := canonicalPeers(self, peers)
	return err
}

func canonicalPeers(self uint64, peers []raft.Peer) ([]raft.Peer, error) {
	if len(peers) == 0 {
		return nil, errors.New("raftstore: at least one bootstrap peer is required")
	}
	seen := make(map[uint64]struct{}, len(peers))
	selfPresent := false
	canonical := make([]raft.Peer, len(peers))
	for index, peer := range peers {
		if peer.ID == 0 {
			return nil, errors.New("raftstore: bootstrap peer id must be non-zero")
		}
		if len(peer.Context) != 0 {
			return nil, errors.New("raftstore: bootstrap peer context is unsupported")
		}
		if _, exists := seen[peer.ID]; exists {
			return nil, fmt.Errorf("raftstore: duplicate bootstrap peer id %d", peer.ID)
		}
		seen[peer.ID] = struct{}{}
		canonical[index] = raft.Peer{ID: peer.ID}
		if peer.ID == self {
			selfPresent = true
		}
	}
	if !selfPresent {
		return nil, errors.New("raftstore: bootstrap peers must include local node")
	}
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].ID < canonical[j].ID })
	return canonical, nil
}

func sameBootstrapPeers(stored []uint64, peers []raft.Peer) bool {
	if len(stored) != len(peers) {
		return false
	}
	ids := make([]uint64, len(peers))
	for index, peer := range peers {
		ids[index] = peer.ID
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return slices.Equal(stored, ids)
}

func (r *Replica) readyLoop() {
	// Close cancels transport first, then waits for this whole loop before
	// stopping raft. A Ready may already have been received when Close wins;
	// stopping raft before ApplyConfChange would make upstream return nil and
	// let state.apply advance past a membership entry without its ConfState.
	defer func() {
		close(r.done)
		close(r.readyDone)
	}()
	ready := r.node.Ready()
	for {
		var rd raft.Ready
		select {
		case <-r.stopLoop:
			return
		case next, ok := <-ready:
			if !ok {
				return
			}
			rd = next
		}
		if err := r.disk.persistReady(rd); err != nil {
			r.fail(err)
			return
		}
		if !raft.IsEmptySnap(rd.Snapshot) {
			r.stateMu.Lock()
			err := r.state.installSnapshot(r.disk, rd.Snapshot)
			r.stateMu.Unlock()
			if err != nil {
				r.fail(err)
				return
			}
			r.signalApplied()
		}
		for _, entry := range rd.CommittedEntries {
			if entry == nil {
				r.fail(fmt.Errorf("%w: nil committed entry", ErrCorrupt))
				return
			}
			var confState *pb.ConfState
			var confStateIndex uint64
			switch entry.GetType() {
			case pb.EntryConfChange:
				var change pb.ConfChange
				if err := proto.Unmarshal(entry.GetData(), &change); err != nil {
					r.fail(err)
					return
				}
				if err := validateCommittedConfChange(&change); err != nil {
					r.fail(fmt.Errorf("%w: committed conf change: %v", ErrCorrupt, err))
					return
				}
				confState = r.node.ApplyConfChange(&change)
				confStateIndex = entry.GetIndex()
			case pb.EntryConfChangeV2:
				var change pb.ConfChangeV2
				if err := proto.Unmarshal(entry.GetData(), &change); err != nil {
					r.fail(err)
					return
				}
				if err := validateCommittedConfChange(&change); err != nil {
					r.fail(fmt.Errorf("%w: committed conf change: %v", ErrCorrupt, err))
					return
				}
				confState = r.node.ApplyConfChange(&change)
				confStateIndex = entry.GetIndex()
			}
			r.stateMu.Lock()
			results, conflicts, err := r.state.apply(r.disk, []*pb.Entry{entry}, confState, confStateIndex)
			r.stateMu.Unlock()
			if err != nil {
				r.fail(err)
				return
			}
			r.notify(results)
			r.notifyErrors(conflicts)
			r.signalApplied()
			// notify removes retained proposals. If its accounting invariant
			// detects corruption, stop this Ready before cloning/sending messages
			// or calling Advance; a fatal replica must not participate further.
			if r.fatalState() != nil {
				return
			}
		}
		if rd.SoftState != nil && rd.SoftState.RaftState != raft.StateLeader {
			r.observeSoftState()
			// A proposal accepted by the old leader but absent from this Ready's
			// committed results has no known outcome. Release every unresolved
			// waiter immediately. A false ambiguity is safe because retry uses the
			// same operation identity and state apply deduplicates it.
			r.finishPending(ErrAmbiguous)
			if r.fatalState() != nil {
				return
			}
		} else if rd.SoftState != nil {
			// Leader changes also fence the unlocked Node.Status admission check.
			// A proposal may only register against the exact SoftState observed
			// before it called Node.Status.
			r.observeSoftState()
		}
		// Ready is read-only until Advance. Clone all messages while the Ready
		// is owned by this loop, persist/apply first, then send durable copies;
		// only after sends return is it safe to Advance and let raft reuse its
		// message buffers.
		messages := make([]*pb.Message, len(rd.Messages))
		for index, message := range rd.Messages {
			messages[index] = proto.Clone(message).(*pb.Message)
		}
		r.send(messages)
		// Advance is deliberately after logical-state Sync and outbound copies.
		r.node.Advance()
		if r.readyHook != nil {
			r.readyHook()
		}
	}
}

func (r *Replica) fatalState() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fatal
}

func (r *Replica) observeSoftState() {
	r.mu.Lock()
	r.admissionEpoch++
	r.mu.Unlock()
}

func (r *Replica) fail(err error) {
	r.mu.Lock()
	if r.fatal == nil {
		r.fatal = err
	}
	r.mu.Unlock()
	r.requestFatalStop()
	// Wake local waiters (including WaitConfState and applied-index waits) as
	// soon as the fatal state is published; they must not wait for a caller's
	// context timeout after the Ready loop has stopped.
	r.signalApplied()
	r.finishPending(err)
}

// requestFatalStop publishes a single asynchronous shutdown for every fatal
// transition. It cannot stop raft while holding Replica.mu: node.Stop may
// rendezvous with the Ready loop, which may itself be reporting the fatal
// condition. The shutdown first cancels transport and waits for an already
// owned Ready to finish, preserving the ApplyConfChange/Close ordering; only
// then does it stop raft and election participation.
func (r *Replica) requestFatalStop() {
	r.fatalStopOnce.Do(func() {
		go func() {
			r.stopTransport()
			if r.readyDone != nil {
				<-r.readyDone
			}
			r.stopNode()
		}()
	})
}

func (r *Replica) notify(results map[uuidv7.UUID]Result) {
	for id, result := range results {
		result = cloneResult(result)
		encoded, encodeErr := EncodeCommand(result.Command)
		r.mu.Lock()
		pending := r.removePendingLocked(id)
		r.mu.Unlock()
		if pending != nil {
			var delivery proposalDelivery
			if encodeErr != nil || !bytes.Equal(pending.encoded, encoded) {
				// A durable result for the same operation ID must always bind to
				// the exact command submitted by this waiter. Treat a mismatch as
				// a deterministic client conflict, never as replica corruption.
				delivery.err = ErrOperationConflict
			} else {
				delivery.result = result
			}
			for _, waiter := range pending.waiters {
				waiter <- delivery
				close(waiter)
			}
		}
	}
}

func (r *Replica) notifyErrors(conflicts map[uuidv7.UUID]error) {
	for id, err := range conflicts {
		r.mu.Lock()
		pending := r.removePendingLocked(id)
		r.mu.Unlock()
		if pending == nil {
			continue
		}
		for _, waiter := range pending.waiters {
			waiter <- proposalDelivery{err: err}
			close(waiter)
		}
	}
}

func (r *Replica) finishPending(err error) {
	r.mu.Lock()
	pending := r.pending
	r.pending = make(map[uuidv7.UUID]*pendingProposal)
	expectedBytes := 0
	for _, proposal := range pending {
		expectedBytes += len(proposal.encoded)
	}
	if expectedBytes != r.pendingBytes {
		r.markPendingAccountingCorruptLocked()
	}
	r.pendingBytes = 0
	r.mu.Unlock()
	for _, proposal := range pending {
		for _, waiter := range proposal.waiters {
			waiter <- proposalDelivery{err: err}
			close(waiter)
		}
	}
}

func (r *Replica) stopNode() {
	if r.node == nil {
		return
	}
	r.nodeStopOnce.Do(func() { r.node.Stop() })
}

func (r *Replica) stopTransport() {
	if r.transportCancel != nil {
		r.transportCancel()
	}
	if r.stopLoop != nil {
		r.stopLoopOnce.Do(func() { close(r.stopLoop) })
	}
	r.mu.Lock()
	if r.appliedCh != nil {
		previous := r.appliedCh
		r.appliedCh = make(chan struct{})
		close(previous)
	}
	r.mu.Unlock()
}

func (r *Replica) removeWaiter(operationID uuidv7.UUID, waiterID uint64, waiter chan proposalDelivery) {
	r.mu.Lock()
	proposal := r.pending[operationID]
	if proposal != nil {
		if current, ok := proposal.waiters[waiterID]; ok && current == waiter {
			delete(proposal.waiters, waiterID)
			// Once Raft accepted the proposal it cannot be canceled. Keep the
			// operation identity pending without waiters until its durable result
			// or a fatal/stop error arrives, preventing a retry from submitting a
			// second log entry. Before submission, removal cancels the owner.
			if len(proposal.waiters) == 0 && !proposal.submitted {
				r.removePendingLocked(operationID)
			}
		}
	}
	r.mu.Unlock()
}

// removePendingLocked removes one operation and releases its retained command
// bytes. Callers must hold r.mu.
func (r *Replica) removePendingLocked(operationID uuidv7.UUID) *pendingProposal {
	proposal := r.pending[operationID]
	if proposal == nil {
		return nil
	}
	delete(r.pending, operationID)
	if len(proposal.encoded) > r.pendingBytes {
		r.markPendingAccountingCorruptLocked()
		r.pendingBytes = 0
	} else if len(proposal.encoded) == r.pendingBytes {
		r.pendingBytes = 0
	} else {
		r.pendingBytes -= len(proposal.encoded)
	}
	return proposal
}

func (r *Replica) markPendingAccountingCorruptLocked() {
	if r.fatal == nil {
		r.fatal = fmt.Errorf("%w: pending proposal byte accounting", ErrCorrupt)
		r.requestFatalStop()
	}
}

func (r *Replica) send(messages []*pb.Message) {
	for _, message := range messages {
		if r.transport == nil {
			continue
		}
		if message.GetType() == pb.MsgSnap {
			snapshotTransport, ok := r.transport.(SnapshotTransport)
			if !ok {
				r.node.ReportSnapshot(message.GetTo(), raft.SnapshotFailure)
				continue
			}
			err := snapshotTransport.SendSnapshot(r.transportCtx, message.GetTo(), message)
			if err != nil {
				r.node.ReportUnreachable(message.GetTo())
				r.node.ReportSnapshot(message.GetTo(), raft.SnapshotFailure)
				continue
			}
			r.node.ReportSnapshot(message.GetTo(), raft.SnapshotFinish)
			continue
		}
		err := r.transport.Send(r.transportCtx, message.GetTo(), proto.Clone(message).(*pb.Message))
		if err != nil {
			r.node.ReportUnreachable(message.GetTo())
			if message.GetType() == pb.MsgSnap {
				r.node.ReportSnapshot(message.GetTo(), raft.SnapshotFailure)
			}
			continue
		}
	}
}

func (r *Replica) signalApplied() {
	r.mu.Lock()
	previous := r.appliedCh
	r.appliedCh = make(chan struct{})
	close(previous)
	r.mu.Unlock()
}

func (r *Replica) waitApplied(ctx context.Context, index uint64) error {
	for {
		r.stateMu.RLock()
		applied := r.state.lastApplied
		r.stateMu.RUnlock()
		if applied >= index {
			return nil
		}
		r.mu.Lock()
		if r.fatal != nil {
			err := r.fatal
			r.mu.Unlock()
			return err
		}
		if r.closed {
			r.mu.Unlock()
			return ErrStopped
		}
		notice := r.appliedCh
		r.mu.Unlock()
		select {
		case <-notice:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (r *Replica) Tick() {
	r.mu.Lock()
	fatal, closed := r.fatal, r.closed
	r.mu.Unlock()
	if fatal == nil && !closed {
		r.node.Tick()
	}
}
func (r *Replica) Campaign(ctx context.Context) error {
	if ctx == nil {
		return errors.New("raftstore: nil campaign context")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrStopped
	}
	if r.fatal != nil {
		err := r.fatal
		r.mu.Unlock()
		return err
	}
	r.mu.Unlock()
	err := r.node.Campaign(ctx)
	if errors.Is(err, raft.ErrStopped) {
		return ErrStopped
	}
	return err
}

func (r *Replica) Step(ctx context.Context, message *pb.Message) error {
	if ctx == nil {
		return errors.New("raftstore: nil step context")
	}
	if message == nil {
		return errors.New("raftstore: nil raft message")
	}
	// Node.Step may retain protobuf slices after returning from this method.
	// Own a deep copy before validation/enqueue so transports and callers can
	// safely reuse or mutate their message after Step returns.
	message = proto.Clone(message).(*pb.Message)
	if err := r.readOpen(); err != nil {
		return err
	}
	if message.GetType() == pb.MsgSnap {
		if err := validateIncomingSnapshot(message.GetSnapshot()); err != nil {
			err = fmt.Errorf("%w: incoming snapshot: %v", ErrCorrupt, err)
			r.fail(err)
			return err
		}
	}
	// Node methods are blocking channel calls. Never hold Replica.mu while
	// invoking one: the Ready loop may be waiting for Advance while fail/Close
	// needs the same lifecycle lock to stop the node and release that wait.
	err := r.node.Step(ctx, message)
	if errors.Is(err, raft.ErrStopped) {
		return ErrStopped
	}
	return err
}

func validateIncomingSnapshot(snapshot *pb.Snapshot) error {
	if snapshot == nil || snapshot.GetMetadata().GetIndex() == 0 || snapshot.GetMetadata().GetTerm() == 0 {
		return errors.New("snapshot metadata is empty")
	}
	if err := validateConfState(snapshot.GetMetadata().GetConfState()); err != nil {
		return err
	}
	decoded, err := decodeSnapshot(snapshot.GetData())
	if err != nil {
		return err
	}
	if decoded.lastApplied != snapshot.GetMetadata().GetIndex() {
		return errors.New("snapshot data index mismatch")
	}
	return nil
}

// SubmitConfChange admits one simple membership change to the local Raft
// proposal queue. A nil error means admission only; it does not mean that the
// change committed or that ConfState is durable. Call WaitConfState to await
// the applied membership state and treat context cancellation/leadership loss
// as an unresolved outcome suitable for retrying the same change.
func (r *Replica) SubmitConfChange(ctx context.Context, change *pb.ConfChangeV2) error {
	if ctx == nil {
		return errors.New("raftstore: nil membership context")
	}
	if change == nil {
		return errors.New("raftstore: exactly one non-nil membership change is required")
	}
	// Clone before inspecting nested fields: validation and the asynchronous
	// proposal must both operate on a stable snapshot of caller-owned protobuf
	// memory. Callers must not mutate the input concurrently with this method;
	// after it returns they may safely reuse or mutate their original value.
	change = proto.Clone(change).(*pb.ConfChangeV2)
	if len(change.Changes) != 1 || change.Changes[0] == nil {
		return errors.New("raftstore: exactly one non-nil membership change is required")
	}
	if change.GetTransition() != pb.ConfChangeTransition_ConfChangeTransitionAuto {
		return errors.New("raftstore: only automatic one-change membership transitions are supported")
	}
	if len(change.GetContext()) != 0 {
		return errors.New("raftstore: membership context is unsupported")
	}
	if change.Changes[0].GetNodeId() == 0 {
		return errors.New("raftstore: membership node id must be non-zero")
	}
	switch change.Changes[0].GetType() {
	case pb.ConfChangeAddNode, pb.ConfChangeAddLearnerNode, pb.ConfChangeRemoveNode:
	default:
		return errors.New("raftstore: unsupported membership change")
	}
	if change.Changes[0].GetType() == pb.ConfChangeAddLearnerNode && r.transport == nil {
		return errors.New("raftstore: adding a learner requires transport")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrStopped
	}
	if r.fatal != nil {
		err := r.fatal
		r.mu.Unlock()
		return err
	}
	r.mu.Unlock()
	status := r.node.Status()
	if status.Lead != r.id {
		return ErrNotLeader
	}
	if change.Changes[0].GetType() == pb.ConfChangeAddNode {
		if _, learner := status.Config.Learners[change.Changes[0].GetNodeId()]; !learner {
			return errors.New("raftstore: only a configured learner may be promoted")
		}
		progress, tracked := status.Progress[change.Changes[0].GetNodeId()]
		if !tracked || progress.Match < status.GetCommit() {
			return errors.New("raftstore: learner is not caught up to the committed index")
		}
	}
	if change.Changes[0].GetType() == pb.ConfChangeAddLearnerNode {
		id := change.Changes[0].GetNodeId()
		for _, voters := range status.Config.Voters {
			if _, voter := voters[id]; voter {
				return errors.New("raftstore: cannot demote an existing voter to learner")
			}
		}
		if _, learner := status.Config.Learners[id]; learner {
			return errors.New("raftstore: learner is already configured")
		}
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrStopped
	}
	if r.fatal != nil {
		err := r.fatal
		r.mu.Unlock()
		return err
	}
	r.mu.Unlock()
	err := r.node.ProposeConfChange(ctx, change)
	if errors.Is(err, raft.ErrStopped) {
		return ErrStopped
	}
	return err
}

// validateCommittedConfChange runs before raft.ApplyConfChange. Raft's joint
// tracker assumes these invariants and is allowed to panic on malformed
// ConfState; untrusted log bytes must therefore fail closed at our boundary.
func validateCommittedConfChange(change pb.ConfChangeI) error {
	var changes []*pb.ConfChangeSingle
	switch value := change.(type) {
	case *pb.ConfChange:
		if len(value.GetContext()) != 0 {
			return errors.New("membership context is unsupported")
		}
		changes = []*pb.ConfChangeSingle{{NodeId: value.NodeId, Type: value.Type}}
	case *pb.ConfChangeV2:
		if value.GetTransition() != pb.ConfChangeTransition_ConfChangeTransitionAuto {
			return errors.New("joint membership transition is outside the M3 contract")
		}
		if len(value.GetContext()) != 0 {
			return errors.New("membership context is unsupported")
		}
		changes = value.Changes
	default:
		return errors.New("unknown membership change type")
	}
	if len(changes) != 1 || changes[0] == nil || changes[0].GetNodeId() == 0 {
		return errors.New("exactly one non-zero membership change is required")
	}
	switch changes[0].GetType() {
	case pb.ConfChangeAddNode, pb.ConfChangeAddLearnerNode, pb.ConfChangeRemoveNode:
		return nil
	default:
		return errors.New("unsupported membership change")
	}
}

func (r *Replica) SubmitAddLearner(ctx context.Context, id uint64) error {
	if id == 0 {
		return errors.New("raftstore: learner node id must be non-zero")
	}
	return r.SubmitConfChange(ctx, &pb.ConfChangeV2{Changes: []*pb.ConfChangeSingle{{NodeId: u64ptr(id), Type: pb.ConfChangeAddLearnerNode.Enum()}}})
}

func (r *Replica) SubmitPromote(ctx context.Context, id uint64) error {
	if id == 0 {
		return errors.New("raftstore: promoted node id must be non-zero")
	}
	return r.SubmitConfChange(ctx, &pb.ConfChangeV2{Changes: []*pb.ConfChangeSingle{{NodeId: u64ptr(id), Type: pb.ConfChangeAddNode.Enum()}}})
}

func (r *Replica) SubmitRemove(ctx context.Context, id uint64) error {
	if id == 0 {
		return errors.New("raftstore: removed node id must be non-zero")
	}
	return r.SubmitConfChange(ctx, &pb.ConfChangeV2{Changes: []*pb.ConfChangeSingle{{NodeId: u64ptr(id), Type: pb.ConfChangeRemoveNode.Enum()}}})
}

// WaitConfState waits until the locally durable applied ConfState satisfies
// condition. It is the commit boundary for SubmitConfChange; Submit* itself
// intentionally reports only local Raft admission. The predicate receives a
// defensive copy and must not retain or mutate it.
func (r *Replica) WaitConfState(ctx context.Context, condition func(*pb.ConfState) bool) error {
	if ctx == nil {
		return errors.New("raftstore: nil membership wait context")
	}
	if condition == nil {
		return errors.New("raftstore: nil membership predicate")
	}
	for {
		if err := r.readOpen(); err != nil {
			return err
		}
		r.stateMu.RLock()
		confState := r.disk.confStateCopy()
		r.stateMu.RUnlock()
		matched := condition(confState)
		if matched {
			return nil
		}
		r.mu.Lock()
		if r.fatal != nil {
			err := r.fatal
			r.mu.Unlock()
			return err
		}
		if r.closed {
			r.mu.Unlock()
			return ErrStopped
		}
		notice := r.appliedCh
		r.mu.Unlock()
		select {
		case <-notice:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (r *Replica) Propose(ctx context.Context, command Command) (ProposalResult, error) {
	if ctx == nil {
		return ProposalResult{}, errors.New("raftstore: nil proposal context")
	}
	// Establish the lifecycle boundary before consulting cached results. A
	// retry after Close/fatal must not be mistaken for a successful local
	// cache hit; the second check below linearizes a cached return against
	// Close without holding stateMu across any Raft call.
	if err := r.readOpen(); err != nil {
		return ProposalResult{}, err
	}
	encoded, err := EncodeCommand(command)
	if err != nil {
		return ProposalResult{}, err
	}
	// State results are protected by stateMu, while lifecycle/pending state is
	// protected by mu. Neither lock may be held across a raft.Node call.
	r.stateMu.RLock()
	existing, exists := r.state.results[command.OperationID]
	if exists {
		existing = cloneResult(existing)
	}
	r.stateMu.RUnlock()
	if exists {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return ProposalResult{}, ErrStopped
		}
		if r.fatal != nil {
			err := r.fatal
			r.mu.Unlock()
			return ProposalResult{}, err
		}
		stored, encodeErr := EncodeCommand(existing.Command)
		if encodeErr != nil {
			r.mu.Unlock()
			return ProposalResult{}, fmt.Errorf("%w: result command: %v", ErrCorrupt, encodeErr)
		}
		if !bytes.Equal(stored, encoded) {
			r.mu.Unlock()
			return ProposalResult{}, ErrOperationConflict
		}
		if existing.PreconditionFailed() {
			result := ProposalResult{Outcome: OutcomePreconditionFailed, Applied: existing}
			r.mu.Unlock()
			return result, nil
		}
		if existing.VersionConflict() {
			result := ProposalResult{Outcome: OutcomeVersionConflict, Applied: existing}
			r.mu.Unlock()
			return result, nil
		}
		result := ProposalResult{Outcome: OutcomeSuccess, Applied: existing}
		r.mu.Unlock()
		return result, nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ProposalResult{}, ErrStopped
	}
	if r.fatal != nil {
		err := r.fatal
		r.mu.Unlock()
		return ProposalResult{}, err
	}
	admissionEpoch := r.admissionEpoch
	r.mu.Unlock()
	status := r.node.Status()
	if status.Lead != r.id {
		return ProposalResult{Outcome: OutcomeNotLeader}, ErrNotLeader
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ProposalResult{}, ErrStopped
	}
	if r.fatal != nil {
		err := r.fatal
		r.mu.Unlock()
		return ProposalResult{}, err
	}
	if admissionEpoch != r.admissionEpoch {
		r.mu.Unlock()
		return ProposalResult{Outcome: OutcomeAmbiguous}, ErrAmbiguous
	}
	if r.pendingBytes < 0 || r.pendingBytes > maxPendingProposalBytes {
		r.markPendingAccountingCorruptLocked()
		err := r.fatal
		r.mu.Unlock()
		return ProposalResult{}, err
	}
	proposal := r.pending[command.OperationID]
	owner := proposal == nil
	if owner {
		if len(r.pending) >= maxPendingProposals || len(encoded) > maxPendingProposalBytes-r.pendingBytes {
			r.mu.Unlock()
			return ProposalResult{}, ErrBackpressure
		}
		proposal = &pendingProposal{encoded: append([]byte(nil), encoded...), waiters: make(map[uint64]chan proposalDelivery)}
		r.pending[command.OperationID] = proposal
		r.pendingBytes += len(encoded)
	} else if !bytes.Equal(proposal.encoded, encoded) {
		r.mu.Unlock()
		return ProposalResult{}, ErrOperationConflict
	}
	proposal.nextWaiter++
	waiterID := proposal.nextWaiter
	waiter := make(chan proposalDelivery, 1)
	proposal.waiters[waiterID] = waiter
	r.mu.Unlock()
	if owner {
		go r.submitProposal(command.OperationID, encoded)
	}
	return r.awaitProposal(ctx, command.OperationID, encoded, waiterID, waiter)
}

func (r *Replica) submitProposal(operationID uuidv7.UUID, encoded []byte) {
	r.mu.Lock()
	proposal := r.pending[operationID]
	if proposal == nil || !bytes.Equal(proposal.encoded, encoded) {
		r.mu.Unlock()
		return
	}
	proposal.submitted = true
	r.mu.Unlock()
	err := r.node.Propose(context.Background(), encoded)
	if err == nil {
		return
	}
	if errors.Is(err, raft.ErrStopped) {
		err = ErrStopped
	} else if errors.Is(err, raft.ErrProposalDropped) {
		// Raft did not admit this entry because its finite uncommitted-entry
		// budget was exceeded. No durable outcome exists; retrying the same
		// operation ID is safe after backpressure subsides.
		err = ErrBackpressure
	} else if r.node.Status().Lead != r.id {
		err = ErrNotLeader
	} else {
		err = ErrAmbiguous
	}
	r.failPending(operationID, encoded, err)
}

func (r *Replica) failPending(operationID uuidv7.UUID, encoded []byte, err error) {
	r.mu.Lock()
	proposal := r.pending[operationID]
	if proposal == nil || !bytes.Equal(proposal.encoded, encoded) {
		r.mu.Unlock()
		return
	}
	proposal = r.removePendingLocked(operationID)
	r.mu.Unlock()
	for _, waiter := range proposal.waiters {
		waiter <- proposalDelivery{err: err}
		close(waiter)
	}
}

func (r *Replica) awaitProposal(ctx context.Context, operationID uuidv7.UUID, encoded []byte, waiterID uint64, waiter chan proposalDelivery) (ProposalResult, error) {
	select {
	case delivery := <-waiter:
		if delivery.err != nil {
			if errors.Is(delivery.err, ErrNotLeader) {
				return ProposalResult{Outcome: OutcomeNotLeader}, ErrNotLeader
			}
			if errors.Is(delivery.err, ErrAmbiguous) {
				return ProposalResult{Outcome: OutcomeAmbiguous}, ErrAmbiguous
			}
			return ProposalResult{}, delivery.err
		}
		stored, err := EncodeCommand(delivery.result.Command)
		if err != nil || !bytes.Equal(stored, encoded) {
			return ProposalResult{}, fmt.Errorf("%w: delivered result command mismatch", ErrCorrupt)
		}
		result := cloneResult(delivery.result)
		if result.PreconditionFailed() {
			return ProposalResult{Outcome: OutcomePreconditionFailed, Applied: result}, nil
		}
		if result.VersionConflict() {
			return ProposalResult{Outcome: OutcomeVersionConflict, Applied: result}, nil
		}
		return ProposalResult{Outcome: OutcomeSuccess, Applied: result}, nil
	case <-ctx.Done():
		r.removeWaiter(operationID, waiterID, waiter)
		return ProposalResult{Outcome: OutcomeAmbiguous}, ErrAmbiguous
	}
}

// GetLocal reads the locally applied state. It is not a linearizable follower
// read and does not perform a leader ReadIndex or freshness fence.
func (r *Replica) GetLocal(table, key string) (storage.Row, error) {
	if err := r.readOpen(); err != nil {
		return storage.Row{}, err
	}
	r.stateMu.RLock()
	defer r.stateMu.RUnlock()
	if err := r.readOpen(); err != nil {
		return storage.Row{}, err
	}
	return r.state.get(table, key)
}

// LookupResult returns the durable result for an operation ID. It is the
// retry primitive after a client receives ErrAmbiguous.
func (r *Replica) LookupResult(operationID uuidv7.UUID) (Result, bool, error) {
	if err := r.readOpen(); err != nil {
		return Result{}, false, err
	}
	r.stateMu.RLock()
	defer r.stateMu.RUnlock()
	if err := r.readOpen(); err != nil {
		return Result{}, false, err
	}
	result, ok := r.state.results[operationID]
	if ok {
		result = cloneResult(result)
	}
	return result, ok, nil
}

// InstallRangeCatalog durably replaces the complete route image and publishes
// its pointer only after the owned engine has synced the canonical bytes. A
// failed or uncertain publication enters the same fatal boundary as other
// durable state failures; no partial route can be served.
func (r *Replica) InstallRangeCatalog(catalog *RangeCatalog) error {
	if catalog == nil {
		return ErrCatalogCorrupt
	}
	if err := r.readOpen(); err != nil {
		return err
	}
	encoded, err := catalog.MarshalBinary()
	if err != nil {
		return err
	}
	validated, err := UnmarshalRangeCatalog(encoded)
	if err != nil {
		return err
	}
	r.catalogMu.Lock()
	defer r.catalogMu.Unlock()
	previous := r.rangeCatalog
	if previous != nil {
		if err := validateCatalogReplacement(previous, validated); err != nil {
			return err
		}
		encoded, err = validated.MarshalBinary()
		if err != nil {
			return err
		}
	}
	if err := validated.ValidateAgainstVoters(r.disk.confStateCopy().GetVoters()); err != nil {
		return err
	}
	if err := r.disk.persistCatalog(encoded); err != nil {
		r.fail(err)
		return err
	}
	r.rangeCatalog = validated
	return nil
}

// RangeCatalog returns a decoded defensive copy of the last durably
// installed image. A nil result means this replica has no routing authority.
func (r *Replica) RangeCatalog() (*RangeCatalog, error) {
	if err := r.readOpen(); err != nil {
		return nil, err
	}
	r.catalogMu.RLock()
	catalog := r.rangeCatalog
	if catalog == nil {
		r.catalogMu.RUnlock()
		return nil, nil
	}
	encoded, err := catalog.MarshalBinary()
	r.catalogMu.RUnlock()
	if err != nil {
		return nil, err
	}
	return UnmarshalRangeCatalog(encoded)
}

// RouteRange checks a complete caller-supplied route fence. Group identity is
// mandatory; stale routes return a typed moved error and never authorize data.
func (r *Replica) RouteRange(key []byte, rangeID string, generation, ownerEpoch, groupID uint64) (RangeDescriptor, error) {
	catalog, err := r.RangeCatalog()
	if err != nil {
		return RangeDescriptor{}, err
	}
	if catalog == nil {
		return RangeDescriptor{}, ErrCatalogCorrupt
	}
	return catalog.RouteAt(append([]byte(nil), key...), rangeID, generation, ownerEpoch, groupID)
}

// Status is a nonblocking diagnostic snapshot. Once Close wins the lifecycle
// race it returns Fatal=ErrStopped (or the earlier fatal error) instead of
// exposing the zero Status that raft.Node returns after its goroutine exits.
func (r *Replica) Status() Diagnostic {
	r.mu.Lock()
	if r.node == nil {
		fatal := r.fatal
		r.mu.Unlock()
		return Diagnostic{ID: r.id, Fatal: fatal}
	}
	id := r.id
	fatal := r.fatal
	if r.closed {
		if fatal == nil {
			fatal = ErrStopped
		}
		r.mu.Unlock()
		return Diagnostic{ID: id, Fatal: fatal}
	}
	node := r.node
	r.mu.Unlock()
	status := node.Status()
	r.mu.Lock()
	if r.closed && fatal == nil {
		fatal = ErrStopped
	}
	if r.fatal != nil {
		fatal = r.fatal
	}
	r.mu.Unlock()
	return Diagnostic{ID: id, Term: status.GetTerm(), Leader: status.Lead, Commit: status.GetCommit(), Applied: status.Applied, State: status.RaftState, Fatal: fatal}
}
func (r *Replica) ExportState() ([]byte, error) {
	if err := r.readOpen(); err != nil {
		return nil, err
	}
	r.stateMu.RLock()
	defer r.stateMu.RUnlock()
	if err := r.readOpen(); err != nil {
		return nil, err
	}
	return r.state.exportSnapshot()
}

// CreateSnapshot is valid only at a fully applied commit index. Snapshot
// chunks and metadata are persisted through the owned engine while the
// uncommitted suffix remains available to Raft.
func (r *Replica) CreateSnapshot() error {
	if err := r.readOpen(); err != nil {
		return err
	}
	r.stateMu.RLock()
	defer r.stateMu.RUnlock()
	if err := r.readOpen(); err != nil {
		return err
	}
	confState := r.disk.confStateCopy()
	data, err := r.state.exportSnapshot()
	index := r.state.lastApplied
	if err != nil {
		return err
	}
	if err := r.readOpen(); err != nil {
		return err
	}
	if index == 0 {
		return errors.New("raftstore: snapshot requires a non-zero applied index")
	}
	commit := r.disk.commitIndex()
	if commit != index {
		return fmt.Errorf("raftstore: snapshot requires applied=commit, applied=%d commit=%d", index, commit)
	}
	term, err := r.disk.Term(index)
	if err != nil {
		return err
	}
	snapshot := &pb.Snapshot{Data: data, Metadata: &pb.SnapshotMetadata{Index: u64ptr(index), Term: u64ptr(term), ConfState: confState}}
	if err := r.disk.persistLocalSnapshot(snapshot); err != nil {
		return err
	}
	return nil
}

func (r *Replica) readOpen() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrStopped
	}
	if r.fatal != nil {
		return r.fatal
	}
	return nil
}
func (r *Replica) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrStopped
	}
	r.closed = true
	r.mu.Unlock()
	r.finishPending(ErrStopped)
	r.stopTransport()
	// Transport cancellation releases a blocking Send/SendSnapshot. Do not
	// stop raft until a Ready already owned by readyLoop has persisted and
	// applied its committed entries; in particular, ApplyConfChange must run
	// while the node is still live and able to return its ConfState.
	if r.readyDone != nil {
		<-r.readyDone
	}
	r.stopNode()
	<-r.done
	// All public state reads and snapshot creation hold stateMu. Waiting for
	// this exclusive lock before closing Pebble prevents a concurrent read
	// that passed its first lifecycle check from touching a closed database.
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.disk.close()
}
