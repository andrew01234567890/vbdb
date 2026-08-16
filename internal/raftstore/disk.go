package raftstore

// This file is the Raft storage adapter for the owned M2 engine.  It is
// intentionally not a second WAL: every record below is an opaque engine
// key/value and Engine.Apply is the only durability boundary.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/andrew01234567890/vbdb/internal/engine"
	raft "go.etcd.io/raft/v3"
	pb "go.etcd.io/raft/v3/raftpb"
	"google.golang.org/protobuf/proto"
)

const (
	diskMagic        = "VBR3"
	diskFormat       = byte(1)
	kHardState       = "m3/raft/hardstate"
	kApplied         = "m3/raft/applied"
	kConfState       = "m3/raft/confstate"
	kConfIndex       = "m3/raft/confstate-index"
	kSnapshot        = "m3/raft/snapshot"
	kSnapshotPre     = "m3/raft/snapshot/"
	kNodeID          = "m3/raft/node-id"
	kBootstrap       = "m3/raft/bootstrap"
	kEntryPre        = "m3/raft/entry/"
	kRowPre          = "m3/state/row/"
	kHistPre         = "m3/state/history/"
	kDedupePre       = "m3/state/dedupe/"
	kResultPre       = "m3/state/result/"
	kStateMeta       = "m3/state/meta"
	kStateGeneration = "m3/state/generation"
	kCatalog         = "m4/catalog/complete"
	maxSnapshotChunk = 256 << 10
	maxSnapshotBytes = 64 << 20
	maxReadyEntries  = 32
	maxReadyBytes    = 8 << 20
)

var diskCRC = crc32.MakeTable(crc32.Castagnoli)

type diskStore struct {
	mu                  sync.RWMutex
	db                  *engine.Engine
	entries             map[uint64]*pb.Entry
	hardState           *pb.HardState
	confState           *pb.ConfState
	confStateIndex      uint64
	snapshot            *pb.Snapshot
	applied             uint64
	failed              error
	syncFail            func(string) error
	nodeID              uint64
	hadNodeID           bool
	bootstrapPeers      []uint64
	bootstrapIncomplete bool
	catalogData         []byte
}

func openDisk(dir string, expectedNodeID uint64, syncFail func(string) error) (*diskStore, error) {
	if err := rejectPebbleDirectory(dir); err != nil {
		return nil, err
	}
	// A successful result retains the canonical command and (for a successful
	// PUT) the applied row.  The public row stays capped at 1 MiB; this larger
	// explicit adapter bound is the finite envelope required to persist both
	// copies without asking the M2 engine to accept an unbounded value.
	db, err := engine.Open(dir, engine.Options{MaxValueBytes: maxResultBytes + 32})
	if err != nil {
		return nil, fmt.Errorf("raftstore: open owned engine: %w", err)
	}
	d := &diskStore{db: db, entries: make(map[uint64]*pb.Entry), hardState: &pb.HardState{}, confState: &pb.ConfState{}, snapshot: pb.EnsureSnapshot(nil), syncFail: syncFail, nodeID: expectedNodeID}
	if err := d.load(expectedNodeID); err != nil {
		_ = db.Close()
		return nil, err
	}
	return d, nil
}

// Pebble leaves CURRENT/MANIFEST/OPTIONS files.  They are rejected before the
// owned engine opens the directory; this unreleased project has no migration
// format and must never silently reinterpret an old store.
func rejectPebbleDirectory(dir string) error {
	entries, err := os.ReadDir(filepath.Clean(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("raftstore: inspect data directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "CURRENT" || strings.HasPrefix(name, "MANIFEST-") || strings.HasPrefix(name, "OPTIONS-") || strings.HasPrefix(name, "000") && strings.HasSuffix(name, ".log") {
			return fmt.Errorf("raftstore: obsolete Pebble directory rejected: %s", name)
		}
	}
	return nil
}

func wrapDisk(kind byte, payload []byte) []byte {
	out := make([]byte, 0, 10+len(payload))
	out = append(out, diskMagic...)
	out = append(out, diskFormat, kind)
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(payload)))
	out = append(out, n[:]...)
	out = append(out, payload...)
	binary.BigEndian.PutUint32(n[:], crc32.Checksum(out, diskCRC))
	return append(out, n[:]...)
}

