package raftstore

import (
	"bytes"
	"errors"
	"testing"
)

func testRange(id string, start, end []byte, generation uint64, phase ServingPhase) RangeDescriptor {
	return RangeDescriptor{RangeID: id, Start: start, End: end, Generation: generation, OwnerEpoch: generation, GroupID: generation, Voters: []uint64{1, 2, 3}, Phase: phase}
}

func TestRangeCatalogRouteFenceCopiesAndGroup(t *testing.T) {
	catalog, err := NewRangeCatalog(1, []RangeDescriptor{testRange("source", nil, nil, 1, RangeServing)})
	if err != nil {
		t.Fatal(err)
	}
	route, err := catalog.Route([]byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	route.Start = []byte("mutated")
	route.Voters[0] = 99
	if _, err := catalog.RouteAt([]byte("k"), "source", 1, 1, 9); !errors.Is(err, ErrRangeMoved) {
		t.Fatalf("group mismatch error = %v", err)
	}
	checked, err := catalog.RouteAt([]byte("k"), "source", 1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(checked.Start, nil) || checked.Voters[0] != 1 {
		t.Fatalf("catalog leaked mutable route: %+v", checked)
	}
	if err := catalog.VerifyGeneration(checked); err != nil {
		t.Fatal(err)
	}
}

func TestRangeCatalogRejectsGapsOverlapsAndInvalidPhase(t *testing.T) {
	left := testRange("left", nil, []byte("m"), 1, RangeServing)
	right := testRange("right", []byte("n"), nil, 1, RangeServing)
	if _, err := NewRangeCatalog(1, []RangeDescriptor{left, right}); !errors.Is(err, ErrRangeGap) {
		t.Fatalf("gap error = %v", err)
	}
	right.Start = []byte("m")
	right.End = []byte("z")
	if _, err := NewRangeCatalog(1, []RangeDescriptor{left, right}); !errors.Is(err, ErrRangeGap) {
		t.Fatalf("bounded final gap error = %v", err)
	}
	left.End = []byte("z")
	if _, err := NewRangeCatalog(1, []RangeDescriptor{left, right}); !errors.Is(err, ErrRangeOverlap) {
		t.Fatalf("overlap error = %v", err)
	}
	left.End = []byte("m")
	left.Phase = ServingPhase(99)
	if _, err := NewRangeCatalog(1, []RangeDescriptor{left, right}); !errors.Is(err, ErrRangeInvalid) {
		t.Fatalf("phase error = %v", err)
	}
}

func TestRangeCatalogCanonicalCodecAndTombstone(t *testing.T) {
	source := testRange("source", nil, nil, 1, RangeServing)
	catalog, err := NewRangeCatalog(1, []RangeDescriptor{source})
	if err != nil {
		t.Fatal(err)
	}
	left := testRange("left", nil, []byte("m"), 2, RangeServing)
	right := testRange("right", []byte("m"), nil, 2, RangeServing)
	if err := catalog.Replace(2, []RangeDescriptor{left, right}); err != nil {
		t.Fatal(err)
	}
	encoded, err := catalog.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalRangeCatalog(encoded)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, err := decoded.MarshalBinary()
	if err != nil || !bytes.Equal(encoded, reencoded) {
		t.Fatalf("non-canonical round trip: err=%v equal=%v", err, bytes.Equal(encoded, reencoded))
	}
	tombstone, ok := decoded.DescriptorByID("source")
	if !ok || tombstone.Phase != RangeRetired {
		t.Fatalf("source tombstone = %+v, ok=%v", tombstone, ok)
	}
	if err := decoded.Replace(3, []RangeDescriptor{source}); !errors.Is(err, ErrCatalogStale) {
		t.Fatalf("resurrection error = %v", err)
	}
	encoded[len(encoded)-1] ^= 1
	if _, err := UnmarshalRangeCatalog(encoded); !errors.Is(err, ErrCatalogCorrupt) {
		t.Fatalf("checksum error = %v", err)
	}
}

func TestRangeCatalogPhaseTransitionsAreMonotonic(t *testing.T) {
	catalog, err := NewRangeCatalog(1, []RangeDescriptor{testRange("source", nil, nil, 1, RangeCopying)})
	if err != nil {
		t.Fatal(err)
	}
	catching := testRange("source", nil, nil, 1, RangeCatchingUp)
	if err := catalog.Replace(2, []RangeDescriptor{catching}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Replace(3, []RangeDescriptor{testRange("source", nil, nil, 1, RangeCopying)}); !errors.Is(err, ErrCatalogStale) {
		t.Fatalf("phase regression error = %v", err)
	}
}
