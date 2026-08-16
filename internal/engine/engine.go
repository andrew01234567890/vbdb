package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

type Engine struct {
	mu          syncRWMutex
	fs          rootFS
	opts        Options
	wal         *wal
	lsn         uint64
	active      map[string]memEntry
	activeBytes int
	manifest    manifest
	pins        int
	obsolete    []sstFile
	lock        File
	lockFlock   bool
	lockFile    bool
	closed      bool
	poisoned    error
	snapshots   int
}

// syncRWMutex is named to make accidental replacement with a lock that does
// not permit concurrent readers obvious in review.
type syncRWMutex = sync.RWMutex

// Open opens or creates a single-process engine rooted at dir. The default
// OSFS uses os.Root so every WAL path is descriptor-relative and cannot escape
// through a symlink substitution. Injected filesystems remain behind the same
// path validation seam for deterministic fault tests.
func Open(dir string, options Options) (*Engine, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: empty data directory", ErrInvalidDataDir)
	}
	opts, err := options.normalized()
	if err != nil {
		return nil, err
	}
	dir = filepath.Clean(dir)
	baseFS := options.FS
	if baseFS == nil {
		baseFS = OSFS{}
	}
	createdBase, err := prepareBaseDirectory(baseFS, dir)
	if err != nil {
		return nil, err
	}
	if createdBase && options.FS == nil {
		if err := syncOSDirectory(filepath.Dir(dir)); err != nil {
			return nil, err
		}
	}
	fsys := rootFS{base: dir, fs: baseFS}
	if options.FS == nil {
		before, statErr := baseFS.Lstat(dir)
		if statErr != nil {
			return nil, fmt.Errorf("%w: stat data directory before root: %v", ErrFilesystem, statErr)
		}
		root, rootErr := os.OpenRoot(dir)
		if rootErr != nil {
			return nil, fmt.Errorf("%w: open descriptor root: %v", ErrFilesystem, rootErr)
		}
		after, statErr := root.Stat(".")
		if statErr != nil || !os.SameFile(before, after) || after.Mode().Perm()&0o077 != 0 {
			_ = root.Close()
			if statErr != nil {
				return nil, fmt.Errorf("%w: stat data directory after root: %v", ErrFilesystem, statErr)
			}
			return nil, ErrInvalidDataDir
		}
		fsys.root = root
	}
	if err := ensureDirectory(fsys, "wal"); err != nil {
		_ = fsys.closeRoot()
		return nil, err
	}
	if err := syncDirectory(fsys, "."); err != nil {
		_ = fsys.closeRoot()
		return nil, err
	}
	lock, flock, created, err := acquireLock(fsys)
	if err != nil {
		_ = fsys.closeRoot()
		return nil, err
	}
	m, err := openManifest(fsys, opts)
	if err != nil {
		releaseLock(fsys, lock, flock, created)
		_ = fsys.closeRoot()
		return nil, err
	}
	w, recovered, lsn, err := openWAL(fsys, opts, emptyState(), m.flushedLSN)
	if err != nil {
		releaseLock(fsys, lock, flock, created)
		_ = fsys.closeRoot()
		return nil, err
	}
	active := entriesMap(recovered)
	e := &Engine{fs: fsys, opts: opts, wal: w, lsn: lsn, lock: lock, lockFlock: flock, lockFile: created, active: active, activeBytes: memtableBytes(active), manifest: m}
	// WAL records newer than the last manifest checkpoint are already durable,
	// but must be incorporated into an immutable checkpoint before old WAL can
	// be reclaimed. Checkpointing during Open also gives pre-LSM directories a
	// safe one-time conversion path without changing their visible contents.
	if len(e.active) > 0 {
		if err := e.checkpoint(true); err != nil {
			_ = w.close()
			releaseLock(fsys, lock, flock, created)
			_ = fsys.closeRoot()
			return nil, err
		}
	}
	return e, nil
}

