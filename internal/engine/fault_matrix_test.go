package engine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// targetFaultFS is deliberately test-only. It targets the small set of
// durability boundaries by the logical path passed through the FS seam,
// rather than relying on call ordering that changes when recovery evolves.
// A failure is armed for one matching call, so the subsequent reopen can use
// the same filesystem with the injected fault consumed.
type targetFaultFS struct {
	base OSFS

	mu   sync.Mutex
	root string

	syncAt    map[string]int
	syncHits  map[string]int
	writeAt   map[string]int
	writeHits map[string]int
	writeMode map[string]string
}

var errInjectedDiskFull = errors.New("injected disk full")

func newTargetFaultFS() *targetFaultFS {
	return &targetFaultFS{
		syncAt:    make(map[string]int),
		syncHits:  make(map[string]int),
		writeAt:   make(map[string]int),
		writeHits: make(map[string]int),
		writeMode: make(map[string]string),
	}
}

func (f *targetFaultFS) rememberRoot(path string) {
	clean := filepath.Clean(path)
	if f.root == "" && filepath.Base(clean) != "wal" && filepath.Base(clean) != "sst" {
		f.root = clean
	}
}

func (f *targetFaultFS) MkdirAll(path string, perm fs.FileMode) error {
	f.mu.Lock()
	f.rememberRoot(path)
	f.mu.Unlock()
	return f.base.MkdirAll(path, perm)
}

func (f *targetFaultFS) Lstat(path string) (fs.FileInfo, error) {
	f.mu.Lock()
	f.rememberRoot(path)
	f.mu.Unlock()
	return f.base.Lstat(path)
}

func (f *targetFaultFS) ReadDir(path string) ([]fs.DirEntry, error) {
	return f.base.ReadDir(path)
}

func (f *targetFaultFS) Rename(oldPath, newPath string) error {
	return f.base.Rename(oldPath, newPath)
}

func (f *targetFaultFS) Remove(path string) error { return f.base.Remove(path) }

func (f *targetFaultFS) target(path string) string {
	clean := filepath.ToSlash(filepath.Clean(path))
	f.mu.Lock()
	root := filepath.ToSlash(f.root)
	f.mu.Unlock()
	if root != "" && clean == root {
		return "root-dir"
	}
	if strings.HasSuffix(clean, "/sst") {
		return "sst-dir"
	}
	if strings.HasSuffix(clean, "/wal") {
		return "wal-dir"
	}
	base := filepath.Base(clean)
	switch {
	case base == "CURRENT" || strings.HasPrefix(base, ".CURRENT"):
		return "current-file"
	case strings.HasPrefix(base, "MANIFEST-") || strings.HasPrefix(base, ".MANIFEST-"):
		return "manifest-file"
	case strings.HasSuffix(base, ".sst") || strings.HasSuffix(base, ".sst.tmp"):
		return "sst-file"
	case strings.HasSuffix(base, ".wal"):
		return "wal-file"
	default:
		return base
	}
}

func (f *targetFaultFS) failNextSync(target string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncAt[target] = f.syncHits[target] + 1
}

func (f *targetFaultFS) failNextWrite(target, mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeMode[target] = mode
	f.writeAt[target] = f.writeHits[target] + 1
}

func (f *targetFaultFS) syncFailure(target string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncHits[target]++
	return f.syncAt[target] == f.syncHits[target]
}

func (f *targetFaultFS) writeFailure(target string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeHits[target]++
	return f.writeMode[target], f.writeAt[target] == f.writeHits[target]
}

func (f *targetFaultFS) OpenFile(path string, flag int, perm fs.FileMode) (File, error) {
	file, err := f.base.OpenFile(path, flag, perm)
	if err != nil {
		return nil, err
	}
	return &targetFaultFile{File: file, owner: f, target: f.target(path)}, nil
}

type targetFaultFile struct {
	File
	owner  *targetFaultFS
	target string
}

