package raftstore

import (
	"errors"
	"testing"

	"github.com/andrew01234567890/vbdb/internal/storage"
)

func TestSplitCutoverPublishesChildrenAndRetiresSourceOnce(t *testing.T) {
	source := transferDescriptor()
	harness, err := NewSplitHarness(source, []storage.Row{transferRow(t, "users", "a", `{"v":1}`, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.BeginSplit([]byte("m"), "left", "right", false); err != nil {
		t.Fatal(err)
	}
	if err := harness.Cutover(); err != nil {
		t.Fatal(err)
	}
	if err := harness.Cutover(); err != nil {
		t.Fatalf("idempotent cutover failed: %v", err)
	}
	catalog := harness.Catalog()
	if catalog == nil || len(catalog.Descriptors()) != 2 {
		t.Fatalf("cutover published unexpected catalog: %#v", catalog)
	}
	retired, ok := catalog.DescriptorByID(source.RangeID)
	if !ok || retired.Phase != RangeRetired {
		t.Fatalf("source tombstone missing: %#v %v", retired, ok)
	}
	if _, err := catalog.RouteAt([]byte("a"), source.RangeID, source.Generation, source.OwnerEpoch, source.GroupID); !errors.Is(err, ErrRangeMoved) {
		t.Fatalf("stale source route returned %v", err)
	}
	command := transferCommand(t, "a", `{"v":2}`)
	if _, err := harness.WriteAtOwner(command, source.RangeID, source.Generation, source.OwnerEpoch, source.GroupID); !errors.Is(err, ErrRangeMoved) {
		t.Fatalf("stale owner write returned %v", err)
	}
	row, err := harness.Read("users", []byte("a"))
	if err != nil || row.Sequence != 1 {
		t.Fatalf("child read after cutover failed: %#v %v", row, err)
	}
}

func TestSplitCutoverLeavesSourceServingWhenFinalBarrierFails(t *testing.T) {
	source := transferDescriptor()
	harness, err := NewSplitHarness(source, []storage.Row{transferRow(t, "users", "a", `{"v":1}`, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.BeginSplit([]byte("m"), "left", "right", false); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.Write(transferCommand(t, "b", `{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := harness.Cutover(); !errors.Is(err, ErrSplitBarrier) {
		t.Fatalf("unprepared cutover returned %v", err)
	}
	if harness.SourceDescriptor().Phase != RangeServing || len(harness.Catalog().Descriptors()) != 1 {
		t.Fatal("failed cutover exposed a partial child catalog")
	}
}