func prepareBaseDirectory(fsys FS, dir string) (bool, error) {
	info, err := fsys.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := fsys.MkdirAll(dir, 0o700); err != nil {
			return false, fmt.Errorf("%w: create data directory: %v", ErrFilesystem, err)
		}
		info, err = fsys.Lstat(dir)
		if err == nil {
			created := true
			if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
				return created, ErrInvalidDataDir
			}
			return created, nil
		}
	}
	if err != nil {
		return false, fmt.Errorf("%w: stat data directory: %v", ErrFilesystem, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return false, ErrInvalidDataDir
	}
	return false, nil
}

func acquireLock(fsys rootFS) (File, bool, bool, error) {
	// Linux OSFS returns a Lockable file and deliberately keeps the persistent
	// LOCK inode. Filesystems without a process-lock primitive use an atomic
	// create as a presence lock; probing with O_CREATE first would create the
	// inode and make the subsequent O_EXCL call fail on every fresh open.
	file, err := fsys.OpenFile("LOCK", os.O_RDWR, 0o600)
	if err == nil {
		if lockable, ok := file.(Lockable); ok {
			if err := lockable.TryLock(); err != nil {
				_ = file.Close()
				if errors.Is(err, os.ErrExist) || errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES) {
					return nil, false, false, ErrLocked
				}
				return nil, false, false, fmt.Errorf("%w: lock: %v", ErrFilesystem, err)
			}
			return file, true, false, nil
		}
		_ = file.Close()
		return nil, false, false, ErrLocked
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, false, fmt.Errorf("%w: probe lock: %v", ErrFilesystem, err)
	}
	file, err = fsys.OpenFile("LOCK", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, false, false, ErrLocked
		}
		return nil, false, false, fmt.Errorf("%w: create lock: %v", ErrFilesystem, err)
	}
	if lockable, ok := file.(Lockable); ok {
		if err := lockable.TryLock(); err != nil {
			_ = file.Close()
			_ = fsys.Remove("LOCK")
			if errors.Is(err, os.ErrExist) || errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES) {
				return nil, false, false, ErrLocked
			}
			return nil, false, false, fmt.Errorf("%w: lock: %v", ErrFilesystem, err)
		}
		return file, true, false, nil
	}
	return file, false, true, nil
}

func releaseLock(fsys rootFS, file File, flock, created bool) error {
	if file == nil {
		return nil
	}
	var first error
	if flock {
		if lockable, ok := file.(Lockable); ok {
			if err := lockable.Unlock(); err != nil {
				first = err
			}
		}
	}
	if err := file.Close(); err != nil && first == nil {
		first = err
	}
	if created {
		if err := fsys.Remove("LOCK"); err != nil && !errors.Is(err, fs.ErrNotExist) && first == nil {
			first = err
		}
	}
	return first
}

func (e *Engine) stateErr() error {
	if e.closed {
		return ErrClosed
	}
	if e.poisoned != nil {
		return e.poisoned
	}
	return nil
}

func poisonError(err error) error {
	if errors.Is(err, ErrPoisoned) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrPoisoned, err)
}

// Apply durably commits one canonical atomic batch and returns the new local
// physical LSN. Both Batch and *Batch are accepted to keep the seam ergonomic
// for callers that use either value-style or builder-style batches.
func (e *Engine) Apply(input any) (uint64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.stateErr(); err != nil {
		return 0, err
	}
	batch, err := batchValue(input)
	if err != nil {
		return 0, err
	}
	if e.lsn == ^uint64(0) {
		return 0, fmt.Errorf("%w: LSN exhausted", ErrPoisoned)
	}
	ops, _, err := canonicalOperations(batch, e.opts)
	if err != nil {
		return 0, err
	}
	/*
		The WAL append is the durability boundary. Work out the new memtable
		before appending, but do not publish it until append+Sync succeeds.
	*/
	activeCopy := cloneMemtable(e.active)
	activeBytes := e.activeBytes
	if err := applyToMemtable(activeCopy, &activeBytes, ops, e.opts); err != nil {
		return 0, err
	}
	nextLSN := e.lsn + 1
	frame, _, err := encodeFrame(nextLSN, batch, e.opts)
	if err != nil {
		return 0, err
	}
	if err := e.wal.appendFrame(frame, nextLSN); err != nil {
		e.poisoned = poisonError(err)
		return 0, e.poisoned
	}
	// This publication is deliberately the final operation after WAL Sync.
	e.lsn = nextLSN
	e.active = activeCopy
	e.activeBytes = activeBytes
	if b, ok := input.(*Batch); ok {
		b.sealed = true
	}
	if e.activeBytes >= e.opts.MemtableBytes {
		if err := e.flushActive(); err != nil {
			e.poisoned = poisonError(err)
			return 0, e.poisoned
		}
	}
	return nextLSN, nil
}

