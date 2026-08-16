package raftstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/andrew01234567890/vbdb/internal/storage"
	raft "go.etcd.io/raft/v3"
)

const (
	readMagic          = "VBR4"
	readContextBytes   = 4 + 16 + 8 + 4
	maxPendingReads    = 1024
	maxPendingReadByte = 256 << 10
	maxRetiredReads    = 2048
)

var readCRC = crc32.MakeTable(crc32.Castagnoli)

var (
	ErrReadIndexUnavailable = errors.New("raftstore: read index unavailable")
	ErrReadContext          = errors.New("raftstore: invalid read index context")
	ErrReadContextExhausted = errors.New("raftstore: read index context exhausted")
)

type pendingRead struct {
	table      string
	key        string
	descriptor RangeDescriptor
	bytes      int
	waiter     chan readDelivery
}

type readDelivery struct {
	index uint64
	err   error
}

// encodeReadContext creates a bounded, checksummed context. The durable boot
// nonce is part of the identity so sequence reuse after restart is impossible.
func encodeReadContext(nonce []byte, sequence uint64) ([]byte, error) {
	if len(nonce) != 16 || sequence == 0 {
		return nil, ErrReadContext
	}
	encoded := make([]byte, readContextBytes)
	copy(encoded, readMagic)
	copy(encoded[4:20], nonce)
	binary.BigEndian.PutUint64(encoded[20:28], sequence)
	binary.BigEndian.PutUint32(encoded[28:], crc32.Checksum(encoded[:28], readCRC))
	return encoded, nil
}

func decodeReadContext(encoded, nonce []byte) error {
	if len(encoded) != readContextBytes || !bytes.Equal(encoded[:4], []byte(readMagic)) || len(nonce) != 16 || !bytes.Equal(encoded[4:20], nonce) {
		return ErrReadContext
	}
	if binary.BigEndian.Uint64(encoded[20:28]) == 0 || crc32.Checksum(encoded[:28], readCRC) != binary.BigEndian.Uint32(encoded[28:]) {
		return ErrReadContext
	}
	return nil
}

func readDescriptorBytes(descriptor RangeDescriptor) int {
	return len(descriptor.RangeID) + len(descriptor.Start) + len(descriptor.End) + len(descriptor.Voters)*8 + 40
}

func (r *Replica) currentRoute(key, rangeID string, generation, ownerEpoch, groupID uint64) (RangeDescriptor, uint64, error) {
	r.catalogMu.RLock()
	defer r.catalogMu.RUnlock()
	if r.rangeCatalog == nil {
		return RangeDescriptor{}, 0, ErrCatalogCorrupt
	}
	descriptor, err := r.rangeCatalog.RouteAt([]byte(key), rangeID, generation, ownerEpoch, groupID)
	if err != nil {
		return RangeDescriptor{}, 0, err
	}
	return descriptor, r.rangeCatalog.Version(), nil
}

// ReadIndex performs a quorum-backed linearizable read for the current route.
// A locally present row is never used as a fallback when the quorum fence is
// unavailable.
func (r *Replica) ReadIndex(ctx context.Context, table, key string) (storage.Row, error) {
	if ctx == nil {
		return storage.Row{}, errors.New("raftstore: nil read context")
	}
	if !validTable(table) || !validKey(key) {
		return storage.Row{}, fmt.Errorf("%w: coordinates", ErrInvalidCommand)
	}
	if err := r.readOpen(); err != nil {
		return storage.Row{}, err
	}
	r.catalogMu.RLock()
	if r.rangeCatalog == nil {
		r.catalogMu.RUnlock()
		return storage.Row{}, ErrCatalogCorrupt
	}
	descriptor, err := r.rangeCatalog.Route([]byte(key))
	version := uint64(0)
	if err == nil {
		version = r.rangeCatalog.Version()
	}
	r.catalogMu.RUnlock()
	if err != nil {
		return storage.Row{}, err
	}
	return r.readIndexAt(ctx, table, key, descriptor, version)
}

// ReadIndexAt serves a caller's complete identity coordinates. The current
// catalog supplies the immutable span and voter evidence; all of it is
// revalidated after the applied-index fence before the row is returned.
func (r *Replica) ReadIndexAt(ctx context.Context, table, key, rangeID string, generation, ownerEpoch, groupID uint64) (storage.Row, error) {
	if ctx == nil {
		return storage.Row{}, errors.New("raftstore: nil read context")
	}
	if !validTable(table) || !validKey(key) {
		return storage.Row{}, fmt.Errorf("%w: coordinates", ErrInvalidCommand)
	}
	if err := r.readOpen(); err != nil {
		return storage.Row{}, err
	}
	descriptor, version, err := r.currentRoute(key, rangeID, generation, ownerEpoch, groupID)
	if err != nil {
		return storage.Row{}, err
	}
	return r.readIndexAt(ctx, table, key, descriptor, version)
}

