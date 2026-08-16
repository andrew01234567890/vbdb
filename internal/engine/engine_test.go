package engine

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func putBatch(t *testing.T, e *Engine, key, value string) uint64 {
	t.Helper()
	b := e.NewBatch()
	if err := b.Put([]byte(key), []byte(value)); err != nil {
		t.Fatal(err)
	}
	lsn, err := e.Apply(&b)
	if err != nil {
		t.Fatal(err)
	}
	return lsn
}

func TestAtomicOrderedSnapshotAndReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	e, err := Open(dir, Options{SegmentBytes: 256})
	if err != nil {
		t.Fatal(err)
	}
	b := e.NewBatch()
	if err := b.Put([]byte("b"), []byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := b.Put([]byte("a"), []byte("one")); err != nil {
		t.Fatal(err)
	}
	lsn, err := e.Apply(b)
	if err != nil || lsn != 1 {
		t.Fatalf("Apply = %d, %v", lsn, err)
	}
	snap, err := e.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if got, err := snap.Get([]byte("a")); err != nil || string(got) != "one" {
		t.Fatalf("snapshot a = %q, %v", got, err)
	}
	deleteBatch := e.NewBatch()
	if err := deleteBatch.Delete([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(deleteBatch); err != nil {
		t.Fatal(err)
	}
	if got, err := snap.Get([]byte("a")); err != nil || string(got) != "one" {
		t.Fatalf("old snapshot after delete = %q, %v", got, err)
	}
	entries, err := e.Scan(nil, nil, 4)
	if err != nil || len(entries) != 1 || string(entries[0].Key) != "b" {
		t.Fatalf("Scan = %#v, %v", entries, err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = Open(dir, Options{SegmentBytes: 256})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if got, err := e.Get([]byte("a")); !errors.Is(err, ErrNotFound) || got != nil {
		t.Fatalf("deleted a = %q, %v", got, err)
	}
	if got, err := e.Get([]byte("b")); err != nil || string(got) != "two" {
		t.Fatalf("reopened b = %q, %v", got, err)
	}
	if lsn, err := e.LSN(); err != nil || lsn != 2 {
		t.Fatalf("reopened LSN = %d, %v", lsn, err)
	}
}

func TestExclusiveLockAndOwnerOnlyDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	e, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{}); !errors.Is(err, ErrLocked) {
		t.Fatalf("second open = %v, want ErrLocked", err)
	}
	if got := mustMode(t, dir); got&0o077 != 0 {
		t.Fatalf("data directory mode %04o is not owner-only", got)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = Open(dir, Options{})
	if err != nil {
		t.Fatalf("reopen after close = %v", err)
	}
	_ = e.Close()
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{}); !errors.Is(err, ErrInvalidDataDir) {
		t.Fatalf("insecure open = %v", err)
	}
}

func TestBatchBoundsAndCallerCopies(t *testing.T) {
	e, err := Open(filepath.Join(t.TempDir(), "data"), Options{MaxKeyBytes: 4, MaxValueBytes: 4, MaxBatchBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	b := e.NewBatch()
	key := []byte("key")
	value := []byte("value")
	if err := b.Put(key, value); !errors.Is(err, ErrValueTooLarge) {
		t.Fatalf("oversized value = %v", err)
	}
	value = []byte("good")
	if err := b.Put(key, value); err != nil {
		t.Fatal(err)
	}
	key[0] = 'X'
	value[0] = 'X'
	if _, err := e.Apply(&b); err != nil {
		t.Fatal(err)
	}
	got, err := e.Get([]byte("key"))
	if err != nil || string(got) != "good" {
		t.Fatalf("caller copy = %q, %v", got, err)
	}
	if err := b.Delete([]byte("key")); !errors.Is(err, ErrInvalidBatch) {
		t.Fatalf("sealed batch reuse = %v", err)
	}
}

type faultFS struct {
	base       OSFS
	short      atomic.Bool
	zero       atomic.Bool
	failSync   atomic.Bool
	writeOnce  atomic.Bool
	renameN    atomic.Int32
	failRename atomic.Int32
}

func (f *faultFS) MkdirAll(path string, perm fs.FileMode) error { return f.base.MkdirAll(path, perm) }
func (f *faultFS) Lstat(path string) (fs.FileInfo, error)       { return f.base.Lstat(path) }
func (f *faultFS) ReadDir(path string) ([]fs.DirEntry, error)   { return f.base.ReadDir(path) }
func (f *faultFS) Rename(oldPath, newPath string) error {
	if n := f.failRename.Load(); n > 0 && f.renameN.Add(1) == n {
		return errors.New("injected rename failure")
	}
	return f.base.Rename(oldPath, newPath)
}
func (f *faultFS) Remove(path string) error { return f.base.Remove(path) }
func (f *faultFS) OpenFile(path string, flag int, perm fs.FileMode) (File, error) {
	file, err := f.base.OpenFile(path, flag, perm)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ".wal") {
		return &faultFile{File: file, owner: f}, nil
	}
	return file, nil
}

type faultFile struct {
	File
	owner *faultFS
}

// nonLockingFS deliberately hides the concrete Linux flock implementation so
// the presence-lock fallback is exercised on every test host.
type nonLockingFS struct{ FS }

type nonLockingFile struct{ File }

func (f nonLockingFS) OpenFile(path string, flag int, perm fs.FileMode) (File, error) {
	file, err := f.FS.OpenFile(path, flag, perm)
	if err != nil {
		return nil, err
	}
	return nonLockingFile{File: file}, nil
}

func TestNonLockablePresenceLockLifecycle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	filesystem := nonLockingFS{FS: OSFS{}}
	options := Options{FS: filesystem}
	e, err := Open(dir, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, options); !errors.Is(err, ErrLocked) {
		t.Fatalf("second non-lockable open = %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = Open(dir, options)
	if err != nil {
		t.Fatalf("reopen after clean close = %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	// A stale presence lock remains fail-closed until an operator removes it.
	stale, err := os.OpenFile(filepath.Join(dir, "LOCK"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, options); !errors.Is(err, ErrLocked) {
		t.Fatalf("stale non-lockable open = %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "LOCK")); err != nil {
		t.Fatal(err)
	}
}

type noSyncFS struct{ FS }

type noSyncFile struct{ File }

func (f noSyncFile) Sync() error { return nil }

func (f noSyncFS) OpenFile(path string, flag int, perm fs.FileMode) (File, error) {
	file, err := f.FS.OpenFile(path, flag, perm)
	if err != nil {
		return nil, err
	}
	return noSyncFile{File: file}, nil
}

func (f *faultFile) WriteAt(p []byte, off int64) (int, error) {
	if f.owner.zero.Load() {
		return 0, nil
	}
	if f.owner.short.Load() && !f.owner.writeOnce.Swap(true) && len(p) > 1 {
		return 1, nil
	}
	return f.File.WriteAt(p, off)
}

func (f *faultFile) Sync() error {
	if f.owner.failSync.Load() {
		return errors.New("injected sync failure")
	}
	return f.File.Sync()
}

func TestShortWriteAndUncertainSyncPoison(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	faults := &faultFS{}
	e, err := Open(dir, Options{FS: faults, SegmentBytes: 256})
	if err != nil {
		t.Fatal(err)
	}
	faults.short.Store(true)
	put := e.NewBatch()
	if err := put.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(put); err != nil {
		t.Fatalf("short write should be retried: %v", err)
	}
	faults.failSync.Store(true)
	put = e.NewBatch()
	if err := put.Put([]byte("poison"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(put); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("sync error = %v", err)
	}
	if _, err := e.Get([]byte("k")); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("read after sync error = %v", err)
	}
	_ = e.Close()
}

func TestConcurrentSnapshotsAndApply(t *testing.T) {
	e, err := Open(filepath.Join(t.TempDir(), "data"), Options{MaxSnapshots: 128})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				b := e.NewBatch()
				key := []byte{byte(i), byte(j)}
				if err := b.Put(key, []byte("v")); err != nil {
					t.Errorf("Put: %v", err)
					return
				}
				if _, err := e.Apply(b); err != nil {
					t.Errorf("Apply: %v", err)
					return
				}
				s, err := e.Snapshot()
				if err != nil {
					t.Errorf("Snapshot: %v", err)
					return
				}
				if _, err := s.Scan(nil, nil, 1); err != nil {
					t.Errorf("Scan: %v", err)
				}
				_ = s.Close()
			}
		}(i)
	}
	wg.Wait()
	if lsn, err := e.LSN(); err != nil || lsn != 160 {
		t.Fatalf("final LSN = %d, %v", lsn, err)
	}
}

func TestRecoveryTruncatesOnlyIncompleteFinalTail(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	e, err := Open(dir, Options{SegmentBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	putBatch(t, e, "a", "one")
	putBatch(t, e, "b", "two")
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	segments, err := filepath.Glob(filepath.Join(dir, "wal", "segment-*.wal"))
	if err != nil || len(segments) != 1 {
		t.Fatalf("segments = %v, %v", segments, err)
	}
	f, err := os.OpenFile(segments[0], os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0x56, 0x42, 0x46}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = Open(dir, Options{SegmentBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if got, err := e.Get([]byte("a")); err != nil || string(got) != "one" {
		t.Fatalf("prefix a = %q, %v", got, err)
	}
	if got, err := e.Get([]byte("b")); err != nil || string(got) != "two" {
		t.Fatalf("prefix b = %q, %v", got, err)
	}
}

func TestRecoveryFailsCompleteCorruption(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	e, err := Open(dir, Options{SegmentBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	putBatch(t, e, "a", "one")
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	segment := filepath.Join(dir, "wal", segmentName(1))
	f, err := os.OpenFile(segment, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The frame header begins after the segment header. A complete header
	// mutation must fail, even when the file is the final segment.
	if _, err := f.WriteAt([]byte{0xff}, segmentHeaderLen+8); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{SegmentBytes: 4096}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt final frame open = %v", err)
	}
}

func TestZeroWritePoisonsEngine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	faults := &faultFS{}
	e, err := Open(dir, Options{FS: faults})
	if err != nil {
		t.Fatal(err)
	}
	faults.zero.Store(true)
	b := e.NewBatch()
	if err := b.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(b); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("zero write = %v", err)
	}
	_ = e.Close()
}

func TestLSMFlushCompactionAndReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	options := Options{MemtableBytes: 1, SSTBlockBytes: 1024, SegmentBytes: 4096}
	e, err := Open(dir, options)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		putBatch(t, e, string(rune('a'+i)), string(rune('0'+i)))
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = Open(dir, options)
	if err != nil {
		t.Fatalf("reopen after SST flush: %v", err)
	}
	defer e.Close()
	for i := 0; i < 6; i++ {
		key := string(rune('a' + i))
		got, err := e.Get([]byte(key))
		if err != nil || string(got) != string(rune('0'+i)) {
			t.Fatalf("reopened %q = %q, %v", key, got, err)
		}
	}
	last, err := e.Last(nil, nil)
	if err != nil || string(last.Key) != "f" {
		t.Fatalf("Last = %#v, %v", last, err)
	}
	it, err := e.Iterate(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for {
		_, ok := it.Next()
		if !ok {
			break
		}
		count++
		if count > 100 {
			t.Fatal("streaming iterator did not terminate")
		}
	}
	if err := it.Close(); err != nil || count != 6 {
		t.Fatalf("iterator count/close = %d, %v", count, err)
	}
	stream, err := e.Stream(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	streamCount := 0
	for {
		_, ok, nextErr := stream.Next()
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		if !ok {
			break
		}
		streamCount++
	}
	if stream.Err() != nil || streamCount != 6 {
		t.Fatalf("stream count/error = %d, %v", streamCount, stream.Err())
	}
	_ = stream.Close()
}

func TestLSMFlushesOneOversizedBoundedValue(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	e, err := Open(dir, Options{MemtableBytes: 1, MaxValueBytes: 100_000, SSTBlockBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	value := bytes.Repeat([]byte{'x'}, 50_000)
	b := e.NewBatch()
	if err := b.Put([]byte("large"), value); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(b); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = Open(dir, Options{MemtableBytes: 1, MaxValueBytes: 100_000, SSTBlockBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	got, err := e.Get([]byte("large"))
	if err != nil || !bytes.Equal(got, value) {
		t.Fatalf("large value = %d bytes, %v", len(got), err)
	}
}

func TestLSMRejectsCorruptManifestAndSST(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, dir string)
	}{
		{name: "manifest", mutate: func(t *testing.T, dir string) {
			path := filepath.Join(dir, "CURRENT")
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteAt([]byte{0xff}, 0); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
		}},
		{name: "sst", mutate: func(t *testing.T, dir string) {
			matches, err := filepath.Glob(filepath.Join(dir, "sst", "sst-*.sst"))
			if err != nil || len(matches) == 0 {
				t.Fatalf("SST files = %v, %v", matches, err)
			}
			f, err := os.OpenFile(matches[0], os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteAt([]byte{0xff}, 8); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
		}},
		{name: "sst-tail", mutate: func(t *testing.T, dir string) {
			matches, err := filepath.Glob(filepath.Join(dir, "sst", "sst-*.sst"))
			if err != nil || len(matches) == 0 {
				t.Fatalf("SST files = %v, %v", matches, err)
			}
			f, err := os.OpenFile(matches[0], os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			info, err := f.Stat()
			if err != nil || info.Size() < 2 {
				t.Fatalf("SST stat = %v, %v", info, err)
			}
			if err := f.Truncate(info.Size() - 1); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			e, err := Open(dir, Options{MemtableBytes: 1})
			if err != nil {
				t.Fatal(err)
			}
			putBatch(t, e, "k", "v")
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, dir)
			if _, err := Open(dir, Options{MemtableBytes: 1}); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("corrupt %s open = %v", tc.name, err)
			}
		})
	}
}

func TestManifestPublicationFailureReplaysDurableWAL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	faults := &faultFS{}
	e, err := Open(dir, Options{FS: faults, MemtableBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	// The first rename publishes the SST. The second is the manifest rename;
	// CURRENT is therefore never advanced when this fault fires.
	faults.failRename.Store(2)
	b := e.NewBatch()
	if err := b.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(b); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("manifest rename failure = %v", err)
	}
	_ = e.Close()
	e, err = Open(dir, Options{FS: &faultFS{}, MemtableBytes: 1})
	if err != nil {
		t.Fatalf("reopen after manifest fault: %v", err)
	}
	defer e.Close()
	if got, err := e.Get([]byte("k")); err != nil || string(got) != "v" {
		t.Fatalf("replayed value = %q, %v", got, err)
	}
}

func TestMissingCurrentRecoversHighestManifest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	faults := &faultFS{}
	e, err := Open(dir, Options{FS: faults, MemtableBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	faults.failRename.Store(3) // leave the durable manifest without CURRENT
	b := e.NewBatch()
	if err := b.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(b); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("CURRENT rename failure = %v", err)
	}
	_ = e.Close()
	e, err = Open(dir, Options{FS: &faultFS{}, MemtableBytes: 1})
	if err != nil {
		t.Fatalf("reopen without CURRENT: %v", err)
	}
	defer e.Close()
	if got, err := e.Get([]byte("k")); err != nil || string(got) != "v" {
		t.Fatalf("recovered value = %q, %v", got, err)
	}
}

func TestLSMPartitionsL1AndPreservesTombstonePrecedence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	options := Options{MemtableBytes: 1, MaxKeyBytes: 32, MaxValueBytes: 512, SSTBlockBytes: 1024, MaxSSTBytes: 2048, MaxSSTFiles: 64, SegmentBytes: 4096}
	e, err := Open(dir, options)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 180; i++ {
		key := fmt.Sprintf("k-%03d", i)
		putBatch(t, e, key, "value")
	}
	// Force a newer L0 tombstone over an older L1 value.
	delete := e.NewBatch()
	if err := delete.Delete([]byte("k-090")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(delete); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Get([]byte("k-090")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tombstone Get = %v", err)
	}
	if entries, err := e.Scan([]byte("k-089"), []byte("k-092"), 8); err != nil || len(entries) != 2 || string(entries[0].Key) != "k-089" || string(entries[1].Key) != "k-091" {
		t.Fatalf("tombstone Scan = %#v, %v", entries, err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "sst", "sst-*.sst"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) < 2 {
		t.Fatalf("L1 was not partitioned: %d SSTs", len(matches))
	}
	e, err = Open(dir, options)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Get([]byte("k-090")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reopened tombstone Get = %v", err)
	}
}

func TestSnapshotPinsObsoleteSSTUntilClose(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	options := Options{MemtableBytes: 1, MaxKeyBytes: 32, MaxValueBytes: 512, SSTBlockBytes: 1024, MaxSSTBytes: 4096, MaxSSTFiles: 64, SegmentBytes: 4096}
	e, err := Open(dir, options)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		putBatch(t, e, fmt.Sprintf("k-%02d", i), "old")
	}
	snapshot, err := e.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		putBatch(t, e, fmt.Sprintf("k-%02d", i), "new")
	}
	if got, err := snapshot.Get([]byte("k-03")); err != nil || string(got) != "old" {
		t.Fatalf("snapshot before compaction = %q, %v", got, err)
	}
	oldFiles, err := filepath.Glob(filepath.Join(dir, "sst", "sst-*.sst"))
	if err != nil || len(oldFiles) == 0 {
		t.Fatalf("old SST files = %v, %v", oldFiles, err)
	}
	// More writes force a compaction while the snapshot is pinned.
	for i := 8; i < 16; i++ {
		putBatch(t, e, fmt.Sprintf("k-%02d", i), "new")
	}
	if got, err := snapshot.Get([]byte("k-03")); err != nil || string(got) != "old" {
		t.Fatalf("snapshot across compaction = %q, %v", got, err)
	}
	for _, path := range oldFiles {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("pinned SST removed too early (%s): %v", path, err)
		}
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCompactionCapacityFailureLeavesReopenableManifest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	options := Options{MemtableBytes: 1, MaxKeyBytes: 32, MaxValueBytes: 1000, SSTBlockBytes: 1024, MaxSSTBytes: 4096, MaxSSTFiles: 5, SegmentBytes: 4096}
	e, err := Open(dir, options)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		putBatch(t, e, fmt.Sprintf("k-%02d", i), strings.Repeat("v", 1000))
	}
	b := e.NewBatch()
	if err := b.Put([]byte("k-03"), []byte(strings.Repeat("v", 1000))); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(b); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("capacity failure = %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = Open(dir, options)
	if err != nil {
		t.Fatalf("reopen after capacity failure = %v", err)
	}
	defer e.Close()
	for i := 0; i < 4; i++ {
		if got, err := e.Get([]byte(fmt.Sprintf("k-%02d", i))); err != nil || len(got) != 1000 {
			t.Fatalf("reopened k-%02d = %d bytes, %v", i, len(got), err)
		}
	}
}

func TestCompactionTriggerRequiresReservedManifestSlots(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "data"), Options{MaxSSTFiles: l0CompactionTrigger}); !errors.Is(err, ErrInvalidBatch) {
		t.Fatalf("MaxSSTFiles at trigger = %v", err)
	}
}

func TestObsoleteCleanupToleratesPartialPriorCleanup(t *testing.T) {
	e, err := Open(filepath.Join(t.TempDir(), "data"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.mu.Lock()
	e.obsolete = []sstFile{{name: "sst-00000000000000000001.sst"}}
	err = e.cleanupObsoleteLocked()
	e.mu.Unlock()
	if err != nil {
		t.Fatalf("cleanup missing obsolete file = %v", err)
	}
}

func TestCompactionWithSmallBatchLimitExceedsBatchCountCoupling(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	options := Options{
		FS:            noSyncFS{FS: OSFS{}},
		MaxBatchOps:   4,
		MaxKeyBytes:   32,
		MaxValueBytes: 4,
		MemtableBytes: 32 << 10,
		SSTBlockBytes: 1024,
		MaxSSTBytes:   2 << 20,
		MaxSSTFiles:   64,
		SegmentBytes:  64 << 10,
	}
	e, err := Open(dir, options)
	if err != nil {
		t.Fatal(err)
	}
	// Each operation is an independent one-op batch. The third compaction
	// merges more than maxManifestFiles*MaxBatchOps physical records, which
	// used to be rejected by an unrelated SST entry-count guard.
	records := maxManifestFiles*options.MaxBatchOps + 6000
	for i := 0; i < records; i++ {
		b := e.NewBatch()
		if err := b.Put([]byte(fmt.Sprintf("k-%05d", i)), []byte("v")); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Apply(b); err != nil {
			t.Fatalf("apply %d/%d: %v", i, records, err)
		}
	}
	if got, err := e.Get([]byte(fmt.Sprintf("k-%05d", records-1))); err != nil || string(got) != "v" {
		t.Fatalf("latest value = %q, %v", got, err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = Open(dir, options)
	if err != nil {
		t.Fatalf("reopen after large compaction = %v", err)
	}
	defer e.Close()
	if got, err := e.Get([]byte("k-00000")); err != nil || string(got) != "v" {
		t.Fatalf("reopened first value = %q, %v", got, err)
	}
}

func mustMode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