func (f *targetFaultFile) WriteAt(p []byte, off int64) (int, error) {
	mode, fail := f.owner.writeFailure(f.target)
	if !fail {
		return f.File.WriteAt(p, off)
	}
	switch mode {
	case "complete":
		n, err := f.File.WriteAt(p, off)
		if err != nil {
			return n, err
		}
		return n, errInjectedDiskFull
	case "partial":
		n := len(p) / 2
		if n == 0 && len(p) > 0 {
			n = 1
		}
		written, err := f.File.WriteAt(p[:n], off)
		if err != nil {
			return written, err
		}
		return written, errInjectedDiskFull
	default:
		return 0, errInjectedDiskFull
	}
}

func (f *targetFaultFile) Sync() error {
	if f.owner.syncFailure(f.target) {
		return errInjectedDiskFull
	}
	return f.File.Sync()
}

func TestFaultMatrixBeforeManifestVisibility(t *testing.T) {
	tests := []struct {
		name       string
		target     string
		kind       string
		want       string
		wantPoison bool
	}{
		{name: "wal-write", target: "wal-file", kind: "write", want: "prior", wantPoison: true},
		{name: "wal-complete-write-error", target: "wal-file", kind: "complete-write", want: "complete", wantPoison: true},
		{name: "wal-partial-write-error", target: "wal-file", kind: "partial-write", want: "prior", wantPoison: true},
		{name: "sst-write", target: "sst-file", kind: "write", want: "complete", wantPoison: true},
		{name: "sst-sync", target: "sst-file", kind: "sync", want: "complete", wantPoison: true},
		{name: "sst-directory-sync", target: "sst-dir", kind: "sync", want: "complete", wantPoison: true},
		{name: "manifest-write", target: "manifest-file", kind: "write", want: "complete", wantPoison: true},
		{name: "manifest-sync", target: "manifest-file", kind: "sync", want: "complete", wantPoison: true},
		{name: "current-sync", target: "current-file", kind: "sync", want: "complete", wantPoison: true},
		{name: "root-directory-sync", target: "root-dir", kind: "sync", want: "complete", wantPoison: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			faults := newTargetFaultFS()
			opts := Options{FS: faults, MemtableBytes: 1, SegmentBytes: 4096, SSTBlockBytes: 1024}
			e, err := Open(dir, opts)
			if err != nil {
				t.Fatal(err)
			}
			switch tc.kind {
			case "write":
				faults.failNextWrite(tc.target, "zero")
			case "complete-write":
				faults.failNextWrite(tc.target, "complete")
			case "partial-write":
				faults.failNextWrite(tc.target, "partial")
			case "sync":
				faults.failNextSync(tc.target)
			default:
				t.Fatalf("unknown fault kind %q", tc.kind)
			}
			b := e.NewBatch()
			if err := b.Put([]byte("k"), []byte("value")); err != nil {
				t.Fatal(err)
			}
			_, applyErr := e.Apply(b)
			if tc.wantPoison && !errors.Is(applyErr, ErrPoisoned) {
				t.Fatalf("Apply error = %v, want ErrPoisoned", applyErr)
			}
			if _, err := e.Get([]byte("k")); !errors.Is(err, ErrPoisoned) {
				t.Fatalf("poisoned read error = %v", err)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}

			// Reopen uses the real production filesystem. The injected fault is
			// consumed, and recovery must expose only a complete durable record.
			reopened, err := Open(dir, Options{MemtableBytes: 1, SegmentBytes: 4096, SSTBlockBytes: 1024})
			if err != nil {
				t.Fatalf("reopen after %s: %v", tc.name, err)
			}
			defer reopened.Close()
			got, getErr := reopened.Get([]byte("k"))
			switch tc.want {
			case "complete":
				if getErr != nil || string(got) != "value" {
					t.Fatalf("reopened value = %q, %v; want complete batch", got, getErr)
				}
			case "prior":
				if !errors.Is(getErr, ErrNotFound) || got != nil {
					t.Fatalf("reopened value = %q, %v; want prior state", got, getErr)
				}
			default:
				t.Fatalf("unknown expected state %q", tc.want)
			}
		})
	}
}