func unwrapDisk(encoded []byte, kind byte) ([]byte, error) {
	if len(encoded) < 14 || !bytes.Equal(encoded[:4], []byte(diskMagic)) || encoded[4] != diskFormat || encoded[5] != kind {
		return nil, ErrCorrupt
	}
	length := int(binary.BigEndian.Uint32(encoded[6:10]))
	if length < 0 || len(encoded) != 14+length || crc32.Checksum(encoded[:len(encoded)-4], diskCRC) != binary.BigEndian.Uint32(encoded[len(encoded)-4:]) {
		return nil, ErrCorrupt
	}
	return append([]byte(nil), encoded[10:10+length]...), nil
}

func (d *diskStore) scan() ([]engine.Entry, error) {
	stream, err := d.db.Stream(nil, nil)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	var out []engine.Entry
	for {
		entry, ok, err := stream.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		out = append(out, entry)
	}
	return out, stream.Err()
}

func (d *diskStore) load(expectedNodeID uint64) error {
	items, err := d.scan()
	if err != nil {
		return fmt.Errorf("raftstore: scan: %w", err)
	}
	seenPersistent := false
	for _, item := range items {
		key, value := item.Key, item.Value
		seenPersistent = true
		switch {
		case bytes.Equal(key, []byte(kNodeID)):
			payload, err := unwrapDisk(value, 6)
			if err != nil || len(payload) != 8 {
				return d.corrupt("node identity", err)
			}
			stored := binary.BigEndian.Uint64(payload)
			if stored == 0 || stored != expectedNodeID {
				return fmt.Errorf("raftstore: node identity mismatch: stored=%d requested=%d", stored, expectedNodeID)
			}
			d.nodeID, d.hadNodeID = stored, true
		case bytes.Equal(key, []byte(kBootstrap)):
			payload, err := unwrapDisk(value, 7)
			if err != nil || len(payload) == 0 || len(payload)%8 != 0 {
				return d.corrupt("bootstrap peers", err)
			}
			d.bootstrapPeers = make([]uint64, len(payload)/8)
			for i := range d.bootstrapPeers {
				d.bootstrapPeers[i] = binary.BigEndian.Uint64(payload[i*8:])
				if d.bootstrapPeers[i] == 0 || (i > 0 && d.bootstrapPeers[i] <= d.bootstrapPeers[i-1]) {
					return d.corrupt("bootstrap peers", nil)
				}
			}
		case bytes.Equal(key, []byte(kCatalog)):
			payload, err := unwrapDisk(value, 14)
			if err != nil {
				return d.corrupt("range catalog", err)
			}
			catalog, err := UnmarshalRangeCatalog(payload)
			if err != nil {
				return d.corrupt("range catalog", err)
			}
			canonical, err := catalog.MarshalBinary()
			if err != nil || !bytes.Equal(canonical, payload) {
				return d.corrupt("range catalog canonicality", err)
			}
			d.catalogData = append([]byte(nil), payload...)
		case bytes.Equal(key, []byte(kHardState)):
			payload, err := unwrapDisk(value, 1)
			if err != nil {
				return d.corrupt("hard state", err)
			}
			d.hardState = &pb.HardState{}
			if err := proto.Unmarshal(payload, d.hardState); err != nil {
				return d.corrupt("hard state protobuf", err)
			}
		case bytes.Equal(key, []byte(kApplied)):
			payload, err := unwrapDisk(value, 2)
			if err != nil || len(payload) != 8 {
				return d.corrupt("applied", err)
			}
			d.applied = binary.BigEndian.Uint64(payload)
		case bytes.Equal(key, []byte(kConfState)):
			payload, err := unwrapDisk(value, 3)
			if err != nil {
				return d.corrupt("conf state", err)
			}
			d.confState = &pb.ConfState{}
			if err := proto.Unmarshal(payload, d.confState); err != nil {
				return d.corrupt("conf state protobuf", err)
			}
		case bytes.Equal(key, []byte(kConfIndex)):
			payload, err := unwrapDisk(value, 8)
			if err != nil || len(payload) != 8 {
				return d.corrupt("conf state index", err)
			}
			d.confStateIndex = binary.BigEndian.Uint64(payload)
		case bytes.Equal(key, []byte(kSnapshot)):
			payload, err := unwrapDisk(value, 4)
			if err != nil {
				return d.corrupt("snapshot metadata", err)
			}
			d.snapshot = &pb.Snapshot{}
			if err := proto.Unmarshal(payload, d.snapshot); err != nil {
				return d.corrupt("snapshot protobuf", err)
			}
		case bytes.HasPrefix(key, []byte(kSnapshotPre)):
			// Chunks are validated when the active snapshot is reconstructed.
			if _, err := unwrapDisk(value, 11); err != nil {
				return d.corrupt("snapshot chunk", err)
			}
		case bytes.HasPrefix(key, []byte(kEntryPre)):
			if len(key) != len(kEntryPre)+8 {
				return d.corrupt("entry key", nil)
			}
			index := binary.BigEndian.Uint64(key[len(kEntryPre):])
			payload, err := unwrapDisk(value, 5)
			if err != nil {
				return d.corrupt("entry", err)
			}
			entry := &pb.Entry{}
			if err := proto.Unmarshal(payload, entry); err != nil || entry.GetIndex() != index || index == 0 || entry.GetTerm() == 0 {
				return d.corrupt("entry protobuf", err)
			}
			d.entries[index] = entry
		case bytes.HasPrefix(key, []byte(kRowPre)), bytes.HasPrefix(key, []byte(kHistPre)), bytes.HasPrefix(key, []byte(kDedupePre)), bytes.HasPrefix(key, []byte(kResultPre)), bytes.HasPrefix(key, []byte("m3/state/g/")), bytes.Equal(key, []byte(kStateMeta)), bytes.Equal(key, []byte(kStateGeneration)):
			if _, err := unwrapDisk(value, 10); err != nil {
				return d.corrupt("logical record", err)
			}
		default:
			return d.corrupt("unknown keyspace", nil)
		}
	}
	if d.snapshot == nil {
		d.snapshot = pb.EnsureSnapshot(nil)
	} else {
		d.snapshot = pb.EnsureSnapshot(d.snapshot)
		if _, err := d.snapshotDataLocked(); err != nil {
			return d.corrupt("snapshot data", err)
		}
	}
	if err := validateConfState(d.confState); err != nil {
		return d.corrupt("conf state", err)
	}
	if !d.hadNodeID && seenPersistent {
		return d.corrupt("node identity missing", nil)
	}
	if d.hadNodeID && len(d.bootstrapPeers) == 0 {
		return d.corrupt("bootstrap peer set missing", nil)
	}
	if d.hadNodeID && len(d.entries) == 0 && d.hardState.GetTerm() == 0 {
		d.bootstrapIncomplete = true
	}
	if d.snapshot.GetMetadata().GetIndex() > d.hardState.GetCommit() && d.hardState.GetCommit() != 0 {
		return d.corrupt("snapshot ahead of commit", nil)
	}
	if d.applied > d.hardState.GetCommit() {
		return d.corrupt("applied ahead of commit", nil)
	}
	indices := make([]uint64, 0, len(d.entries))
	for index := range d.entries {
		if index > d.snapshot.GetMetadata().GetIndex() {
			indices = append(indices, index)
		}
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
	first := d.snapshot.GetMetadata().GetIndex() + 1
	if d.snapshot.GetMetadata().GetIndex() == 0 {
		first = 1
	}
	for i, index := range indices {
		want := first + uint64(i)
		if index != want {
			return d.corrupt("entry gap", nil)
		}
	}
	return nil
}

func (d *diskStore) snapshotDataLocked() ([]byte, error) {
	index := d.snapshot.GetMetadata().GetIndex()
	if index == 0 {
		return nil, nil
	}
	prefix := []byte(fmt.Sprintf("%s%020d/", kSnapshotPre, index))
	items, err := d.scan()
	if err != nil {
		return nil, err
	}
	parts := make(map[int][]byte)
	for _, item := range items {
		if !bytes.HasPrefix(item.Key, prefix) {
			continue
		}
		suffix := string(item.Key[len(prefix):])
		if len(suffix) != 8 {
			return nil, ErrCorrupt
		}
		offset, err := strconv.Atoi(suffix)
		if err != nil || offset < 0 {
			return nil, ErrCorrupt
		}
		part, err := unwrapDisk(item.Value, 11)
		if err != nil || len(part) > maxSnapshotChunk {
			return nil, ErrCorrupt
		}
		if _, exists := parts[offset]; exists {
			return nil, ErrCorrupt
		}
		parts[offset] = part
	}
	var data []byte
	next := 0
	for ; ; next++ {
		part, ok := parts[next]
		if !ok {
			break
		}
		data = append(data, part...)
	}
	if len(parts) != 0 && len(parts) != next {
		return nil, ErrCorrupt
	}
	return data, nil
}

func (d *diskStore) corrupt(what string, err error) error {
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrCorrupt, what, err)
	}
	return fmt.Errorf("%w: %s", ErrCorrupt, what)
}

