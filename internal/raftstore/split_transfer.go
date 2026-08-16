package raftstore

import (
	"bytes"
	"sort"
	"sync"

	"github.com/andrew01234567890/vbdb/internal/storage"
	"github.com/andrew01234567890/vbdb/pkg/uuidv7"
	pb "go.etcd.io/raft/v3/raftpb"
)

// SplitHarness is a deterministic transfer harness. It models the source
// barrier, secondary selection, bounded suffix retention, and projected child
// catch-up; the source remains the only serving authority on this branch.
type SplitHarness struct {
	mu             sync.Mutex
	catalog        *RangeCatalog
	source         RangeDescriptor
	sourceRows     map[string]storage.Row
	sourceSequence uint64
	sourceEpoch    uint64
	leader         uint64
	leaderTerm     uint64
	replicas       map[uint64]*SplitReplica
	deltas         []SplitDelta
	deltaBytes     int
	operation      *splitOperation
	operations     map[uuidv7.UUID]splitOperationRecord
	cutover        bool
}

func NewSplitHarness(source RangeDescriptor, rows []storage.Row) (*SplitHarness, error) {
	if source.Phase != RangeServing {
		return nil, ErrRangeNotServing
	}
	if err := source.Validate(); err != nil {
		return nil, err
	}
	ordered := cloneSplitRows(rows)
	sort.Slice(ordered, func(i, j int) bool { return splitRowLess(ordered[i], ordered[j]) })
	if err := validateSplitRows(ordered, source, ^uint64(0)); err != nil {
		return nil, err
	}
	catalog, err := NewRangeCatalog(source.Generation, []RangeDescriptor{source})
	if err != nil {
		return nil, err
	}
	harness := &SplitHarness{catalog: catalog, source: source.Clone(), sourceRows: make(map[string]storage.Row, len(ordered)), sourceEpoch: source.OwnerEpoch, leader: source.Voters[0], leaderTerm: 1, replicas: make(map[uint64]*SplitReplica), operations: make(map[uuidv7.UUID]splitOperationRecord)}
	for _, row := range ordered {
		harness.sourceRows[splitRowID(row.Table, row.Key)] = cloneTransferRow(row)
		if row.Sequence > harness.sourceSequence {
			harness.sourceSequence = row.Sequence
		}
	}
	for _, id := range source.Voters[1:] {
		replica := &SplitReplica{ID: id, RangeID: source.RangeID, GroupID: source.GroupID, Generation: source.Generation, OwnerEpoch: source.OwnerEpoch, Voters: append([]uint64(nil), source.Voters...), ConfigFingerprint: splitConfigFingerprint(source), Term: harness.leaderTerm, Applied: harness.sourceSequence, Available: true, Rows: make(map[string]storage.Row, len(harness.sourceRows))}
		for key, row := range harness.sourceRows {
			replica.Rows[key] = cloneTransferRow(row)
		}
		harness.replicas[id] = replica
	}
	return harness, nil
}

func (h *SplitHarness) Catalog() *RangeCatalog {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.catalog == nil {
		return nil
	}
	encoded, err := h.catalog.MarshalBinary()
	if err != nil {
		return nil
	}
	catalog, _ := UnmarshalRangeCatalog(encoded)
	return catalog
}

func (h *SplitHarness) SourceDescriptor() RangeDescriptor {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.source.Clone()
}

func (h *SplitHarness) RegisterSecondary(replica SplitReplica) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if replica.ID == 0 || replica.ID == h.leader || !containsSplitVoter(h.source.Voters, replica.ID) {
		return ErrSplitUnsafeCopy
	}
	if replica.RangeID != h.source.RangeID || replica.GroupID != h.source.GroupID || replica.Generation != h.source.Generation || replica.OwnerEpoch != h.source.OwnerEpoch || !equalUint64(replica.Voters, h.source.Voters) || replica.ConfigFingerprint != splitConfigFingerprint(h.source) || replica.Term != h.leaderTerm {
		return ErrSplitGeneration
	}
	if replica.Applied > h.sourceSequence {
		return ErrSplitGeneration
	}
	if err := validateSplitRowMap(replica.Rows, h.source, replica.Applied); err != nil {
		return err
	}
	clone := &replica
	clone.Voters = append([]uint64(nil), replica.Voters...)
	clone.Rows = cloneTransferMap(replica.Rows)
	h.replicas[replica.ID] = clone
	return nil
}

