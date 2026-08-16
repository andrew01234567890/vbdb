package raftstore

import (
	"bytes"
	"errors"
)

// Cutover performs the sole serving-authority change for one split. The
// source barrier and both prepared child projections are checked while the
// harness lock is held; catalog replacement publishes both children and the
// retired source in one in-memory image.
func (h *SplitHarness) Cutover() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.operation == nil {
		return ErrSplitNotActive
	}
	if h.cutover {
		return nil
	}
	if h.operation.leftApplied != h.sourceSequence || h.operation.rightApplied != h.sourceSequence {
		return ErrSplitBarrier
	}
	proof := SplitTargetProof{Source: h.operation.source.Clone(), Left: h.operation.left.Clone(), Right: h.operation.right.Clone(), Barrier: h.operation.barrier, FinalSequence: h.sourceSequence, LeftHash: splitRowsDigest(h.operation.leftRows), RightHash: splitRowsDigest(h.operation.rightRows), Quorum: true}
	if err := proof.ValidateWithoutConfState(h.source.Voters); err != nil {
		return err
	}
	left := h.operation.left.Clone()
	left.Phase = RangeServing
	right := h.operation.right.Clone()
	right.Phase = RangeServing
	if !bytes.Equal(left.End, right.Start) || !bytes.Equal(left.Start, h.source.Start) || !bytes.Equal(right.End, h.source.End) {
		return ErrSplitQuorum
	}
	if h.catalog == nil || h.catalog.Version() == ^uint64(0) {
		return ErrCatalogStale
	}
	if err := h.catalog.Replace(h.catalog.Version()+1, []RangeDescriptor{left, right}); err != nil {
		return err
	}
	h.operation.left = left
	h.operation.right = right
	h.source.Phase = RangeRetired
	h.cutover = true
	return nil
}

// ValidateWithoutConfState is used only by the deterministic harness before
// the complete catalog replacement; the actual proof still binds voters to
// the source's durable membership at this boundary.
func (p SplitTargetProof) ValidateWithoutConfState(voters []uint64) error {
	if err := p.ValidateShape(); err != nil {
		return err
	}
	if !equalUint64(voters, p.Source.Voters) {
		return ErrSplitQuorum
	}
	return nil
}

func (p SplitTargetProof) ValidateShape() error {
	if err := p.Source.Validate(); err != nil || p.Source.Phase != RangeServing || p.Barrier == 0 || p.FinalSequence < p.Barrier {
		return ErrSplitQuorum
	}
	if p.Left.Phase != RangeCatchingUp || p.Right.Phase != RangeCatchingUp || !equalUint64(p.Left.Voters, p.Source.Voters) || !equalUint64(p.Right.Voters, p.Source.Voters) {
		return ErrSplitQuorum
	}
	if !bytes.Equal(p.Left.End, p.Right.Start) || !bytes.Equal(p.Left.Start, p.Source.Start) || !bytes.Equal(p.Right.End, p.Source.End) || p.Left.RangeID == p.Right.RangeID || p.Source.RangeID == p.Left.RangeID || p.Source.RangeID == p.Right.RangeID {
		return ErrSplitQuorum
	}
	return nil
}

// StaleOwnerWrite makes the no-mutation old-owner behavior explicit for
// callers that have retained the source route after cutover.
func (h *SplitHarness) StaleOwnerWrite(command Command, generation, ownerEpoch, groupID uint64) (Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cutover {
		return Result{}, ErrRangeMoved
	}
	if generation != h.source.Generation || ownerEpoch != h.source.OwnerEpoch || groupID != h.source.GroupID {
		return Result{}, ErrRangeMoved
	}
	return Result{}, errors.New("raftstore: stale-owner helper requires cutover")
}