func (d *diskStore) persistIdentityAndBootstrap(peers []raft.Peer) error {
	ids := make([]uint64, len(peers))
	for i, peer := range peers {
		if peer.ID == 0 || len(peer.Context) != 0 {
			return errors.New("raftstore: invalid bootstrap peer")
		}
		ids[i] = peer.ID
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			return errors.New("raftstore: duplicate bootstrap peer")
		}
	}
	payload := make([]byte, len(ids)*8)
	for i, id := range ids {
		binary.BigEndian.PutUint64(payload[i*8:], id)
	}
	identity := make([]byte, 8)
	binary.BigEndian.PutUint64(identity, d.nodeID)
	b := d.db.NewBatch()
	if err := b.Put([]byte(kNodeID), wrapDisk(6, identity)); err != nil {
		return err
	}
	if err := b.Put([]byte(kBootstrap), wrapDisk(7, payload)); err != nil {
		return err
	}
	if err := d.beforeSync("bootstrap"); err != nil {
		d.failed = err
		return err
	}
	if _, err := d.db.Apply(&b); err != nil {
		d.failed = err
		return err
	}
	d.mu.Lock()
	d.hadNodeID = true
	d.bootstrapPeers = ids
	d.mu.Unlock()
	return nil
}

func (d *diskStore) InitialState() (*pb.HardState, *pb.ConfState, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.failed != nil {
		return nil, nil, d.failed
	}
	return proto.Clone(d.hardState).(*pb.HardState), proto.Clone(d.confState).(*pb.ConfState), nil
}