func (h *SplitHarness) SetLeader(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if containsSplitVoter(h.source.Voters, id) && id != h.leader {
		h.leader = id
		h.leaderTerm++
		for _, replica := range h.replicas {
			replica.Term = h.leaderTerm
		}
	}
}

func (h *SplitHarness) SetSecondaryAvailable(id uint64, available bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if replica := h.replicas[id]; replica != nil {
		replica.Available = available
	}
}

// BeginSplit captures the durable source barrier and creates non-serving child
// projections. The secondary must be a matching, healthy non-leader unless the
// explicit deterministic fallback is requested.
func (h *SplitHarness) BeginSplit(boundary []byte, leftID, rightID string, allowLeaderFallback bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.operation != nil {
		return ErrSplitActive
	}
	if len(boundary) == 0 || !h.source.Contains(boundary) || bytes.Equal(boundary, h.source.Start) || (h.source.End != nil && bytes.Equal(boundary, h.source.End)) || leftID == "" || rightID == "" || leftID == rightID || leftID == h.source.RangeID || rightID == h.source.RangeID {
		return ErrRangeInvalid
	}
	barrier := h.sourceSequence
	secondaryFound := false
	for id, replica := range h.replicas {
		if id == h.leader || !replica.Available || replica.Term != h.leaderTerm || replica.Applied < barrier || replica.RangeID != h.source.RangeID || replica.GroupID != h.source.GroupID || replica.Generation != h.source.Generation || replica.OwnerEpoch != h.source.OwnerEpoch || !equalUint64(replica.Voters, h.source.Voters) {
			continue
		}
		secondaryFound = true
		break
	}
	if !secondaryFound && !allowLeaderFallback {
		return ErrSplitUnsafeCopy
	}
	rows := make([]storage.Row, 0, len(h.sourceRows))
	for _, row := range h.sourceRows {
		rows = append(rows, cloneTransferRow(row))
	}
	sort.Slice(rows, func(i, j int) bool { return splitRowLess(rows[i], rows[j]) })
	snapshot, err := BuildSplitSnapshot(h.source, barrier, h.sourceEpoch, rows)
	if err != nil {
		return err
	}
	leftGroup, rightGroup, ok := childGroups(h.source.GroupID)
	if !ok {
		return ErrSplitGeneration
	}
	left := RangeDescriptor{RangeID: leftID, Start: append([]byte(nil), h.source.Start...), End: append([]byte(nil), boundary...), Generation: h.source.Generation + 1, OwnerEpoch: h.source.OwnerEpoch + 1, GroupID: leftGroup, Voters: append([]uint64(nil), h.source.Voters...), Phase: RangeCatchingUp}
	right := RangeDescriptor{RangeID: rightID, Start: append([]byte(nil), boundary...), End: append([]byte(nil), h.source.End...), Generation: h.source.Generation + 1, OwnerEpoch: h.source.OwnerEpoch + 1, GroupID: rightGroup, Voters: append([]uint64(nil), h.source.Voters...), Phase: RangeCatchingUp}
	if left.Generation == 0 || left.OwnerEpoch == 0 {
		return ErrSplitGeneration
	}
	active := &splitOperation{source: h.source.Clone(), left: left, right: right, barrier: barrier, snapshot: snapshot, leftRows: make(map[string]storage.Row), rightRows: make(map[string]storage.Row), deltas: make(map[uint64]SplitDelta), deltaDigests: make(map[uint64][32]byte), operationRecords: make(map[uuidv7.UUID]splitOperationRecord)}
	for key, row := range h.sourceRows {
		if left.Contains([]byte(row.Key)) {
			active.leftRows[key] = cloneTransferRow(row)
		} else if right.Contains([]byte(row.Key)) {
			active.rightRows[key] = cloneTransferRow(row)
		}
	}
	active.leftApplied, active.rightApplied = barrier, barrier
	h.operation = active
	return nil
}

