// Package engine implements VBDB's small, owned, durable ordered key/value
// engine.  The engine's LSN is a local physical sequence only; it is not a
// Raft index, MVCC timestamp, or user-visible version.
package engine

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
)

const (
	DefaultMaxKeyBytes   = 1 << 10
	DefaultMaxValueBytes = 1 << 20
	DefaultMaxBatchOps   = 4096
	DefaultMaxBatchBytes = 16 << 20
	DefaultMaxScanItems  = 4096
	DefaultMaxSnapshots  = 64
	DefaultSegmentBytes  = 16 << 20
	DefaultMemtableBytes = 4 << 20
	DefaultSSTBlockBytes = 32 << 10
	DefaultMaxSSTBytes   = 128 << 20
	DefaultMaxSSTFiles   = 64
)

var (
	ErrClosed          = errors.New("engine: closed")
	ErrPoisoned        = errors.New("engine: poisoned after uncertain durability")
	ErrCorrupt         = errors.New("engine: corrupt data")
	ErrLocked          = errors.New("engine: data directory is already owned")
	ErrInvalidKey      = errors.New("engine: invalid key")
	ErrValueTooLarge   = errors.New("engine: value is too large")
	ErrBatchTooLarge   = errors.New("engine: batch is too large")
	ErrDuplicateKey    = errors.New("engine: batch contains a duplicate key")
	ErrInvalidBatch    = errors.New("engine: invalid batch")
	ErrInvalidLimit    = errors.New("engine: invalid scan limit")
	ErrNotFound        = errors.New("engine: key not found")
	ErrShortWrite      = errors.New("engine: short WAL write")
	ErrInvalidDataDir  = errors.New("engine: data directory must be owner-only")
	ErrFilesystem      = errors.New("engine: filesystem operation failed")
	ErrTooManySnapshot = errors.New("engine: too many active snapshots")
)

// Options configures Open. Zero limits select the documented defaults.
// FS is intentionally an interface so tests and embedders can inject short,
// zero, truncate, and sync failures without changing the engine.
type Options struct {
	FS            FS
	MaxKeyBytes   int
	MaxValueBytes int
	MaxBatchOps   int
	MaxBatchBytes int
	MaxScanItems  int
	MaxSnapshots  int
	SegmentBytes  int
	MemtableBytes int
	SSTBlockBytes int
	MaxSSTBytes   int
	MaxSSTFiles   int
}

func (o Options) normalized() (Options, error) {
	if o.MaxKeyBytes == 0 {
		o.MaxKeyBytes = DefaultMaxKeyBytes
	}
	if o.MaxValueBytes == 0 {
		o.MaxValueBytes = DefaultMaxValueBytes
	}
	if o.MaxBatchOps == 0 {
		o.MaxBatchOps = DefaultMaxBatchOps
	}
	if o.MaxBatchBytes == 0 {
		o.MaxBatchBytes = DefaultMaxBatchBytes
	}
	if o.MaxScanItems == 0 {
		o.MaxScanItems = DefaultMaxScanItems
	}
	if o.MaxSnapshots == 0 {
		o.MaxSnapshots = DefaultMaxSnapshots
	}
	if o.SegmentBytes == 0 {
		o.SegmentBytes = DefaultSegmentBytes
	}
	if o.MemtableBytes == 0 {
		o.MemtableBytes = DefaultMemtableBytes
	}
	if o.SSTBlockBytes == 0 {
		o.SSTBlockBytes = DefaultSSTBlockBytes
	}
	if o.MaxSSTBytes == 0 {
		o.MaxSSTBytes = DefaultMaxSSTBytes
	}
	if o.MaxSSTFiles == 0 {
		o.MaxSSTFiles = DefaultMaxSSTFiles
	}
	if o.MaxKeyBytes < 1 || o.MaxValueBytes < 1 || o.MaxBatchOps < 1 ||
		o.MaxBatchBytes < 1 || o.MaxScanItems < 1 ||
		o.MaxSnapshots < 1 || o.SegmentBytes < minSegmentBytes || o.MemtableBytes < 1 ||
		o.SSTBlockBytes < 1024 || o.MaxSSTBytes < o.SSTBlockBytes || o.MaxSSTFiles <= l0CompactionTrigger {
		return Options{}, fmt.Errorf("%w: limits must be positive and segment must hold one frame", ErrInvalidBatch)
	}
	if int64(o.MaxBatchBytes) > maxUint32 || int64(o.MaxKeyBytes) > maxUint32 || int64(o.MaxValueBytes) > maxUint32 {
		return Options{}, fmt.Errorf("%w: limit exceeds WAL field width", ErrInvalidBatch)
	}
	// Every valid configuration must be able to encode at least one record in
	// one SST. This includes the per-record checksum, block/index metadata, and
	// footer; oversized values may still occupy one block.
	minRecord := 1 + 4 + 4 + o.MaxKeyBytes + o.MaxValueBytes + 4
	minSST, ok := checkedAdd(sstHeaderLen+blockHeaderLen+sstFooterLen+20+2*o.MaxKeyBytes, minRecord)
	if !ok || int64(minSST) > int64(o.MaxSSTBytes) {
		return Options{}, fmt.Errorf("%w: MaxSSTBytes cannot hold one maximum record", ErrInvalidBatch)
	}
	return o, nil
}

