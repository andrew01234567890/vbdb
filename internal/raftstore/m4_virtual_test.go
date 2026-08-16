package raftstore

import (
	"errors"
	"testing"

	"github.com/andrew01234567890/vbdb/internal/storage"
)

func TestM4VirtualDropReorderAndQuorumFaults(t *testing.T) {
	harness, err := NewSplitHarness(transferDescriptor(), []storage.Row{transferRow(t, "users", "a", `{"v":1}`, 1)})
	if err != nil {
		t.Fatal(err)
	}
	harness.SetSecondaryAvailable(2, false)
	harness.SetSecondaryAvailable(3, false)
	if err := harness.BeginSplit([]byte("m"), "left", "right", false); !errors.Is(err, ErrSplitUnsafeCopy) {
		t.Fatalf("quorum-loss begin returned %v", err)
	}
	if err := harness.BeginSplit([]byte("m"), "left", "right", true); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.Write(transferCommand(t, "b", `{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := harness.CatchUp(); !errors.Is(err, ErrSplitBarrier) {
		t.Fatalf("dropped delta returned %v", err)
	}
	row, err := harness.Read("users", []byte("b"))
	if err != nil || row.Sequence != 2 {
		t.Fatalf("source did not retain write during dropped transfer: %#v %v", row, err)
	}
}

func TestM4VirtualConflictingDuplicateAndPeakBounds(t *testing.T) {
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
	delta := harness.deltas[0]
	if err := harness.DeliverDelta(delta); err != nil {
		t.Fatal(err)
	}
	conflict := cloneSplitDelta(delta)
	conflict.Result.Row.Value = []byte(`{"v":9}`)
	if err := harness.DeliverDelta(conflict); !errors.Is(err, ErrSplitChecksum) {
		t.Fatalf("conflicting duplicate returned %v", err)
	}
	if _, err := splitCatchUpScratchMemory(make([]SplitDelta, maxSplitDeltas+1)); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("delta bound returned %v", err)
	}
}