// Write keeps the source authoritative and retains the canonical delta after
// the barrier. This deliberately models only the unconditional/conditional M3
// state semantics needed by the transfer proof.
func (h *SplitHarness) Write(command Command) (Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := validateCommand(command); err != nil {
		return Result{}, err
	}
	if stored, exists := h.operations[command.OperationID]; exists {
		return cloneResult(stored.Result), nil
	}
	if h.cutover {
		var target map[string]storage.Row
		if h.operation.left.Contains([]byte(command.Key)) {
			target = h.operation.leftRows
		} else if h.operation.right.Contains([]byte(command.Key)) {
			target = h.operation.rightRows
		} else {
			return Result{}, ErrRangeGap
		}
		if h.operation.left.Contains([]byte(command.Key)) || h.operation.right.Contains([]byte(command.Key)) {
			result := evaluateTransferCommand(target, command, h.sourceSequence+1)
			h.sourceSequence++
			if result.Succeeded() {
				target[splitRowID(result.Row.Table, result.Row.Key)] = cloneTransferRow(result.Row)
			}
			delta := splitDeltaFor(h.operation.source, h.sourceEpoch, h.sourceSequence, command, result)
			digest, err := splitDeltaDigest(delta)
			if err != nil {
				return Result{}, ErrSplitChecksum
			}
			h.operations[command.OperationID] = splitOperationRecord{Sequence: delta.Sequence, Digest: digest, Result: cloneResult(result)}
			return cloneResult(result), nil
		}
	}
	if h.operation != nil && (len(h.deltas) >= maxSplitDeltas || h.deltaBytes+len(command.Value) > maxSplitDeltaBytesTotal) {
		return Result{}, ErrBackpressure
	}
	if h.sourceSequence == ^uint64(0) {
		return Result{}, ErrBackpressure
	}
	result := evaluateTransferCommand(h.sourceRows, command, h.sourceSequence+1)
	h.sourceSequence++
	delta := splitDeltaFor(h.source, h.sourceEpoch, h.sourceSequence, command, result)
	digest, err := splitDeltaDigest(delta)
	if err != nil {
		return Result{}, ErrSplitChecksum
	}
	h.operations[command.OperationID] = splitOperationRecord{Sequence: delta.Sequence, Digest: digest, Result: cloneResult(result)}
	if result.Succeeded() {
		h.sourceRows[splitRowID(result.Row.Table, result.Row.Key)] = cloneTransferRow(result.Row)
	}
	if h.operation != nil {
		h.deltas = append(h.deltas, cloneSplitDelta(delta))
		h.deltaBytes += len(command.Value)
	}
	for _, replica := range h.replicas {
		if replica.Available && result.Succeeded() {
			replica.Rows[splitRowID(result.Row.Table, result.Row.Key)] = cloneTransferRow(result.Row)
			replica.Applied = h.sourceSequence
		}
	}
	return cloneResult(result), nil
}

func (h *SplitHarness) WriteAtLeader(command Command, leaderID, term, ownerEpoch uint64) (Result, error) {
	h.mu.Lock()
	valid := leaderID == h.leader && term == h.leaderTerm && ownerEpoch == h.sourceEpoch
	cutover := h.cutover
	h.mu.Unlock()
	if !valid || cutover {
		return Result{}, ErrSplitGeneration
	}
	return h.Write(command)
}

func (h *SplitHarness) WriteAtOwner(command Command, rangeID string, generation, ownerEpoch uint64, groupID ...uint64) (Result, error) {
	h.mu.Lock()
	group := h.source.GroupID
	sourceID := h.source.RangeID
	leftID, rightID := "", ""
	if h.operation != nil {
		leftID, rightID = h.operation.left.RangeID, h.operation.right.RangeID
	}
	if len(groupID) == 1 {
		group = groupID[0]
	}
	descriptor, err := h.catalog.RouteAt([]byte(command.Key), rangeID, generation, ownerEpoch, group)
	h.mu.Unlock()
	if err != nil {
		return Result{}, err
	}
	if descriptor.RangeID != sourceID && descriptor.RangeID != leftID && descriptor.RangeID != rightID {
		return Result{}, ErrRangeMoved
	}
	return h.Write(command)
}