// Batch is an atomic set of distinct PUT and DELETE operations. Put and
// Delete copy caller-owned bytes before returning. Operations are encoded in
// byte-lexicographic key order at Apply time, so the physical batch format is
// canonical regardless of the order in which methods were called.
type Batch struct {
	ops      []operation
	bytes    int
	maxOps   int
	maxBytes int
	maxKey   int
	maxValue int
	sealed   bool
}

// NewBatch returns an unbounded builder. Engine.NewBatch is preferred because
// it applies the engine's configured limits while methods are called.
func NewBatch() Batch { return Batch{} }

func (e *Engine) NewBatch() Batch {
	if e == nil {
		return Batch{}
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return Batch{maxOps: e.opts.MaxBatchOps, maxBytes: e.opts.MaxBatchBytes, maxKey: e.opts.MaxKeyBytes, maxValue: e.opts.MaxValueBytes}
}

func (b *Batch) configureDefaults() {
	if b.maxOps == 0 {
		b.maxOps = DefaultMaxBatchOps
	}
	if b.maxBytes == 0 {
		b.maxBytes = DefaultMaxBatchBytes
	}
	if b.maxKey == 0 {
		b.maxKey = DefaultMaxKeyBytes
	}
	if b.maxValue == 0 {
		b.maxValue = DefaultMaxValueBytes
	}
}

func (b *Batch) Put(key, value []byte) error {
	if b == nil {
		return fmt.Errorf("%w: nil batch", ErrInvalidBatch)
	}
	b.configureDefaults()
	if b.sealed {
		return fmt.Errorf("%w: batch already applied", ErrInvalidBatch)
	}
	if err := validateKey(key, b.maxKey); err != nil {
		return err
	}
	if len(value) > b.maxValue {
		return ErrValueTooLarge
	}
	if len(b.ops) >= b.maxOps {
		return ErrBatchTooLarge
	}
	if _, exists := b.find(key); exists {
		return ErrDuplicateKey
	}
	need, ok := checkedAdd(1+4+4+len(key)+len(value), b.bytes)
	if !ok || need > b.maxBytes {
		return ErrBatchTooLarge
	}
	// key and value are copied only after all individual lengths and aggregate
	// bounds have been checked.
	b.ops = append(b.ops, operation{kind: putOp, key: append([]byte(nil), key...), value: append([]byte(nil), value...)})
	b.bytes = need
	return nil
}

func (b *Batch) Delete(key []byte) error {
	if b == nil {
		return fmt.Errorf("%w: nil batch", ErrInvalidBatch)
	}
	b.configureDefaults()
	if b.sealed {
		return fmt.Errorf("%w: batch already applied", ErrInvalidBatch)
	}
	if err := validateKey(key, b.maxKey); err != nil {
		return err
	}
	if len(b.ops) >= b.maxOps {
		return ErrBatchTooLarge
	}
	if _, exists := b.find(key); exists {
		return ErrDuplicateKey
	}
	need, ok := checkedAdd(1+4+4+len(key), b.bytes)
	if !ok || need > b.maxBytes {
		return ErrBatchTooLarge
	}
	b.ops = append(b.ops, operation{kind: deleteOp, key: append([]byte(nil), key...)})
	b.bytes = need
	return nil
}

func (b *Batch) find(key []byte) (int, bool) {
	for i := range b.ops {
		if bytes.Equal(b.ops[i].key, key) {
			return i, true
		}
	}
	return 0, false
}

// Entry is one ordered key/value result. Both byte slices are fresh copies.
type Entry struct {
	Key   []byte
	Value []byte
}

// KV is retained as a concise alias for callers that prefer KV terminology.
type KV = Entry

// Snapshot is a stable view at one local engine LSN. It pins the referenced
// immutable files until Close, while retaining only one bounded memtable copy.
type Snapshot struct {
	owner *Engine
	mu    sync.RWMutex
	view  *engineView
	lsn   uint64
	once  sync.Once
}

func (s *Snapshot) LSN() uint64 {
	if s == nil {
		return 0
	}
	return s.lsn
}

func (s *Snapshot) Get(key []byte) ([]byte, error) {
	v, release, err := s.acquireView()
	if err != nil {
		return nil, ErrClosed
	}
	defer release()
	return lookupView(v, key)
}

func (s *Snapshot) Scan(start, end []byte, limit int) ([]Entry, error) {
	v, release, err := s.acquireView()
	if err != nil {
		return nil, ErrClosed
	}
	defer release()
	it, err := newIterator(v, start, end, limit)
	if err != nil {
		return nil, err
	}
	defer it.Close()
	entries := make([]Entry, 0, limit)
	for len(entries) < limit {
		entry, ok := it.Next()
		if !ok {
			break
		}
		entries = append(entries, entry)
	}
	return entries, it.err
}

// Last returns the greatest key in [start,end). A nil bound is open.
func (s *Snapshot) Last(start, end []byte) (Entry, error) {
	_, release, err := s.acquireView()
	if err != nil {
		return Entry{}, ErrClosed
	}
	release()
	it, err := s.Iterate(start, end)
	if err != nil {
		return Entry{}, err
	}
	defer it.Close()
	var last Entry
	found := false
	for {
		entry, ok := it.Next()
		if !ok {
			break
		}
		last, found = entry, true
	}
	if it.err != nil {
		return Entry{}, it.err
	}
	if !found {
		return Entry{}, ErrNotFound
	}
	return last, nil
}

func (s *Snapshot) Iterator(start, end []byte, limit int) (*Iterator, error) {
	v, release, err := s.acquireView()
	if err != nil {
		return nil, ErrClosed
	}
	defer release()
	s.owner.mu.Lock()
	s.owner.pins++
	it, err := newIterator(v, start, end, limit)
	s.owner.mu.Unlock()
	if err != nil {
		s.owner.releaseView(v)
		return nil, err
	}
	it.release = true
	return it, nil
}

// Iterate returns a streaming iterator over the complete bounded key range.
// Unlike Scan and Iterator, it does not impose MaxScanItems and does not
// materialize the result set. The stable state pointer keeps each value
// observable until the iterator is closed.
func (s *Snapshot) Iterate(start, end []byte) (*Iterator, error) {
	v, release, err := s.acquireView()
	if err != nil {
		return nil, ErrClosed
	}
	defer release()
	s.owner.mu.Lock()
	s.owner.pins++
	it, err := newStreamingIterator(v, start, end)
	s.owner.mu.Unlock()
	if err != nil {
		s.owner.releaseView(v)
		return nil, err
	}
	it.release = true
	return it, nil
}

// Stream is the error-reporting form of Iterate. Its Next method keeps the
// iteration contract extensible for readers that validate immutable files as
// they walk them; callers must check the returned error before accepting the
// end-of-stream marker.
type Stream struct {
	iterator *Iterator
	err      error
}

func (e *Engine) Stream(start, end []byte) (*Stream, error) {
	it, err := e.Iterate(start, end)
	if err != nil {
		return nil, err
	}
	return &Stream{iterator: it}, nil
}

func (s *Snapshot) Stream(start, end []byte) (*Stream, error) {
	it, err := s.Iterate(start, end)
	if err != nil {
		return nil, err
	}
	return &Stream{iterator: it}, nil
}

func (s *Stream) Next() (Entry, bool, error) {
	if s == nil || s.iterator == nil {
		return Entry{}, false, ErrClosed
	}
	if s.err != nil {
		return Entry{}, false, s.err
	}
	entry, ok := s.iterator.Next()
	s.err = s.iterator.err
	return entry, ok, s.err
}

func (s *Stream) Err() error {
	if s == nil {
		return ErrClosed
	}
	return s.err
}

func (s *Stream) Close() error {
	if s == nil || s.iterator == nil {
		return nil
	}
	return s.iterator.Close()
}

func (s *Snapshot) acquireView() (*engineView, func(), error) {
	if s == nil {
		return nil, func() {}, ErrClosed
	}
	s.mu.RLock()
	v := s.view
	if v == nil {
		s.mu.RUnlock()
		return nil, func() {}, ErrClosed
	}
	return v, s.mu.RUnlock, nil
}

func (s *Snapshot) Close() error {
	if s == nil {
		return nil
	}
	s.once.Do(func() {
		s.mu.Lock()
		v := s.view
		s.view = nil
		s.mu.Unlock()
		if s.owner != nil {
			s.owner.mu.Lock()
			if s.owner.snapshots > 0 {
				s.owner.snapshots--
			}
			s.owner.mu.Unlock()
			s.owner.releaseView(v)
		}
	})
	return nil
}

// Iterator is a bounded forward iterator over merged memtable/SST sources.
type Iterator struct {
	view    *engineView
	sources []sourceCursor
	start   []byte
	end     []byte
	limit   int
	seen    int
	closed  bool
	err     error
	release bool
}

func (it *Iterator) Next() (Entry, bool) {
	if it == nil || it.closed || it.err != nil {
		return Entry{}, false
	}
	if it.limit > 0 && it.seen >= it.limit {
		_ = it.Close()
		return Entry{}, false
	}
	for {
		if it.end != nil {
			for i := range it.sources {
				if it.sources[i].have && bytes.Compare(it.sources[i].cur.key, it.end) >= 0 {
					it.sources[i].have = false
				}
			}
		}
		chosen, ok, err := nextMergedRaw(it.sources)
		if err != nil {
			it.err = err
			_ = it.Close()
			return Entry{}, false
		}
		if !ok {
			_ = it.Close()
			return Entry{}, false
		}
		if chosen.kind == deleteOp {
			continue
		}
		it.seen++
		return Entry{Key: append([]byte(nil), chosen.key...), Value: append([]byte(nil), chosen.value...)}, true
	}
}

func (it *Iterator) Close() error {
	if it == nil || it.closed {
		return nil
	}
	it.closed = true
	for i := range it.sources {
		it.sources[i].close()
	}
	if it.release && it.view != nil && it.view.owner != nil {
		it.view.owner.releaseView(it.view)
	}
	it.view = nil
	return nil
}

func newIterator(v *engineView, start, end []byte, limit int) (*Iterator, error) {
	if limit <= 0 || limit > v.opt.MaxScanItems {
		return nil, ErrInvalidLimit
	}
	return newIteratorRange(v, start, end, limit)
}

func newStreamingIterator(v *engineView, start, end []byte) (*Iterator, error) {
	return newIteratorRange(v, start, end, 0)
}

func newIteratorRange(v *engineView, start, end []byte, limit int) (*Iterator, error) {
	if err := validateRange(v.opt, start, end); err != nil {
		return nil, err
	}
	if start != nil && end != nil && bytes.Compare(start, end) >= 0 {
		return &Iterator{view: v, start: append([]byte(nil), start...), end: append([]byte(nil), end...), limit: limit}, nil
	}
	sources, err := buildSources(v, start)
	if err != nil {
		return nil, err
	}
	return &Iterator{view: v, sources: sources, start: append([]byte(nil), start...), end: append([]byte(nil), end...), limit: limit}, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func checkedAdd(a, b int) (int, bool) {
	if a < 0 || b < 0 || a > int(^uint(0)>>1)-b {
		return 0, false
	}
	return a + b, true
}

func validateKey(key []byte, max int) error {
	if len(key) == 0 || len(key) > max {
		return ErrInvalidKey
	}
	return nil
}