func (d *diskStore) Entries(lo, hi, maxSize uint64) ([]*pb.Entry, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	first, _ := d.firstIndexLocked()
	last, _ := d.lastIndexLocked()
	if lo < first {
		return nil, raft.ErrCompacted
	}
	if hi > last+1 {
		return nil, raft.ErrUnavailable
	}
	if lo >= hi {
		return nil, nil
	}
	var out []*pb.Entry
	var total uint64
	for i := lo; i < hi; i++ {
		e := d.entries[i]
		if e == nil {
			return nil, raft.ErrUnavailable
		}
		size := uint64(proto.Size(e))
		if len(out) > 0 && total+size > maxSize {
			break
		}
		total += size
		out = append(out, proto.Clone(e).(*pb.Entry))
	}
	return out, nil
}
func (d *diskStore) Term(index uint64) (uint64, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if index == d.snapshot.GetMetadata().GetIndex() {
		return d.snapshot.GetMetadata().GetTerm(), nil
	}
	e := d.entries[index]
	if e == nil {
		if index < d.snapshot.GetMetadata().GetIndex() {
			return 0, raft.ErrCompacted
		}
		return 0, raft.ErrUnavailable
	}
	return e.GetTerm(), nil
}
func (d *diskStore) LastIndex() (uint64, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.lastIndexLocked()
}
func (d *diskStore) lastIndexLocked() (uint64, error) {
	last := d.snapshot.GetMetadata().GetIndex()
	for i := range d.entries {
		if i > last {
			last = i
		}
	}
	return last, nil
}
func (d *diskStore) FirstIndex() (uint64, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	first, _ := d.firstIndexLocked()
	return first, nil
}
func (d *diskStore) firstIndexLocked() (uint64, error) {
	if i := d.snapshot.GetMetadata().GetIndex(); i > 0 {
		return i + 1, nil
	}
	if len(d.entries) == 0 {
		return 1, nil
	}
	first := uint64(^uint64(0))
	for i := range d.entries {
		if i < first {
			first = i
		}
	}
	return first, nil
}
func (d *diskStore) Snapshot() (*pb.Snapshot, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.failed != nil {
		return nil, d.failed
	}
	snap := proto.Clone(d.snapshot).(*pb.Snapshot)
	data, err := d.snapshotDataLocked()
	if err != nil {
		return nil, err
	}
	snap.Data = data
	return snap, nil
}
func (d *diskStore) confStateCopy() *pb.ConfState {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return proto.Clone(d.confState).(*pb.ConfState)
}

