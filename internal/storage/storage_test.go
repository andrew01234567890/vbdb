package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrew01234567890/vbdb/internal/engine"
	"github.com/andrew01234567890/vbdb/pkg/codec"
	"github.com/andrew01234567890/vbdb/pkg/uuidv7"
)

func testGenerator() UUIDGenerator {
	var n byte
	return func() (uuidv7.UUID, error) {
		n++
		return uuidv7.Generator{
			Now:  func() time.Time { return time.UnixMilli(100) },
			Rand: bytes.NewReader(bytes.Repeat([]byte{n}, 10)),
		}.New()
	}
}

func rawEngineOpen(t *testing.T, dir string) *engine.Engine {
	t.Helper()
	db, err := engine.Open(dir, engine.Options{
		MaxKeyBytes:   maxHistoryKeyBytes,
		MaxValueBytes: maxRowRecordBytes,
		MaxBatchOps:   4,
		MaxBatchBytes: maxStorageBatchBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func rawSet(t *testing.T, db *engine.Engine, key, value []byte) {
	t.Helper()
	batch := db.NewBatch()
	if err := batch.Put(key, value); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Apply(&batch); err != nil {
		t.Fatal(err)
	}
}

func rawDelete(t *testing.T, db *engine.Engine, key []byte) {
	t.Helper()
	batch := db.NewBatch()
	if err := batch.Delete(key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Apply(&batch); err != nil {
		t.Fatal(err)
	}
}

func TestPutHistoryConditionsAndReopen(t *testing.T) {
	dir := t.TempDir()
	engine, err := Open(filepath.Join(dir, "data"), Options{UUIDGenerator: testGenerator()})
	if err != nil {
		t.Fatal(err)
	}
	first, err := engine.Put("users", "alice", []byte(`{"n":1}`), Condition{})
	if err != nil || !first.Created || first.Row.Sequence != 1 {
		t.Fatalf("first Put = %#v, %v", first, err)
	}
	if _, err := engine.Put("users", "alice", []byte(`{"n":2}`), Condition{CreateOnly: true}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("create-only existing error = %v", err)
	}
	if sequence, err := engine.Sequence(); err != nil || sequence != 1 {
		t.Fatalf("sequence after failed write = %d, %v", sequence, err)
	}
	second, err := engine.Put("users", "alice", []byte(`{"n":2}`), Condition{IfMatch: &first.Row.Version})
	if err != nil || second.Created || second.Row.Sequence != 2 {
		t.Fatalf("conditional Put = %#v, %v", second, err)
	}
	if _, err := engine.Put("users", "alice", []byte(`{"n":3}`), Condition{IfMatch: &first.Row.Version}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("stale If-Match error = %v", err)
	}
	historical, err := engine.GetAt("users", "alice", first.Row.Sequence)
	if err != nil || !bytes.Equal(historical.Value, first.Row.Value) {
		t.Fatalf("historical read = %#v, %v", historical, err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Get("users", "alice"); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Get = %v", err)
	}
	reopened, err := Open(filepath.Join(dir, "data"), Options{UUIDGenerator: testGenerator()})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	row, err := reopened.Get("users", "alice")
	if err != nil || row.Sequence != 2 || !bytes.Equal(row.Value, second.Row.Value) {
		t.Fatalf("reopened row = %#v, %v", row, err)
	}
	third, err := reopened.Put("users", "bob", []byte(`null`), Condition{})
	if err != nil || third.Row.Sequence != 3 {
		t.Fatalf("next sequence = %#v, %v", third, err)
	}
}

func TestOpenEnforcesOwnerOnlyDataDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix directory permission semantics")
	}
	dir := filepath.Join(t.TempDir(), "data")
	engine, err := Open(dir, Options{UUIDGenerator: testGenerator()})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("new data directory mode = %04o, want 0700", got)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{UUIDGenerator: testGenerator()}); !errors.Is(err, ErrInsecureDataDir) {
		t.Fatalf("Open group-readable directory = %v, want ErrInsecureDataDir", err)
	}
}

func TestOpenRejectsSymlinkDataDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink creation may require elevated privileges")
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, dir + string(filepath.Separator)} {
		if _, err := Open(path, Options{UUIDGenerator: testGenerator()}); !errors.Is(err, ErrInsecureDataDir) {
			t.Fatalf("Open symlink data directory %q = %v, want ErrInsecureDataDir", path, err)
		}
	}
}

