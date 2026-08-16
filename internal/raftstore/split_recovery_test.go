package raftstore

import (
	"errors"
	"testing"
)

func completeRecoveryManifest() SplitRecoveryManifest {
	return SplitRecoveryManifest{Generation: 2, Source: transferDescriptor(), Barrier: 1, SnapshotChecksum: 7, CatalogVersion: 2, Digest: [32]byte{1}, Complete: true}
}

func TestSplitRecoveryAcceptsOnlyCompleteGenerationMarkers(t *testing.T) {
	manifest := completeRecoveryManifest()
	if err := RecoverSplitGeneration(manifest); err != nil {
		t.Fatal(err)
	}
	if !IsCompleteSplitGeneration(manifest) {
		t.Fatal("complete marker was not recoverable")
	}
	manifest.Complete = false
	if err := RecoverSplitGeneration(manifest); !errors.Is(err, ErrSplitPending) {
		t.Fatalf("incomplete generation returned %v", err)
	}
	manifest = completeRecoveryManifest()
	manifest.Digest = [32]byte{}
	if err := RecoverSplitGeneration(manifest); !errors.Is(err, ErrSplitChecksum) {
		t.Fatalf("empty generation digest returned %v", err)
	}
}

func TestSplitRecoveryRejectsRetiredSourceAndForeignCatalogMembership(t *testing.T) {
	manifest := completeRecoveryManifest()
	manifest.Source.Phase = RangeRetired
	if err := RecoverSplitGeneration(manifest); !errors.Is(err, ErrSplitGeneration) {
		t.Fatalf("retired source marker returned %v", err)
	}
	catalog, err := NewRangeCatalog(1, []RangeDescriptor{transferDescriptor()})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecoveryCatalog(catalog, []uint64{1, 2, 3}); err != nil {
		t.Fatalf("matching catalog membership rejected: %v", err)
	}
	if err := ValidateRecoveryCatalog(catalog, []uint64{1, 2, 4}); !errors.Is(err, ErrRangeInvalid) {
		t.Fatalf("foreign catalog membership returned %v", err)
	}
}
