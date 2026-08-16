package raftstore

import (
	"bytes"
	"errors"
	"testing"

	"github.com/andrew01234567890/vbdb/internal/storage"
)

// This suite intentionally uses SplitHarness's deterministic RF3-shaped
// state, not independent child Replicas or production RPC transport.
func TestM4CompleteBoundedSplitHistory(t *testing.T) {
	source := transferDescriptor()
	harness, err := NewSplitHarness(source, []storage.Row{transferRow(t, "users", "a", `{"v":1}`, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.BeginSplit([]byte("m"), "left", "right", false); err != nil {
		t.Fatal(err)
	}
	command := transferCommand(t, "b", `{"v":2}`)
	identity, err := FreezeRetryIdentity(command)
	if err != nil {
		t.Fatal(err)
	}
	result, err := harness.Write(command)
	if err != nil {
		t.Fatal(err)
	}
	if err := RetryResultMatches(identity, result); err != nil {
		t.Fatal(err)
	}
	if err := harness.DeliverDelta(harness.deltas[0]); err != nil {
		t.Fatal(err)
	}
	if err := harness.DeliverDelta(harness.deltas[0]); err != nil {
		t.Fatal(err)
	}
	if err := harness.CatchUp(); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.TargetProof(); err != nil {
		t.Fatal(err)
	}
	if err := harness.Cutover(); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.WriteAtOwner(transferCommand(t, "b", `{"v":4}`), source.RangeID, source.Generation, source.OwnerEpoch, source.GroupID); !errors.Is(err, ErrRangeMoved) {
		t.Fatalf("stale source write returned %v", err)
	}
	row, err := harness.Read("users", []byte("b"))
	if err != nil || !bytes.Equal(row.Value, []byte(`{"v":2}`)) {
		t.Fatalf("post-cutover child row changed: %#v %v", row, err)
	}
}