func (h *SplitHarness) DeliverDelta(delta SplitDelta) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.operation == nil {
		return ErrSplitNotActive
	}
	if delta.Sequence <= h.operation.barrier || delta.Sequence > h.sourceSequence {
		return ErrSplitDeltaOrder
	}
	if err := validateSplitDelta(h.operation, delta); err != nil {
		return err
	}
	digest, err := splitDeltaDigest(delta)
	if err != nil {
		return ErrSplitChecksum
	}
	if previous, exists := h.operation.deltaDigests[delta.Sequence]; exists {
		if previous != digest {
			return ErrSplitChecksum
		}
		return nil
	}
	if len(h.operation.deltas) >= maxSplitDeltas || h.deltaBytes > maxSplitDeltaBytesTotal {
		return ErrBackpressure
	}
	h.operation.deltas[delta.Sequence] = cloneSplitDelta(delta)
	h.operation.deltaDigests[delta.Sequence] = digest
	return nil
}

// CatchUp validates a copied suffix into projected child images and swaps both
// images only after the entire contiguous sequence succeeds.
func (h *SplitHarness) CatchUp() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.operation == nil {
		return ErrSplitNotActive
	}
	ordered := make([]SplitDelta, 0, len(h.operation.deltas))
	for _, delta := range h.operation.deltas {
		ordered = append(ordered, delta)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Sequence < ordered[j].Sequence })
	if _, err := splitCatchUpScratchMemory(ordered); err != nil {
		return err
	}
	left := cloneTransferMap(h.operation.leftRows)
	right := cloneTransferMap(h.operation.rightRows)
	leftApplied, rightApplied := h.operation.leftApplied, h.operation.rightApplied
	for _, delta := range ordered {
		if err := validateSplitDelta(h.operation, delta); err != nil {
			return err
		}
		digest, err := splitDeltaDigest(delta)
		if err != nil || h.operation.deltaDigests[delta.Sequence] != digest {
			return ErrSplitChecksum
		}
		if delta.Sequence != leftApplied+1 && delta.Sequence != rightApplied+1 {
			return ErrSplitDeltaOrder
		}
		if delta.Sequence != leftApplied+1 || delta.Sequence != rightApplied+1 {
			return ErrSplitDeltaOrder
		}
		if err := applyTransferDelta(left, right, &leftApplied, &rightApplied, delta, h.operation); err != nil {
			return err
		}
	}
	h.operation.leftRows, h.operation.rightRows = left, right
	h.operation.leftApplied, h.operation.rightApplied = leftApplied, rightApplied
	if leftApplied != h.sourceSequence || rightApplied != h.sourceSequence {
		return ErrSplitBarrier
	}
	return nil
}

func (h *SplitHarness) Snapshot() (SplitSnapshot, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.operation == nil {
		return SplitSnapshot{}, ErrSplitNotActive
	}
	return h.operation.snapshot.Clone(), nil
}

func (h *SplitHarness) Read(table string, key []byte) (storage.Row, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rows := h.sourceRows
	if h.cutover && h.operation != nil {
		if h.operation.left.Contains(key) {
			rows = h.operation.leftRows
		} else if h.operation.right.Contains(key) {
			rows = h.operation.rightRows
		}
	}
	row, ok := rows[splitRowID(table, string(key))]
	if !ok {
		return storage.Row{}, storage.ErrNotFound
	}
	return cloneTransferRow(row), nil
}

func (h *SplitHarness) TargetProof() (SplitTargetProof, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.operation == nil {
		return SplitTargetProof{}, ErrSplitNotActive
	}
	if h.operation.leftApplied != h.sourceSequence || h.operation.rightApplied != h.sourceSequence {
		return SplitTargetProof{}, ErrSplitBarrier
	}
	proof := SplitTargetProof{Source: h.operation.source.Clone(), Left: h.operation.left.Clone(), Right: h.operation.right.Clone(), Barrier: h.operation.barrier, FinalSequence: h.sourceSequence, LeftHash: splitRowsDigest(h.operation.leftRows), RightHash: splitRowsDigest(h.operation.rightRows), Quorum: true, ConfState: &pb.ConfState{Voters: append([]uint64(nil), h.source.Voters...)}}
	proof.SnapshotHash = [32]byte{}
	return proof, proof.Validate()
}