func TestMissingManifestReferencedSSTIsCorruption(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	e, err := Open(dir, Options{MemtableBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	putBatch(t, e, "k", "v")
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "sst", "sst-*.sst"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("SST files = %v, %v", matches, err)
	}
	if err := os.Remove(matches[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{MemtableBytes: 1}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("missing manifest-referenced SST open = %v, want ErrCorrupt", err)
	}
}

func TestRecoveryRejectsChecksummedSemanticLSNOrdering(t *testing.T) {
	tests := []struct {
		name      string
		lsn       uint64
		conflict  bool
		wantError string
	}{
		{name: "gap", lsn: 9, wantError: "LSN gap or duplicate"},
		{name: "conflicting-duplicate", lsn: 1, conflict: true, wantError: "LSN gap or duplicate"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			opts := Options{MemtableBytes: 1 << 20, SegmentBytes: 4096}
			e, err := Open(dir, opts)
			if err != nil {
				t.Fatal(err)
			}
			putBatch(t, e, "a", "one")
			putBatch(t, e, "b", "two")
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			segment := filepath.Join(dir, "wal", segmentName(1))
			data, err := os.ReadFile(segment)
			if err != nil {
				t.Fatal(err)
			}
			firstOffset := segmentHeaderLen
			firstPayload := int(binary.BigEndian.Uint32(data[firstOffset+8 : firstOffset+12]))
			secondOffset := firstOffset + frameHeaderLen + firstPayload + frameTrailerLen
			if secondOffset+frameHeaderLen > len(data) {
				t.Fatalf("second frame offset %d outside segment size %d", secondOffset, len(data))
			}
			secondPayload := int(binary.BigEndian.Uint32(data[secondOffset+8 : secondOffset+12]))
			secondLen := frameHeaderLen + secondPayload + frameTrailerLen
			if secondOffset+secondLen > len(data) {
				t.Fatalf("second frame length %d outside segment size %d", secondLen, len(data))
			}
			binary.BigEndian.PutUint64(data[secondOffset+12:secondOffset+20], tc.lsn)
			if tc.conflict {
				// Keep the payload structurally valid but make the duplicate
				// carry a different value. Both checksums are recomputed below,
				// so recovery must reject the sequence semantics, not CRC.
				payloadOffset := secondOffset + frameHeaderLen
				keyLen := int(binary.BigEndian.Uint32(data[payloadOffset+1 : payloadOffset+5]))
				valueOffset := payloadOffset + 9 + keyLen
				data[valueOffset] = 'T'
			}
			binary.BigEndian.PutUint32(data[secondOffset+28:secondOffset+32], crc32.Checksum(data[secondOffset:secondOffset+28], walCRC))
			binary.BigEndian.PutUint32(data[secondOffset+secondLen-4:], crc32.Checksum(data[secondOffset:secondOffset+secondLen-4], walCRC))
			if err := os.WriteFile(segment, data, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = Open(dir, opts)
			if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("semantic WAL rejection = %v, want corrupt %q", err, tc.wantError)
			}
		})
	}
}

func TestFaultMatrixNeverPublishesPartialBatch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	faults := newTargetFaultFS()
	e, err := Open(dir, Options{FS: faults, MemtableBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	faults.failNextWrite("wal-file", "partial")
	b := e.NewBatch()
	if err := b.Put([]byte("partial"), bytes.Repeat([]byte{'x'}, 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(b); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("partial WAL write = %v, want ErrPoisoned", err)
	}
	if _, err := e.Get([]byte("partial")); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("read after partial WAL write = %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, Options{MemtableBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, err := reopened.Get([]byte("partial")); !errors.Is(err, ErrNotFound) || got != nil {
		t.Fatalf("partial acknowledged batch = %q, %v; want prior state", got, err)
	}
}

func TestFaultMatrixDiskFullAtManifestAndCheckpointReplaysWAL(t *testing.T) {
	for _, target := range []string{"sst-file", "manifest-file"} {
		t.Run(target, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			faults := newTargetFaultFS()
			e, err := Open(dir, Options{FS: faults, MemtableBytes: 1})
			if err != nil {
				t.Fatal(err)
			}
			faults.failNextWrite(target, "zero")
			b := e.NewBatch()
			if err := b.Put([]byte("k"), []byte("v")); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Apply(b); !errors.Is(err, ErrPoisoned) {
				t.Fatalf("%s disk-full = %v, want ErrPoisoned", target, err)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir, Options{MemtableBytes: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if got, err := reopened.Get([]byte("k")); err != nil || string(got) != "v" {
				t.Fatalf("%s replay = %q, %v; want complete durable WAL batch", target, got, err)
			}
		})
	}
}