func TestReturnableCommitErrorTerminatesTheEngine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	faultFS := &failSyncFS{FS: engine.OSFS{}}
	rowEngine, err := Open(dir, Options{UUIDGenerator: testGenerator(), engineOptions: &engine.Options{FS: faultFS}})
	if err != nil {
		t.Fatal(err)
	}
	faultFS.fail.Store(true)
	_, err = rowEngine.Put("users", "terminal", []byte(`true`), Condition{})
	if err == nil || !errors.Is(err, ErrTerminal) {
		t.Fatalf("returnable commit error = %v, want ErrTerminal", err)
	}
	if _, err := rowEngine.Sequence(); err == nil || !errors.Is(err, ErrTerminal) {
		t.Fatalf("Sequence after terminal commit error = %v, want ErrTerminal", err)
	}
	if _, err := rowEngine.Get("users", "terminal"); err == nil || !errors.Is(err, ErrTerminal) {
		t.Fatalf("Get after terminal commit error = %v, want ErrTerminal", err)
	}
	if err := rowEngine.Close(); err != nil {
		t.Fatalf("terminal Close = %v, want idempotent success", err)
	}
	// A failed sync may leave either no frame or a complete frame in the
	// filesystem, but recovery must never expose a partial four-record Put.
	reopened, err := Open(dir, Options{UUIDGenerator: testGenerator()})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	sequence, err := reopened.Sequence()
	if err != nil {
		t.Fatal(err)
	}
	row, rowErr := reopened.Get("users", "terminal")
	if sequence == 0 {
		if !errors.Is(rowErr, ErrNotFound) {
			t.Fatalf("failed sync absent row error = %v", rowErr)
		}
	} else if sequence != 1 || rowErr != nil || row.Sequence != 1 {
		t.Fatalf("failed sync partial state = sequence %d row=%#v err=%v", sequence, row, rowErr)
	}
	physicalKeys := map[string][]byte{
		"sequence": mustSequenceKey(),
		"head":     mustHeadKey("users", "terminal"),
		"history":  mustHistoryKey("users", "terminal", 1),
		"index":    mustVersionIndexKey(uuidWithSeedAt(100, 1)),
	}
	for name, key := range physicalKeys {
		value, getErr := reopened.db.Get(key)
		if sequence == 0 {
			if !errors.Is(getErr, engine.ErrNotFound) {
				t.Fatalf("failed sync absent physical %s = %v", name, getErr)
			}
		} else if getErr != nil || len(value) == 0 {
			t.Fatalf("failed sync present physical %s = %v", name, getErr)
		}
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	engine, err := Open(filepath.Join(t.TempDir(), "data"), Options{UUIDGenerator: testGenerator()})
	if err != nil {
		t.Fatal(err)
	}
	closeWithTimeout := func() error {
		result := make(chan error, 1)
		go func() { result <- engine.Close() }()
		select {
		case err := <-result:
			return err
		case <-time.After(time.Second):
			return errors.New("Close timed out")
		}
	}
	if err := closeWithTimeout(); err != nil {
		t.Fatalf("first Close = %v", err)
	}
	if err := closeWithTimeout(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

type failSyncFS struct {
	engine.FS
	fail atomic.Bool
}

func (fs *failSyncFS) wrap(name string, file engine.File) engine.File {
	return &failSyncFile{File: file, name: name, fs: fs}
}

func (fs *failSyncFS) OpenFile(name string, flag int, perm fs.FileMode) (engine.File, error) {
	file, err := fs.FS.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return fs.wrap(name, file), nil
}

type failSyncFile struct {
	engine.File
	name string
	fs   *failSyncFS
}

func (file *failSyncFile) TryLock() error {
	lockable, ok := file.File.(engine.Lockable)
	if !ok {
		return errors.New("wrapped file is not lockable")
	}
	return lockable.TryLock()
}

func (file *failSyncFile) Unlock() error {
	lockable, ok := file.File.(engine.Lockable)
	if !ok {
		return errors.New("wrapped file is not lockable")
	}
	return lockable.Unlock()
}

func (file *failSyncFile) Sync() error {
	if file.fs.shouldFail(file.name) {
		return errors.New("injected WAL fsync failure")
	}
	return file.File.Sync()
}

func (fs *failSyncFS) shouldFail(name string) bool {
	return fs.fail.Load() && strings.HasSuffix(filepath.Base(name), ".wal")
}

func TestConcurrentSameVersionExactlyOneSuccess(t *testing.T) {
	engine, err := Open(filepath.Join(t.TempDir(), "data"), Options{UUIDGenerator: testGenerator()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	first, err := engine.Put("users", "race", []byte(`0`), Condition{})
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	wait.Add(2)
	for _, value := range []string{`1`, `2`} {
		go func(value string) {
			defer wait.Done()
			_, putErr := engine.Put("users", "race", []byte(value), Condition{IfMatch: &first.Row.Version})
			if putErr == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			} else if !errors.Is(putErr, ErrPrecondition) {
				t.Errorf("concurrent Put error = %v", putErr)
			}
		}(value)
	}
	wait.Wait()
	if successes != 1 {
		t.Fatalf("concurrent successes = %d, want 1", successes)
	}
}

func TestInvalidGeneratedVersionWritesNothing(t *testing.T) {
	engine, err := Open(filepath.Join(t.TempDir(), "data"), Options{UUIDGenerator: func() (uuidv7.UUID, error) {
		return uuidv7.UUID{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if _, err := engine.Put("users", "invalid", []byte(`true`), Condition{}); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("invalid generated UUID error = %v, want ErrInvalidVersion", err)
	}
	if sequence, err := engine.Sequence(); err != nil || sequence != 0 {
		t.Fatalf("sequence after invalid UUID = %d, %v", sequence, err)
	}
	if _, err := engine.Get("users", "invalid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("row after invalid UUID = %v, want ErrNotFound", err)
	}
}

func TestCommittedVersionsAreNeverReusedAcrossWritesOrRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	a := uuidWithSeed(1)
	b := uuidWithSeed(2)
	versions := []uuidv7.UUID{a, b, a}
	generate := func() UUIDGenerator {
		index := 0
		return func() (uuidv7.UUID, error) {
			if index >= len(versions) {
				return a, nil
			}
			version := versions[index]
			index++
			return version, nil
		}
	}
	engine, err := Open(dir, Options{UUIDGenerator: generate()})
	if err != nil {
		t.Fatal(err)
	}
	first, err := engine.Put("users", "aba", []byte(`"a"`), Condition{})
	if err != nil || first.Row.Version != a {
		t.Fatalf("first Put = %#v, %v", first, err)
	}
	second, err := engine.Put("users", "aba", []byte(`"b"`), Condition{})
	if err != nil || second.Row.Version != b {
		t.Fatalf("second Put = %#v, %v", second, err)
	}
	if _, err := engine.Put("users", "aba", []byte(`"a-again"`), Condition{IfMatch: &second.Row.Version}); !errors.Is(err, ErrVersionUsed) {
		t.Fatalf("ABA Put error = %v, want ErrVersionUsed", err)
	}
	if sequence, err := engine.Sequence(); err != nil || sequence != 2 {
		t.Fatalf("sequence after ABA rejection = %d, %v", sequence, err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir, Options{UUIDGenerator: generate()})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.Put("users", "other", []byte(`true`), Condition{}); !errors.Is(err, ErrVersionUsed) {
		t.Fatalf("restarted duplicate version error = %v, want ErrVersionUsed", err)
	}
	if sequence, err := reopened.Sequence(); err != nil || sequence != 2 {
		t.Fatalf("sequence after restarted rejection = %d, %v", sequence, err)
	}
}

func TestOpenRejectsMissingDurableVersionIndex(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	engine, err := Open(dir, Options{UUIDGenerator: testGenerator()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.Put("users", "indexed", []byte(`true`), Condition{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	index, err := versionIndexKey(result.Row.Version)
	if err != nil {
		t.Fatal(err)
	}
	db := rawEngineOpen(t, dir)
	rawDelete(t, db, index)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{UUIDGenerator: testGenerator()}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open without version index = %v, want ErrCorrupt", err)
	}
}

func TestPutRejectsInvalidConditionsAndCoordinatesAtStorageBoundary(t *testing.T) {
	engine, err := Open(filepath.Join(t.TempDir(), "data"), Options{UUIDGenerator: testGenerator()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	invalid := uuidv7.UUID{}
	for _, condition := range []Condition{
		{CreateOnly: true, IfMatch: &invalid},
		{IfMatch: &invalid},
	} {
		if _, err := engine.Put("users", "condition", []byte(`null`), condition); !errors.Is(err, ErrInvalidCondition) {
			t.Fatalf("condition %#v error = %v, want ErrInvalidCondition", condition, err)
		}
	}
	longTable := "a" + strings.Repeat("b", MaxTableBytes)
	longKey := strings.Repeat("k", MaxKeyBytes+1)
	for _, coordinates := range [][2]string{
		{"Users", "key"},
		{longTable, "key"},
		{"users", longKey},
		{"users", "key/with-slash"},
		{"transactions", "key"},
		{"_admin", "key"},
		{"_cdc", "key"},
	} {
		if _, err := engine.Put(coordinates[0], coordinates[1], []byte(`null`), Condition{}); !errors.Is(err, ErrInvalidCoordinates) {
			t.Fatalf("coordinates %q error = %v, want ErrInvalidCoordinates", coordinates, err)
		}
	}
	if sequence, err := engine.Sequence(); err != nil || sequence != 0 {
		t.Fatalf("sequence after invalid requests = %d, %v", sequence, err)
	}
}

func TestPutEnforcesCanonicalJSONAndExactValueLimit(t *testing.T) {
	engine, err := Open(filepath.Join(t.TempDir(), "data"), Options{UUIDGenerator: testGenerator()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	for name, value := range map[string][]byte{
		"invalid":      []byte(`{"broken"`),
		"noncanonical": []byte(`{"b":2,"a":1}`),
	} {
		if _, err := engine.Put("users", name, value, Condition{}); err == nil {
			t.Fatalf("%s value unexpectedly accepted", name)
		} else if name == "invalid" && !errors.Is(err, ErrInvalidJSON) {
			t.Fatalf("invalid JSON error = %v", err)
		} else if name == "noncanonical" && !errors.Is(err, ErrNonCanonicalJSON) {
			t.Fatalf("noncanonical JSON error = %v", err)
		}
	}
	exact := []byte(`"` + strings.Repeat("a", MaxValueBytes-2) + `"`)
	if len(exact) != MaxValueBytes {
		t.Fatalf("exact value fixture length = %d, want %d", len(exact), MaxValueBytes)
	}
	if _, err := engine.Put("users", "exact", exact, Condition{}); err != nil {
		t.Fatalf("exact maximum value rejected: %v", err)
	}
	tooLarge := append(append([]byte(nil), exact...), ' ')
	if _, err := engine.Put("users", "too-large", tooLarge, Condition{}); !errors.Is(err, ErrValueTooLarge) {
		t.Fatalf("maximum+1 value error = %v, want ErrValueTooLarge", err)
	}
	if sequence, err := engine.Sequence(); err != nil || sequence != 1 {
		t.Fatalf("sequence after value checks = %d, %v", sequence, err)
	}
	oversizedDirect := make([]byte, MaxValueBytes+1)
	allocs := testing.AllocsPerRun(20, func() {
		if _, err := engine.Put("users", "too-large-direct", oversizedDirect, Condition{}); !errors.Is(err, ErrValueTooLarge) {
			t.Fatalf("direct oversized value error = %v, want ErrValueTooLarge", err)
		}
	})
	if allocs != 0 {
		t.Fatalf("direct oversized value allocated %.2f objects per call", allocs)
	}
}

func TestValueCopiesAndReadAtBeforeFirstVersion(t *testing.T) {
	engine, err := Open(filepath.Join(t.TempDir(), "data"), Options{UUIDGenerator: testGenerator()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if _, err := engine.GetAt("users", "copy", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read before first version = %v, want ErrNotFound", err)
	}
	input := []byte(`{"n":1}`)
	if _, err := engine.Put("users", "copy", input, Condition{CreateOnly: true}); err != nil {
		t.Fatal(err)
	}
	input[0] = 'X'
	row, err := engine.Get("users", "copy")
	if err != nil {
		t.Fatal(err)
	}
	row.Value[0] = 'X'
	again, err := engine.Get("users", "copy")
	if err != nil || string(again.Value) != `{"n":1}` {
		t.Fatalf("row value was not copied: %q, %v", again.Value, err)
	}
}

func TestGetAtHistoryIsolatedByCoordinates(t *testing.T) {
	engine, err := Open(filepath.Join(t.TempDir(), "data"), Options{UUIDGenerator: testGenerator()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	firstA, err := engine.Put("users", "a", []byte(`"a1"`), Condition{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Put("users", "aa", []byte(`"aa1"`), Condition{}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Put("users", "a", []byte(`"a2"`), Condition{}); err != nil {
		t.Fatal(err)
	}
	row, err := engine.GetAt("users", "a", firstA.Row.Sequence)
	if err != nil || string(row.Value) != `"a1"` {
		t.Fatalf("coordinate-isolated history = %#v err=%v", row, err)
	}
}

func TestStartupRejectsStaleHeadAndTypedKeyCorruption(t *testing.T) {
	t.Run("stale head", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		engine, err := Open(dir, Options{UUIDGenerator: testGenerator()})
		if err != nil {
			t.Fatal(err)
		}
		first, err := engine.Put("users", "alice", []byte(`1`), Condition{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Put("users", "alice", []byte(`2`), Condition{}); err != nil {
			t.Fatal(err)
		}
		if err := engine.Close(); err != nil {
			t.Fatal(err)
		}
		stale, err := encodeRow(recordHead, first.Row)
		if err != nil {
			t.Fatal(err)
		}
		db := rawEngineOpen(t, dir)
		rawSet(t, db, mustHeadKey("users", "alice"), stale)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, Options{UUIDGenerator: testGenerator()}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Open stale head = %v, want ErrCorrupt", err)
		}
	})

	t.Run("typed head key", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		engine, err := Open(dir, Options{UUIDGenerator: testGenerator()})
		if err != nil {
			t.Fatal(err)
		}
		result, err := engine.Put("users", "alice", []byte(`1`), Condition{})
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.Close(); err != nil {
			t.Fatal(err)
		}
		table, _ := codec.String("vbdb-head")
		badTable := codec.Bytes([]byte("users"))
		key, _ := codec.String("alice")
		badKey, err := codec.EncodeTuple(table, badTable, key)
		if err != nil {
			t.Fatal(err)
		}
		record, err := encodeRow(recordHead, result.Row)
		if err != nil {
			t.Fatal(err)
		}
		db := rawEngineOpen(t, dir)
		rawSet(t, db, badKey, record)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, Options{UUIDGenerator: testGenerator()}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Open typed head key = %v, want ErrCorrupt", err)
		}
	})
}

func TestStartupRejectsDuplicateHistoricalVersion(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	engine, err := Open(dir, Options{UUIDGenerator: testGenerator()})
	if err != nil {
		t.Fatal(err)
	}
	first, err := engine.Put("users", "duplicate", []byte(`1`), Condition{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := engine.Put("users", "duplicate", []byte(`2`), Condition{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	second.Row.Version = first.Row.Version
	duplicate, err := encodeRow(recordVersion, second.Row)
	if err != nil {
		t.Fatal(err)
	}
	key, err := historyKey("users", "duplicate", second.Row.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	db := rawEngineOpen(t, dir)
	rawSet(t, db, key, duplicate)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{UUIDGenerator: testGenerator()}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open duplicate historical version = %v, want ErrCorrupt", err)
	}
}

func mustHeadKey(table, key string) []byte {
	encoded, err := headKey(table, key)
	if err != nil {
		panic(err)
	}
	return encoded
}

func mustSequenceKey() []byte {
	encoded, err := sequenceKey()
	if err != nil {
		panic(err)
	}
	return encoded
}

func mustHistoryKey(table, key string, sequence uint64) []byte {
	encoded, err := historyKey(table, key, sequence)
	if err != nil {
		panic(err)
	}
	return encoded
}

func mustVersionIndexKey(version uuidv7.UUID) []byte {
	encoded, err := versionIndexKey(version)
	if err != nil {
		panic(err)
	}
	return encoded
}

func TestCorruptSequenceFailsOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	engine, err := Open(dir, Options{UUIDGenerator: testGenerator()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Put("users", "alice", []byte(`true`), Condition{}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	sequenceKeyBytes, err := sequenceKey()
	if err != nil {
		t.Fatal(err)
	}
	db := rawEngineOpen(t, dir)
	rawSet(t, db, sequenceKeyBytes, []byte("corrupt"))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Open(dir, Options{UUIDGenerator: testGenerator()})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open corrupt sequence = %v, want ErrCorrupt", err)
	}
}

func TestLiveReadsRejectImpossibleSequences(t *testing.T) {
	t.Run("head beyond durable sequence", func(t *testing.T) {
		engine, err := Open(filepath.Join(t.TempDir(), "data"), Options{UUIDGenerator: testGenerator()})
		if err != nil {
			t.Fatal(err)
		}
		defer engine.Close()
		result, err := engine.Put("users", "head", []byte(`true`), Condition{})
		if err != nil {
			t.Fatal(err)
		}
		result.Row.Sequence++
		encoded, err := encodeRow(recordHead, result.Row)
		if err != nil {
			t.Fatal(err)
		}
		rawSet(t, engine.db, mustHeadKey("users", "head"), encoded)
		if _, err := engine.Get("users", "head"); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("live corrupt head = %v, want ErrCorrupt", err)
		}
	})

	t.Run("history key and value disagree", func(t *testing.T) {
		engine, err := Open(filepath.Join(t.TempDir(), "data"), Options{UUIDGenerator: testGenerator()})
		if err != nil {
			t.Fatal(err)
		}
		defer engine.Close()
		first, err := engine.Put("users", "history", []byte(`1`), Condition{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Put("users", "history", []byte(`2`), Condition{}); err != nil {
			t.Fatal(err)
		}
		first.Row.Sequence++
		encoded, err := encodeRow(recordVersion, first.Row)
		if err != nil {
			t.Fatal(err)
		}
		key, err := historyKey("users", "history", 1)
		if err != nil {
			t.Fatal(err)
		}
		rawSet(t, engine.db, key, encoded)
		if _, err := engine.GetAt("users", "history", 1); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("live corrupt history = %v, want ErrCorrupt", err)
		}
	})

	t.Run("valid checksum non-JSON head", func(t *testing.T) {
		engine, err := Open(filepath.Join(t.TempDir(), "data"), Options{UUIDGenerator: testGenerator()})
		if err != nil {
			t.Fatal(err)
		}
		defer engine.Close()
		result, err := engine.Put("users", "json", []byte(`true`), Condition{})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := encodeRow(recordHead, result.Row)
		if err != nil {
			t.Fatal(err)
		}
		copy(encoded[34:38], []byte("nope"))
		encoded = appendChecksum(encoded[:len(encoded)-4])
		rawSet(t, engine.db, mustHeadKey("users", "json"), encoded)
		if _, err := engine.Get("users", "json"); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("live non-JSON head = %v, want ErrCorrupt", err)
		}
	})
}

func FuzzRowRecordCodec(f *testing.F) {
	row := Row{Sequence: 7, Version: mustUUID(), Value: []byte(`{"ok":true}`)}
	encoded, err := encodeRow(recordVersion, row)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(encoded)
	f.Add([]byte("corrupt"))
	f.Fuzz(func(t *testing.T, input []byte) {
		decoded, err := decodeRow(input, recordVersion)
		if err != nil {
			return
		}
		canonical, err := encodeRow(recordVersion, decoded)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(canonical, input) {
			t.Fatalf("successful decode was not canonical: input=%x canonical=%x", input, canonical)
		}
	})
}

func FuzzSequenceRecordCodec(f *testing.F) {
	encoded := encodeSequence(7)
	f.Add(encoded)
	f.Add([]byte("corrupt"))
	f.Fuzz(func(t *testing.T, input []byte) {
		sequence, err := decodeSequence(input)
		if err != nil {
			return
		}
		if canonical := encodeSequence(sequence); !bytes.Equal(canonical, input) {
			t.Fatalf("successful sequence decode was not canonical: input=%x canonical=%x", input, canonical)
		}
	})
}

func TestRecordMutationsAreRejected(t *testing.T) {
	row := Row{Sequence: 7, Version: mustUUID(), Value: []byte(`{"ok":true}`)}
	encodedRow, err := encodeRow(recordVersion, row)
	if err != nil {
		t.Fatal(err)
	}
	for offset := range encodedRow {
		mutated := append([]byte(nil), encodedRow...)
		mutated[offset] ^= 1
		if _, err := decodeRow(mutated, recordVersion); err == nil {
			t.Fatalf("mutated row byte %d unexpectedly decoded", offset)
		}
	}
	encodedSequence := encodeSequence(row.Sequence)
	for offset := range encodedSequence {
		mutated := append([]byte(nil), encodedSequence...)
		mutated[offset] ^= 1
		if _, err := decodeSequence(mutated); err == nil {
			t.Fatalf("mutated sequence byte %d unexpectedly decoded", offset)
		}
	}
}

func TestPersistedRecordSizeBoundsPrecedeChecksumAndCopy(t *testing.T) {
	row := Row{Table: "users", Key: "bounded", Sequence: 7, Version: mustUUID(), Value: []byte(`true`)}
	encodedRow, err := encodeRow(recordVersion, row)
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint32(encodedRow[30:34], MaxValueBytes+1)
	if _, err := decodeRow(encodedRow, recordVersion); err == nil {
		t.Fatal("row with oversized declared value unexpectedly decoded")
	}
	if _, err := decodeRow(make([]byte, maxRowRecordBytes+1), recordVersion); err == nil {
		t.Fatal("oversized row unexpectedly reached decoding")
	}

	encodedIndex, err := encodeVersionIndex(row)
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint32(encodedIndex[6:10], uint32(maxHistoryKeyBytes+1))
	if _, err := decodeVersionIndex(encodedIndex, row.Version); err == nil {
		t.Fatal("version index with oversized history unexpectedly decoded")
	}
	if _, err := decodeVersionIndex(make([]byte, maxVersionIndexBytes+1), row.Version); err == nil {
		t.Fatal("oversized version index unexpectedly reached decoding")
	}
	maxTable := "a" + strings.Repeat("b", MaxTableBytes-1)
	maxKey := strings.Repeat("\x00", MaxKeyBytes)
	history, err := historyKey(maxTable, maxKey, row.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) > maxHistoryKeyBytes {
		t.Fatalf("maximum-coordinate history key length = %d, bound = %d", len(history), maxHistoryKeyBytes)
	}
	maxRow := row
	maxRow.Table, maxRow.Key = maxTable, maxKey
	if _, err := encodeVersionIndex(maxRow); err != nil {
		t.Fatalf("maximum-coordinate version index = %v", err)
	}
}

func FuzzPersistedKeyCodec(f *testing.F) {
	seed, err := headKey("users", "key")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte("corrupt"))
	f.Fuzz(func(t *testing.T, input []byte) {
		components, err := codec.DecodeTuple(input)
		if err != nil || len(components) != 3 {
			return
		}
		kind, err := components[0].Text()
		if err != nil || kind != "vbdb-head" {
			return
		}
		table, key, err := decodePersistedCoordinates(components, 3)
		if err != nil {
			return
		}
		canonical, err := headKey(table, key)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(canonical, input) {
			t.Fatalf("successful persisted key decode was not canonical: input=%x canonical=%x", input, canonical)
		}
	})
}

func mustUUID() uuidv7.UUID {
	uuid, err := uuidv7.Generator{Now: func() time.Time { return time.UnixMilli(1) }, Rand: bytes.NewReader(bytes.Repeat([]byte{1}, 10))}.New()
	if err != nil {
		panic(err)
	}
	return uuid
}

func uuidWithSeed(seed byte) uuidv7.UUID {
	return uuidWithSeedAt(1, seed)
}

func uuidWithSeedAt(milliseconds int64, seed byte) uuidv7.UUID {
	uuid, err := uuidv7.Generator{Now: func() time.Time { return time.UnixMilli(milliseconds) }, Rand: bytes.NewReader(bytes.Repeat([]byte{seed}, 10))}.New()
	if err != nil {
		panic(err)
	}
	return uuid
}
