package raftstore

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	raft "go.etcd.io/raft/v3"
)

func TestDiskCatalogCompleteImageSurvivesReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "catalog")
	disk, err := openDisk(dir, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := disk.persistIdentityAndBootstrap([]raft.Peer{{ID: 1}}); err != nil {
		t.Fatal(err)
	}
	source, err := NewRangeCatalog(1, []RangeDescriptor{{RangeID: "source", Generation: 1, OwnerEpoch: 1, GroupID: 9, Voters: []uint64{1, 2, 3}, Phase: RangeServing}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := disk.persistCatalog(encoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disk.catalogCopy(), encoded) {
		t.Fatal("published catalog bytes differ from complete image")
	}
	if err := disk.close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openDisk(dir, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.close() }()
	if !bytes.Equal(reopened.catalogCopy(), encoded) {
		t.Fatal("restart did not recover the complete catalog image")
	}
	restored, err := UnmarshalRangeCatalog(reopened.catalogCopy())
	if err != nil || restored.Version() != 1 {
		t.Fatalf("restored catalog = %v, %v", restored, err)
	}
}

func TestDiskCatalogUncertainSyncFailsClosed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "catalog")
	disk, err := openDisk(dir, 1, func(kind string) error {
		if kind == "range-catalog" {
			return errors.New("injected sync uncertainty")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = disk.close() }()
	catalog, err := NewRangeCatalog(1, []RangeDescriptor{{RangeID: "source", Generation: 1, OwnerEpoch: 1, GroupID: 9, Voters: []uint64{1, 2, 3}, Phase: RangeServing}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := catalog.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := disk.persistCatalog(encoded); err == nil {
		t.Fatal("uncertain catalog sync unexpectedly succeeded")
	}
	if len(disk.catalogCopy()) != 0 {
		t.Fatal("uncertain catalog became visible")
	}
	if _, err := disk.Snapshot(); err == nil {
		t.Fatal("failed disk remained usable")
	}
}