func (r *Replica) readIndexAt(ctx context.Context, table, key string, descriptor RangeDescriptor, catalogVersion uint64) (storage.Row, error) {
	if err := ctx.Err(); err != nil {
		return storage.Row{}, err
	}
	encoded, waiter, err := r.admitRead(table, key, descriptor)
	if err != nil {
		return storage.Row{}, err
	}
	if err := r.node.ReadIndex(ctx, encoded); err != nil {
		r.cancelRead(encoded)
		if errors.Is(err, raft.ErrStopped) {
			return storage.Row{}, ErrStopped
		}
		return storage.Row{}, fmt.Errorf("%w: %v", ErrReadIndexUnavailable, err)
	}
	var delivery readDelivery
	select {
	case delivery = <-waiter:
	case <-ctx.Done():
		r.cancelRead(encoded)
		return storage.Row{}, ctx.Err()
	}
	if delivery.err != nil {
		return storage.Row{}, delivery.err
	}
	if delivery.index == 0 {
		return storage.Row{}, ErrReadIndexUnavailable
	}
	if err := r.WaitAppliedAtLeast(ctx, delivery.index); err != nil {
		return storage.Row{}, err
	}
	if err := r.readOpen(); err != nil {
		return storage.Row{}, err
	}
	r.stateMu.RLock()
	if err := r.readOpen(); err != nil {
		r.stateMu.RUnlock()
		return storage.Row{}, err
	}
	row, err := r.state.get(table, key)
	r.stateMu.RUnlock()
	if err != nil {
		return storage.Row{}, err
	}
	if err := r.validateReadRoute(key, descriptor, catalogVersion); err != nil {
		return storage.Row{}, err
	}
	return row, nil
}

func (r *Replica) admitRead(table, key string, descriptor RangeDescriptor) ([]byte, <-chan readDelivery, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, nil, ErrStopped
	}
	if r.fatal != nil {
		return nil, nil, r.fatal
	}
	if len(r.reads) >= maxPendingReads {
		return nil, nil, ErrBackpressure
	}
	if r.readSeq == ^uint64(0) {
		return nil, nil, ErrReadContextExhausted
	}
	r.readSeq++
	encoded, err := encodeReadContext(r.readNonce, r.readSeq)
	if err != nil {
		return nil, nil, err
	}
	retained := len(encoded) + len(table) + len(key) + readDescriptorBytes(descriptor)
	if retained > maxPendingReadByte || r.readBytes > maxPendingReadByte-retained {
		return nil, nil, ErrBackpressure
	}
	waiter := make(chan readDelivery, 1)
	r.reads[string(encoded)] = &pendingRead{table: table, key: key, descriptor: descriptor.Clone(), bytes: retained, waiter: waiter}
	r.readBytes += retained
	return encoded, waiter, nil
}

func (r *Replica) retireReadLocked(encoded []byte) {
	key := string(encoded)
	r.retiredReads[key] = struct{}{}
	if len(r.retiredReads) > maxRetiredReads {
		for old := range r.retiredReads {
			delete(r.retiredReads, old)
			break
		}
	}
}

func (r *Replica) cancelRead(encoded []byte) {
	r.mu.Lock()
	if pending := r.reads[string(encoded)]; pending != nil {
		delete(r.reads, string(encoded))
		if pending.bytes > r.readBytes {
			r.readBytes = 0
		} else {
			r.readBytes -= pending.bytes
		}
		r.retireReadLocked(encoded)
	}
	r.mu.Unlock()
}

func (r *Replica) deliverReadState(readState raft.ReadState) {
	encoded := append([]byte(nil), readState.RequestCtx...)
	if err := decodeReadContext(encoded, r.readNonce); err != nil {
		r.fail(fmt.Errorf("%w: read state: %v", ErrCorrupt, err))
		return
	}
	r.mu.Lock()
	pending := r.reads[string(encoded)]
	if pending != nil {
		delete(r.reads, string(encoded))
		if pending.bytes > r.readBytes {
			r.readBytes = 0
		} else {
			r.readBytes -= pending.bytes
		}
		r.mu.Unlock()
		if readState.Index == 0 {
			pending.waiter <- readDelivery{err: ErrReadIndexUnavailable}
		} else {
			pending.waiter <- readDelivery{index: readState.Index}
		}
		close(pending.waiter)
		return
	}
	_, retired := r.retiredReads[string(encoded)]
	r.mu.Unlock()
	if retired {
		return
	}
	r.fail(fmt.Errorf("%w: unknown read state context", ErrCorrupt))
}

func (r *Replica) finishReads(err error) {
	r.mu.Lock()
	pending := r.reads
	r.reads = make(map[string]*pendingRead)
	r.readBytes = 0
	r.mu.Unlock()
	for _, read := range pending {
		read.waiter <- readDelivery{err: err}
		close(read.waiter)
	}
}

func (r *Replica) validateReadRoute(key string, descriptor RangeDescriptor, catalogVersion uint64) error {
	r.catalogMu.RLock()
	defer r.catalogMu.RUnlock()
	if r.rangeCatalog == nil {
		return &RangeMovedError{RequestedRangeID: descriptor.RangeID, RequestedGeneration: descriptor.Generation, RequestedOwnerEpoch: descriptor.OwnerEpoch, RequestedGroupID: descriptor.GroupID}
	}
	current, err := r.rangeCatalog.RouteAt([]byte(key), descriptor.RangeID, descriptor.Generation, descriptor.OwnerEpoch, descriptor.GroupID)
	if err != nil {
		return err
	}
	if r.rangeCatalog.Version() != catalogVersion || !EqualRangeDescriptor(current, descriptor) {
		return &RangeMovedError{RequestedRangeID: descriptor.RangeID, RequestedGeneration: descriptor.Generation, RequestedOwnerEpoch: descriptor.OwnerEpoch, RequestedGroupID: descriptor.GroupID, Newest: []RangeDescriptor{current}}
	}
	return nil
}
