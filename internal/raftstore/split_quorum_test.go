package raftstore

import (
	"errors"
	"testing"

	"github.com/andrew01234567890/vbdb/internal/storage"
	pb "go.etcd.io/raft/v3/raftpb"
)

func TestSplitCatchUpLateCorruptionLeavesProjectedChildrenUnchanged(t *testing.T) {
	harness, err := NewSplitHarness(transferDescriptor(), []storage.Row{transferRow(t, "users", "a", `{"v":1}`, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.BeginSplit([]byte("m"), "left", "right", false); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.Write(transferCommand(t, "b", `{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.Write(transferCommand(t, "z", `{"v":3}`)); err != nil {
		t.Fatal(err)
	}
	for _, delta := range harness.deltas {
		if err := harness.DeliverDelta(delta); err != nil {
			t.Fatal(err)
		}
	}
	beforeLeft := cloneTransferMap(harness.operation.leftRows)
	beforeRight := cloneTransferMap(harness.operation.rightRows)
	corrupt := harness.operation.deltas[3]
	corrupt.Result.Row.Value = []byte(`{"v":99}`)
	harness.operation.deltas[3] = corrupt
	if err := harness.CatchUp(); !errors.Is(err, ErrSplitChecksum) {
		t.Fatalf("late corruption returned %v", err)
	}
	if len(harness.operation.leftRows) != len(beforeLeft) || len(harness.operation.rightRows) != len(beforeRight) {
		t.Fatal("late catch-up failure partially swapped child images")
	}
	for key, row := range beforeLeft {
		if current := harness.operation.leftRows[key]; current.Sequence != row.Sequence {
			t.Fatal("left child changed after rejected projection")
		}
	}
	for key, row := range beforeRight {
		if current := harness.operation.rightRows[key]; current.Sequence != row.Sequence {
			t.Fatal("right child changed after rejected projection")
		}
	}
}

func TestSplitTargetProofRequiresExactQuorumMembership(t *testing.T) {
	harness, err := NewSplitHarness(transferDescriptor(), []storage.Row{transferRow(t, "users", "a", `{"v":1}`, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.BeginSplit([]byte("m"), "left", "right", false); err != nil {
		t.Fatal(err)
	}
	proof, err := harness.TargetProof()
	if err != nil {
		t.Fatal(err)
	}
	proof.ConfState = &pb.ConfState{Voters: []uint64{1, 2, 4}}
	if err := proof.Validate(); !errors.Is(err, ErrSplitQuorum) {
		t.Fatalf("foreign ConfState returned %v", err)
	}
}
