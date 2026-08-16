package raftstore

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func testReadDescriptor() RangeDescriptor {
	return RangeDescriptor{RangeID: "range-a", Generation: 1, OwnerEpoch: 1, GroupID: 7, Voters: []uint64{1, 2, 3}, Phase: RangeServing}
}

func TestReadContextIsUniqueAndBounded(t *testing.T) {
	nonce := bytes.Repeat([]byte{0x11}, 16)
	first, err := encodeReadContext(nonce, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := encodeReadContext(nonce, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != readContextBytes || bytes.Equal(first, second) {
		t.Fatalf("contexts are not unique and bounded: %x %x", first, second)
	}
	if err := decodeReadContext(first, nonce); err != nil {
		t.Fatalf("valid context rejected: %v", err)
	}
	if err := decodeReadContext(first, bytes.Repeat([]byte{0x22}, 16)); !errors.Is(err, ErrReadContext) {
		t.Fatalf("context from another boot accepted: %v", err)
	}
	first[len(first)-1] ^= 1
	if err := decodeReadContext(first, nonce); !errors.Is(err, ErrReadContext) {
		t.Fatalf("checksum corruption accepted: %v", err)
	}
}

func TestReadAdmissionCancellationRetiresContext(t *testing.T) {
	r := &Replica{
		reads:        make(map[string]*pendingRead),
		retiredReads: make(map[string]struct{}),
		readNonce:    bytes.Repeat([]byte{0x33}, 16),
	}
	encoded, _, err := r.admitRead("users", "alice", testReadDescriptor())
	if err != nil {
		t.Fatal(err)
	}
	r.cancelRead(encoded)
	if len(r.reads) != 0 || r.readBytes != 0 {
		t.Fatalf("canceled read retained state: reads=%d bytes=%d", len(r.reads), r.readBytes)
	}
	if _, ok := r.retiredReads[string(encoded)]; !ok {
		t.Fatal("canceled context was not retired")
	}
}

func TestReadAdmissionBackpressureAndSequenceExhaustion(t *testing.T) {
	r := &Replica{
		reads:        make(map[string]*pendingRead),
		retiredReads: make(map[string]struct{}),
		readNonce:    bytes.Repeat([]byte{0x44}, 16),
	}
	r.readSeq = ^uint64(0)
	if _, _, err := r.admitRead("users", "alice", testReadDescriptor()); !errors.Is(err, ErrReadContextExhausted) {
		t.Fatalf("sequence exhaustion returned %v", err)
	}
	r.readSeq = 0
	for i := 0; i < maxPendingReads; i++ {
		key := string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
		if _, _, err := r.admitRead("users", key, testReadDescriptor()); err != nil {
			t.Fatalf("admission %d failed: %v", i, err)
		}
	}
	if _, _, err := r.admitRead("users", "overflow", testReadDescriptor()); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("count limit returned %v", err)
	}
}

func TestReadFinishWakesPendingWaiters(t *testing.T) {
	r := &Replica{
		reads:        make(map[string]*pendingRead),
		retiredReads: make(map[string]struct{}),
		readNonce:    bytes.Repeat([]byte{0x55}, 16),
	}
	_, waiter, err := r.admitRead("users", "alice", testReadDescriptor())
	if err != nil {
		t.Fatal(err)
	}
	r.finishReads(ErrStopped)
	select {
	case delivery := <-waiter:
		if !errors.Is(delivery.err, ErrStopped) {
			t.Fatalf("waiter received %v", delivery.err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending waiter was not woken")
	}
}