func (d *diskStore) catalogCopy() []byte {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return append([]byte(nil), d.catalogData...)
}
func (d *diskStore) commitIndex() uint64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.hardState.GetCommit()
}

// persistCatalog writes one complete canonical image through the owned engine
// before the caller publishes it to the serving catalog pointer. There is no
// prefix delete or second WAL: the complete value is the durable authority.
func (d *diskStore) persistCatalog(encoded []byte) error {
	catalog, err := UnmarshalRangeCatalog(encoded)
	if err != nil {
		return err
	}
	canonical, err := catalog.MarshalBinary()
	if err != nil || !bytes.Equal(canonical, encoded) {
		return ErrCatalogCorrupt
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failed != nil {
		return d.failed
	}
	if err := d.beforeSync("range-catalog"); err != nil {
		d.failed = err
		return err
	}
	b := d.db.NewBatch()
	if err := b.Put([]byte(kCatalog), wrapDisk(14, encoded)); err != nil {
		return err
	}
	if _, err := d.db.Apply(&b); err != nil {
		d.failed = fmt.Errorf("raftstore: persist range catalog: %w", err)
		return d.failed
	}
	d.catalogData = append([]byte(nil), encoded...)
	return nil
}

func entryKey(index uint64) []byte {
	key := make([]byte, len(kEntryPre)+8)
	copy(key, kEntryPre)
	binary.BigEndian.PutUint64(key[len(kEntryPre):], index)
	return key
}

func (d *diskStore) applyBatch(b *engine.Batch, kind string) error {
	if err := d.beforeSync(kind); err != nil {
		d.failed = err
		return err
	}
	if _, err := d.db.Apply(b); err != nil {
		d.failed = fmt.Errorf("raftstore: %s: %w", kind, err)
		return d.failed
	}
	return nil
}

func (d *diskStore) persistReady(rd raft.Ready) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failed != nil {
		return d.failed
	}
	readyBytes := 0
	if len(rd.Entries) > maxReadyEntries {
		return fmt.Errorf("raftstore: ready exceeds bounded entry count")
	}
	for _, entry := range rd.Entries {
		if entry == nil || entry.GetIndex() == 0 || entry.GetTerm() == 0 {
			return d.corrupt("ready entry", nil)
		}
		readyBytes += proto.Size(entry)
		if readyBytes > maxReadyBytes {
			return fmt.Errorf("raftstore: ready exceeds bounded entry bytes")
		}
	}
	// Snapshot chunks are staged under an index generation.  The metadata key
	// is published only after all chunks are durable, so a crash cannot expose a
	// partial snapshot to restart recovery.
	if !raft.IsEmptySnap(rd.Snapshot) {
		if err := d.stageSnapshotLocked(rd.Snapshot); err != nil {
			d.failed = err
			return err
		}
	}
	b := d.db.NewBatch()
	for _, e := range rd.Entries {
		payload, err := proto.Marshal(e)
		if err != nil {
			return err
		}
		if err := b.Put(entryKey(e.GetIndex()), wrapDisk(5, payload)); err != nil {
			return err
		}
	}
	if !raft.IsEmptyHardState(rd.HardState) {
		payload, _ := proto.Marshal(rd.HardState)
		if err := b.Put([]byte(kHardState), wrapDisk(1, payload)); err != nil {
			return err
		}
	}
	if !raft.IsEmptySnap(rd.Snapshot) {
		meta := proto.Clone(rd.Snapshot).(*pb.Snapshot)
		meta.Data = nil
		payload, _ := proto.Marshal(meta)
		if err := b.Put([]byte(kSnapshot), wrapDisk(4, payload)); err != nil {
			return err
		}
	}
	if len(rd.Entries) == 0 && raft.IsEmptyHardState(rd.HardState) && raft.IsEmptySnap(rd.Snapshot) {
		return nil
	}
	if err := d.applyBatch(&b, "raft-ready"); err != nil {
		return err
	}
	for _, e := range rd.Entries {
		d.entries[e.GetIndex()] = proto.Clone(e).(*pb.Entry)
	}
	if !raft.IsEmptyHardState(rd.HardState) {
		d.hardState = proto.Clone(rd.HardState).(*pb.HardState)
	}
	if !raft.IsEmptySnap(rd.Snapshot) {
		snap := proto.Clone(rd.Snapshot).(*pb.Snapshot)
		snap.Data = nil
		d.snapshot = snap
	}
	return nil
}

