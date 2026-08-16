package raftstore

import (
	"bytes"
	"errors"
	"testing"

	"github.com/andrew01234567890/vbdb/internal/storage"
	"github.com/andrew01234567890/vbdb/pkg/uuidv7"
)

func transferCommand(t *testing.T, key, value string) Command {
	t.Helper()
	command, err := NewPut("users", key, []byte(value), storage.Condition{}, uuidv7.New)
	if err != nil {
		t.Fatal(err)
	}
	return command
}

func TestSplitTransferRetainsSourceAndCatchesUpReorderedDeltas(t *testing.T) {
	source := transferDescriptor()
	harness, err := NewSplitHarness(source, []storage.Row{transferRow(t, "users", "a", `{"v":1}`, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.BeginSplit([]byte("m"), "left", "right", false); err != nil {
		t.Fatal(err)
	}
	first, err := harness.Write(transferCommand(t, "b", `{"v":2}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := harness.Write(transferCommand(t, "z", `{"v":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(harness.deltas) != 2 || harness.operation == nil {
		t.Fatalf("source did not retain ordered suffix: deltas=%d", len(harness.deltas))
	}
	if err := harness.DeliverDelta(harness.deltas[1]); err != nil {
		t.Fatal(err)
	}
	if err := harness.DeliverDelta(harness.deltas[0]); err != nil {
		t.Fatal(err)
	}
	if err := harness.CatchUp(); err != nil {
		t.Fatal(err)
	}
	proof, err := harness.TargetProof()
	if err != nil {
		t.Fatal(err)
	}
	if proof.FinalSequence != 3 || proof.Left.Phase != RangeCatchingUp || proof.Right.Phase != RangeCatchingUp {
		t.Fatalf("unexpected target proof: %#v", proof)
	}
	if first.Row.Sequence != 2 || second.Row.Sequence != 3 {
		t.Fatalf("source sequence not durable: %d %d", first.Row.Sequence, second.Row.Sequence)
	}
	row, err := harness.Read("users", []byte("b"))
	if err != nil || !bytes.Equal(row.Value, []byte(`{"v":2}`)) {
		t.Fatalf("source serving read changed: row=%#v err=%v", row, err)
	}
}

func TestSplitTransferRequiresSafeSecondaryUnlessExplicitFallback(t *testing.T) {
	harness, err := NewSplitHarness(transferDescriptor(), []storage.Row{transferRow(t, "users", "a", `{"v":1}`, 1)})
	if err != nil {
		t.Fatal(err)
	}
	harness.SetSecondaryAvailable(2, false)
	harness.SetSecondaryAvailable(3, false)
	if err := harness.BeginSplit([]byte("m"), "left", "right", false); !errors.Is(err, ErrSplitUnsafeCopy) {
		t.Fatalf("unsafe secondary returned %v", err)
	}
	if err := harness.BeginSplit([]byte("m"), "left", "right", true); err != nil {
		t.Fatalf("explicit deterministic fallback rejected: %v", err)
	}
}