func batchValue(input any) (*Batch, error) {
	switch batch := input.(type) {
	case Batch:
		copy := batch
		return &copy, nil
	case *Batch:
		if batch == nil {
			return nil, ErrInvalidBatch
		}
		return batch, nil
	default:
		return nil, fmt.Errorf("%w: Apply expects Batch or *Batch", ErrInvalidBatch)
	}
}

// Get returns a private value copy from the current durable state.
func (e *Engine) Get(key []byte) ([]byte, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if err := e.stateErr(); err != nil {
		return nil, err
	}
	if err := validateKey(key, e.opts.MaxKeyBytes); err != nil {
		return nil, err
	}
	return lookupView(e.newViewLocked(), key)
}

// Scan returns at most limit entries in [start,end). A nil bound is open.
func (e *Engine) Scan(start, end []byte, limit int) ([]Entry, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if err := e.stateErr(); err != nil {
		return nil, err
	}
	view := e.newViewLocked()
	it, err := newIterator(view, start, end, limit)
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

// Iterator returns a bounded forward iterator over a stable snapshot.
func (e *Engine) Iterator(start, end []byte, limit int) (*Iterator, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.stateErr(); err != nil {
		return nil, err
	}
	view := e.newViewLocked()
	e.pinViewLocked(view)
	it, err := newIterator(view, start, end, limit)
	if err != nil {
		e.pins--
		return nil, err
	}
	it.release = true
	return it, nil
}

// Iterate returns a streaming iterator over the complete bounded key range.
// It is intended for integrity scans and backup/export paths that cannot use
// the bounded request-oriented Iterator API.
func (e *Engine) Iterate(start, end []byte) (*Iterator, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.stateErr(); err != nil {
		return nil, err
	}
	view := e.newViewLocked()
	e.pinViewLocked(view)
	it, err := newStreamingIterator(view, start, end)
	if err != nil {
		e.pins--
		return nil, err
	}
	it.release = true
	return it, nil
}

// Last returns the greatest key in [start,end). A nil bound is open.
func (e *Engine) Last(start, end []byte) (Entry, error) {
	it, err := e.Iterate(start, end)
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
	if err := it.err; err != nil {
		return Entry{}, err
	}
	if !found {
		return Entry{}, ErrNotFound
	}
	return last, nil
}

// Snapshot acquires a stable, immutable state view. Active snapshots are
// bounded so retaining old views cannot create an unbounded memory surface.
func (e *Engine) Snapshot() (*Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.stateErr(); err != nil {
		return nil, err
	}
	if e.snapshots >= e.opts.MaxSnapshots {
		return nil, ErrTooManySnapshot
	}
	e.snapshots++
	view := e.newViewLocked()
	e.pinViewLocked(view)
	return &Snapshot{owner: e, view: view, lsn: e.lsn}, nil
}

// LSN returns the latest durable local physical LSN.
func (e *Engine) LSN() (uint64, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if err := e.stateErr(); err != nil {
		return 0, err
	}
	return e.lsn, nil
}

// Close releases the process lock and closes the WAL. It is idempotent.
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	walErr := e.wal.close()
	lockErr := releaseLock(e.fs, e.lock, e.lockFlock, e.lockFile)
	rootErr := e.fs.closeRoot()
	e.mu.Unlock()
	if walErr != nil {
		return fmt.Errorf("%w: close WAL: %v", ErrFilesystem, walErr)
	}
	if lockErr != nil {
		return fmt.Errorf("%w: close lock: %v", ErrFilesystem, lockErr)
	}
	if rootErr != nil {
		return fmt.Errorf("%w: close root: %v", ErrFilesystem, rootErr)
	}
	return nil
}
