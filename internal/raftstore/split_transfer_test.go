package raftstore

import (
	"bytes"
	"errors"
	"testing"

	"github.com/andrew01234567890/vbdb/internal/storage"
	"github.com/andrew01234567890/vbdb/pkg/uuidv7"
)

func transferDescriptor() RangeDescriptor {
	return RangeDescriptor{RangeID: "source", Generation: 1, OwnerEpoch: 1, GroupID: 9, Voters: []uint64{1, 2, 3}, Phase: RangeServing}
}

func transferRow(t *testing.T, table, key, value string, sequence uint64) storage.Row {
	t.Helper()
	version, err := uuidv7.New()
	if err != nil {
		t.Fatal(err)
	}
	return storage.Row{Table: table, Key: key, Version: version, Sequence: sequence, Value: []byte(value)}
}

func TestSplitSnapshotChunksReorderDuplicateAndChecksum(t *testing.T) {
	source := transferDescriptor()
	snapshot, err := BuildSplitSnapshot(source, 2, source.OwnerEpoch, []storage.Row{transferRow(t, "users", "a", `{"v":1}`, 1), transferRow(t, "users", "z", `{"v":2}`, 2)})
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := snapshot.CopyChunks()
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 || len(chunks[0]) > maxSplitCopyChunkBytes {
		t.Fatalf("invalid bounded chunks: %d", len(chunks))
	}
	transferred := make([][]byte, 0, len(chunks)+1)
	for i := len(chunks) - 1; i >= 0; i-- {
		transferred = append(transferred, chunks[i])
	}
	transferred = append(transferred, chunks[0])
	restored, err := receiveSplitChunks(transferred)
	if err != nil {
		t.Fatal(err)
	}
	if !EqualRangeDescriptor(restored.Source, source) || restored.Barrier != snapshot.Barrier || len(restored.Rows) != len(snapshot.Rows) || !bytes.Equal(restored.Rows[0].Value, snapshot.Rows[0].Value) {
		t.Fatalf("restored snapshot differs: %#v", restored)
	}
	conflicting := append([]byte(nil), chunks[0]...)
	conflicting[len(conflicting)-1] ^= 1
	if _, err := receiveSplitChunks(append(transferred, conflicting)); !errors.Is(err, ErrSplitChecksum) {
		t.Fatalf("conflicting duplicate returned %v", err)
	}
}

func TestSplitTransferBudgetRejectsOverflowBeforeAllocation(t *testing.T) {
	if _, err := splitMemoryAdd(^int(0), 1); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("addition overflow returned %v", err)
	}
	if _, err := splitMemoryMul(^int(0), 2); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("multiplication overflow returned %v", err)
	}
	if _, err := splitTransferScratchMemory(transferDescriptor(), nil); err != nil {
		t.Fatalf("empty transfer scratch rejected: %v", err)
	}
}