func validateSplitDelta(operation *splitOperation, delta SplitDelta) error {
	source := operation.source
	if delta.SourceRangeID != source.RangeID || !bytes.Equal(delta.SourceStart, source.Start) || delta.SourceEndIsInfinity != (source.End == nil) || (!delta.SourceEndIsInfinity && !bytes.Equal(delta.SourceEnd, source.End)) || delta.SourceGeneration != source.Generation || delta.SourceEpoch != source.OwnerEpoch || delta.SourceOwnerEpoch != source.OwnerEpoch || delta.SourceGroupID != source.GroupID || !equalUint64(delta.SourceVoters, source.Voters) || delta.SourceConfigFingerprint != splitConfigFingerprint(source) || delta.SourcePhase != source.Phase {
		return ErrSplitGeneration
	}
	if err := validateCommand(delta.Command); err != nil {
		return ErrSplitChecksum
	}
	commandA, errA := EncodeCommand(delta.Command)
	commandB, errB := EncodeCommand(delta.Result.Command)
	if errA != nil || errB != nil || !bytes.Equal(commandA, commandB) {
		return ErrSplitChecksum
	}
	if _, err := encodeResult(delta.Result); err != nil {
		return ErrSplitChecksum
	}
	if !source.Contains([]byte(delta.Command.Key)) || (!operation.left.Contains([]byte(delta.Command.Key)) && !operation.right.Contains([]byte(delta.Command.Key))) {
		return ErrRangeGap
	}
	if delta.Result.Succeeded() && (delta.Result.Row.Sequence != delta.Sequence || delta.Result.Row.Table != delta.Command.Table || delta.Result.Row.Key != delta.Command.Key || delta.Result.Row.Version != delta.Command.Version || !bytes.Equal(delta.Result.Row.Value, delta.Command.Value)) {
		return ErrSplitChecksum
	}
	return nil
}

func applyTransferDelta(left, right map[string]storage.Row, leftApplied, rightApplied *uint64, delta SplitDelta, operation *splitOperation) error {
	if delta.Result.VersionConflict() || delta.Result.PreconditionFailed() {
		*leftApplied, *rightApplied = delta.Sequence, delta.Sequence
		return nil
	}
	var target map[string]storage.Row
	if operation.left.Contains([]byte(delta.Command.Key)) {
		target = left
	} else {
		target = right
	}
	if delta.Result.Succeeded() {
		target[splitRowID(delta.Result.Row.Table, delta.Result.Row.Key)] = cloneTransferRow(delta.Result.Row)
	}
	*leftApplied, *rightApplied = delta.Sequence, delta.Sequence
	return nil
}

func evaluateTransferCommand(rows map[string]storage.Row, command Command, sequence uint64) Result {
	result := Result{Command: cloneCommand(command), Status: resultSuccess}
	current, exists := rows[splitRowID(command.Table, command.Key)]
	if command.Condition.CreateOnly && exists {
		result.Status = resultPrecondition
		return result
	}
	if command.Condition.IfMatch != nil && (!exists || current.Version != *command.Condition.IfMatch) {
		result.Status = resultPrecondition
		return result
	}
	result.Created = !exists
	result.Row = storage.Row{Table: command.Table, Key: command.Key, Version: command.Version, Sequence: sequence, Value: append([]byte(nil), command.Value...)}
	return result
}

func childGroups(group uint64) (uint64, uint64, bool) {
	if group == 0 || group > ^uint64(0)/2-1 {
		return 0, 0, false
	}
	return group * 2, group*2 + 1, true
}

func containsSplitVoter(voters []uint64, id uint64) bool {
	for _, voter := range voters {
		if voter == id {
			return true
		}
	}
	return false
}

func validateSplitRowMap(rows map[string]storage.Row, source RangeDescriptor, barrier uint64) error {
	ordered := make([]storage.Row, 0, len(rows))
	for key, row := range rows {
		if key != splitRowID(row.Table, row.Key) {
			return ErrSplitChecksum
		}
		ordered = append(ordered, row)
	}
	sort.Slice(ordered, func(i, j int) bool { return splitRowLess(ordered[i], ordered[j]) })
	return validateSplitRows(ordered, source, barrier)
}

func cloneTransferMap(rows map[string]storage.Row) map[string]storage.Row {
	clone := make(map[string]storage.Row, len(rows))
	for key, row := range rows {
		clone[key] = cloneTransferRow(row)
	}
	return clone
}

func cloneTransferRow(row storage.Row) storage.Row {
	row.Value = append([]byte(nil), row.Value...)
	return row
}