func (d *diskStore) stageSnapshotLocked(snapshot *pb.Snapshot) error {
	if len(snapshot.GetData()) > maxSnapshotBytes {
		return fmt.Errorf("raftstore: snapshot exceeds bounded size")
	}
	index := snapshot.GetMetadata().GetIndex()
	if index == 0 {
		return errors.New("raftstore: empty snapshot index")
	}
	for offset := 0; offset < len(snapshot.Data); {
		end := offset + maxSnapshotChunk
		if end > len(snapshot.Data) {
			end = len(snapshot.Data)
		}
		b := d.db.NewBatch()
		key := []byte(fmt.Sprintf("%s%020d/%08d", kSnapshotPre, index, offset/maxSnapshotChunk))
		if err := b.Put(key, wrapDisk(11, snapshot.Data[offset:end])); err != nil {
			return err
		}
		if err := d.applyBatch(&b, "snapshot-chunk"); err != nil {
			return err
		}
		offset = end
	}
	return nil
}

func (d *diskStore) persistLocalSnapshot(snapshot *pb.Snapshot) error {
	if snapshot == nil {
		return errors.New("raftstore: nil snapshot")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failed != nil {
		return d.failed
	}
	if err := d.stageSnapshotLocked(snapshot); err != nil {
		d.failed = err
		return err
	}
	meta := proto.Clone(snapshot).(*pb.Snapshot)
	meta.Data = nil
	payload, err := proto.Marshal(meta)
	if err != nil {
		return err
	}
	b := d.db.NewBatch()
	if err := b.Put([]byte(kSnapshot), wrapDisk(4, payload)); err != nil {
		return err
	}
	if err := d.applyBatch(&b, "snapshot-metadata"); err != nil {
		return err
	}
	d.snapshot = meta
	return nil
}

func (d *diskStore) beforeSync(kind string) error {
	if d.syncFail != nil {
		if err := d.syncFail(kind); err != nil {
			return fmt.Errorf("raftstore: %s sync: %w", kind, err)
		}
	}
	return nil
}
func (d *diskStore) close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.db == nil {
		return nil
	}
	err := d.db.Close()
	d.db = nil
	return err
}

func isKnownRaftKey(key []byte) bool {
	return bytes.Equal(key, []byte(kHardState)) || bytes.Equal(key, []byte(kApplied)) ||
		bytes.Equal(key, []byte(kConfState)) || bytes.Equal(key, []byte(kConfIndex)) ||
		bytes.Equal(key, []byte(kSnapshot)) || bytes.HasPrefix(key, []byte(kSnapshotPre)) ||
		bytes.Equal(key, []byte(kNodeID)) || bytes.Equal(key, []byte(kBootstrap)) ||
		bytes.HasPrefix(key, []byte(kEntryPre)) || bytes.Equal(key, []byte(kStateGeneration)) || bytes.Equal(key, []byte(kCatalog))
}

func isAnyLogicalKey(key []byte) bool {
	return bytes.HasPrefix(key, []byte(kRowPre)) || bytes.HasPrefix(key, []byte(kHistPre)) ||
		bytes.HasPrefix(key, []byte(kDedupePre)) || bytes.HasPrefix(key, []byte(kResultPre)) ||
		bytes.HasPrefix(key, []byte("m3/state/g/")) || bytes.Equal(key, []byte(kStateMeta)) ||
		bytes.Equal(key, []byte(kStateGeneration))
}

// validateConfState is deliberately stricter than raft's protobuf decoder.
// Joint membership fields are outside M3 and must be rejected before any
// incoming or recovered state reaches raft's joint-tracker code.
func validateConfState(state *pb.ConfState) error {
	if state == nil {
		return errors.New("nil conf state")
	}
	if len(state.GetVotersOutgoing()) != 0 || len(state.GetLearnersNext()) != 0 || state.GetAutoLeave() {
		return errors.New("joint membership is outside the M3 contract")
	}
	seen := make(map[uint64]struct{})
	for _, id := range append(append(append([]uint64{}, state.GetVoters()...), state.GetLearners()...), state.GetVotersOutgoing()...) {
		if id == 0 {
			return errors.New("membership node id is zero")
		}
		if _, ok := seen[id]; ok {
			return errors.New("duplicate membership node id")
		}
		seen[id] = struct{}{}
	}
	return nil
}

func u64ptr(value uint64) *uint64 { return &value }

var _ raft.Storage = (*diskStore)(nil)
