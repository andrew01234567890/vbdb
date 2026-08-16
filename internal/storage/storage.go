// Package storage provides VBDB's development-only single-node durable row
// engine. It deliberately contains no Raft, transaction, query, or index
// machinery.
package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/andrew01234567890/vbdb/internal/engine"
	"github.com/andrew01234567890/vbdb/pkg/codec"
	"github.com/andrew01234567890/vbdb/pkg/jsondoc"
	"github.com/andrew01234567890/vbdb/pkg/uuidv7"
)

const (
	// MaxTableBytes and MaxKeyBytes are storage invariants, not just HTTP
	// limits. Direct users of this package must not be able to create keys the
	// gateway could never address.
	MaxTableBytes = 63
	MaxKeyBytes   = 1 << 10
	MaxValueBytes = 1 << 20
	// A history key is a codec tuple. String components escape NUL bytes, so
	// the maximum key component occupies at most twice its input bytes plus
	// its two-byte terminator.
	maxHistoryKeyBytes   = 1 + (1 + len("vbdb-version") + 2) + (1 + MaxTableBytes + 2) + (1 + 2*MaxKeyBytes + 2) + (1 + 8) + 1
	maxRowRecordBytes    = 4 + 1 + 1 + 8 + 16 + 4 + MaxValueBytes + 4
	maxVersionIndexBytes = 4 + 1 + 1 + 4 + maxHistoryKeyBytes + 16 + 4
	// Put always contains exactly four records. Keep the request bound
	// derived from legal storage keys and values rather than the generic limit.
	maxStorageBatchBytes = 4 * (maxHistoryKeyBytes + maxRowRecordBytes + 16)

	// A collision is extraordinarily unlikely with crypto/rand, but injected
	// generators and a restarted process can deliberately return one. A small
	// bound keeps a broken generator from hanging a writer forever while still
	// allowing a generator to skip already-used values after a restart.
	maxVersionAttempts = 16

	magic              = "VBDB"
	recordFormat       = byte(1)
	recordHead         = byte(1)
	recordVersion      = byte(2)
	recordSequence     = byte(3)
	recordVersionIndex = byte(4)
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

var (
	ErrNotFound           = errors.New("storage: row not found")
	ErrPrecondition       = errors.New("storage: write precondition failed")
	ErrClosed             = errors.New("storage: engine is closed")
	ErrTerminal           = errors.New("storage: engine entered a terminal failure")
	ErrCorrupt            = errors.New("storage: corrupt persisted data")
	ErrValueTooLarge      = errors.New("storage: row value is too large")
	ErrInvalidCoordinates = errors.New("storage: invalid row coordinates")
	ErrInsecureDataDir    = errors.New("storage: data directory must be owner-only")
	ErrInvalidJSON        = errors.New("storage: row value is not valid JSON")
	ErrNonCanonicalJSON   = errors.New("storage: row value is not canonical JSON")
	ErrInvalidVersion     = errors.New("storage: UUID generator returned an invalid UUIDv7")
	ErrInvalidCondition   = errors.New("storage: invalid write condition")
	ErrVersionUsed        = errors.New("storage: UUIDv7 version was already committed")
)

// Row is an immutable version of a row. Value is always copied from the owned
// engine.
type Row struct {
	Table    string
	Key      string
	Version  uuidv7.UUID
	Sequence uint64
	Value    []byte
}

// Condition is validated before acquiring the writer lock and evaluated while
// the engine's single writer lock is held. The public struct remains a small
// compatibility surface for direct callers, so the mutually exclusive
// CreateOnly+IfMatch state is rejected explicitly at this boundary.
type Condition struct {
	IfMatch    *uuidv7.UUID
	CreateOnly bool
}

func validateCondition(condition Condition) error {
	if condition.CreateOnly && condition.IfMatch != nil {
		return ErrInvalidCondition
	}
	if condition.IfMatch != nil {
		if _, err := uuidv7.UUIDFromBytes(condition.IfMatch[:]); err != nil {
			return fmt.Errorf("%w: %w: If-Match version: %v", ErrInvalidCondition, ErrInvalidVersion, err)
		}
	}
	return nil
}

// WriteResult describes the row committed by Put.
type WriteResult struct {
	Row     Row
	Created bool
}

// UUIDGenerator is injected by tests; production uses uuidv7.New.
type UUIDGenerator func() (uuidv7.UUID, error)

// Options configures Open.
type Options struct {
	UUIDGenerator UUIDGenerator
	// engineOptions is intentionally package-private: same-package tests use
	// the owned engine's filesystem seam to inject deterministic failures.
	engineOptions *engine.Options
}

// Engine is a synchronized single-process row engine over the owned ordered
// key/value engine.
type Engine struct {
	db          *engine.Engine
	mu          sync.RWMutex
	sequence    uint64
	generate    UUIDGenerator
	closed      bool
	terminalErr error
}

// Open opens or creates a durable engine at dir and validates its sequence
// record before serving reads or writes.
func Open(dir string, options Options) (*Engine, error) {
	if dir == "" {
		return nil, errors.New("storage: data directory is required")
	}
	engineOptions := engine.Options{}
	if options.engineOptions != nil {
		engineOptions = *options.engineOptions
	}
	// Row records are wrapped in codec tuple keys and storage envelopes. The
	// generic engine defaults are intentionally too small for these legal
	// records, so this adapter owns the exact bounds it needs.
	engineOptions.MaxKeyBytes = maxHistoryKeyBytes
	engineOptions.MaxValueBytes = maxRowRecordBytes
	engineOptions.MaxBatchOps = 4
	engineOptions.MaxBatchBytes = maxStorageBatchBytes
	db, err := engine.Open(dir, engineOptions)
	if err != nil {
		return nil, mapEngineError("storage: open", err)
	}
	sequenceKeyBytes, err := sequenceKey()
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: encode sequence key: %w", err)
	}
	generate := options.UUIDGenerator
	if generate == nil {
		generate = uuidv7.New
	}
	e := &Engine{db: db, generate: generate}
	value, err := db.Get(sequenceKeyBytes)
	if err == nil {
		sequence, decodeErr := decodeSequence(value)
		if decodeErr != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%w: sequence: %v", ErrCorrupt, decodeErr)
		}
		e.sequence = sequence
	} else if !errors.Is(err, engine.ErrNotFound) {
		_ = db.Close()
		return nil, mapEngineError("storage: read sequence", err)
	} else {
		hasKeys, scanErr := hasUserKeys(db)
		if scanErr != nil {
			_ = db.Close()
			return nil, fmt.Errorf("storage: inspect records: %w", scanErr)
		}
		if hasKeys {
			_ = db.Close()
			return nil, fmt.Errorf("%w: sequence record is missing", ErrCorrupt)
		}
	}
	if err := validatePersistedRecords(db, e.sequence); err != nil {
		_ = db.Close()
		return nil, err
	}
	return e, nil
}

// Close closes the underlying owned engine. A close attempt is terminal for
// this wrapper even when the engine reports a close error: continuing could
// use a partially closed engine. Close is idempotent after that transition.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	closeErr := e.db.Close()
	e.closed = true
	if closeErr != nil {
		return mapEngineError("storage: close", closeErr)
	}
	return nil
}

// Sequence returns the last durable local commit sequence.
func (e *Engine) Sequence() (uint64, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.terminalErr != nil || e.closed {
		return 0, e.stateErr()
	}
	return e.sequence, nil
}

// Get returns the current head of a row.
func (e *Engine) Get(table, key string) (Row, error) {
	return e.GetAt(table, key, ^uint64(0))
}

// GetAt returns the newest immutable version whose local sequence is at most
// sequence. A sequence of ^uint64(0) means the current head.
func (e *Engine) GetAt(table, key string, sequence uint64) (Row, error) {
	if err := validateCoordinates(table, key); err != nil {
		return Row{}, err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.terminalErr != nil || e.closed {
		return Row{}, e.stateErr()
	}
	head, err := e.readHeadLocked(table, key)
	if err != nil {
		return Row{}, err
	}
	if sequence == ^uint64(0) || head.Sequence <= sequence {
		return head, nil
	}
	return e.readHistoryLocked(table, key, sequence)
}

// Put atomically commits one immutable history record, the current-head
// record, durable version index, and durable sequence record. Conditions are
// checked under the same writer lock and a failed condition writes nothing.
func (e *Engine) Put(table, key string, value []byte, condition Condition) (WriteResult, error) {
	if err := validateCoordinates(table, key); err != nil {
		return WriteResult{}, err
	}
	// Snapshot caller-owned byte and pointer inputs before any canonicalization
	// or condition inspection. Callers must still not concurrently mutate
	// strings or struct fields during this call; Go's API ownership does not
	// make concurrent mutation safe.
	if len(value) > MaxValueBytes {
		return WriteResult{}, ErrValueTooLarge
	}
	valueCopy := append([]byte(nil), value...)
	conditionCopy := condition
	if condition.IfMatch != nil {
		version := *condition.IfMatch
		conditionCopy.IfMatch = &version
	}
	canonical, err := jsondoc.Canonicalize(valueCopy)
	if err != nil {
		return WriteResult{}, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	if len(canonical) > MaxValueBytes {
		return WriteResult{}, ErrValueTooLarge
	}
	if !bytes.Equal(canonical, valueCopy) {
		return WriteResult{}, ErrNonCanonicalJSON
	}
	// The durable batch and returned row must never alias a request buffer that
	// a caller can mutate after Put returns.
	valueCopy = append([]byte(nil), canonical...)
	if err := validateCondition(conditionCopy); err != nil {
		return WriteResult{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.terminalErr != nil || e.closed {
		return WriteResult{}, e.stateErr()
	}
	head, headErr := e.readHeadLocked(table, key)
	missing := errors.Is(headErr, ErrNotFound)
	if headErr != nil && !missing {
		return WriteResult{}, headErr
	}
	if conditionCopy.CreateOnly && !missing {
		return WriteResult{}, ErrPrecondition
	}
	if conditionCopy.IfMatch != nil && (missing || head.Version != *conditionCopy.IfMatch) {
		return WriteResult{}, ErrPrecondition
	}
	if e.sequence == ^uint64(0) {
		return WriteResult{}, errors.New("storage: commit sequence exhausted")
	}
	sequence := e.sequence + 1
	var version uuidv7.UUID
	for attempt := 0; attempt < maxVersionAttempts; attempt++ {
		var generateErr error
		version, generateErr = e.generate()
		if generateErr != nil {
			return WriteResult{}, fmt.Errorf("storage: generate version: %w", generateErr)
		}
		if _, err := uuidv7.UUIDFromBytes(version[:]); err != nil {
			return WriteResult{}, fmt.Errorf("%w: %v", ErrInvalidVersion, err)
		}
		used, indexErr := e.versionUsedLocked(version)
		if indexErr != nil {
			return WriteResult{}, indexErr
		}
		if !used {
			break
		}
		if attempt == maxVersionAttempts-1 {
			return WriteResult{}, ErrVersionUsed
		}
	}
	row := Row{Table: table, Key: key, Version: version, Sequence: sequence, Value: valueCopy}
	versionKey, err := historyKey(table, key, sequence)
	if err != nil {
		return WriteResult{}, err
	}
	headKeyBytes, err := headKey(table, key)
	if err != nil {
		return WriteResult{}, err
	}
	versionRecord, err := encodeRow(recordVersion, row)
	if err != nil {
		return WriteResult{}, err
	}
	headRecord, err := encodeRow(recordHead, row)
	if err != nil {
		return WriteResult{}, err
	}
	sequenceRecord := encodeSequence(sequence)
	sequenceKeyBytes, err := sequenceKey()
	if err != nil {
		return WriteResult{}, err
	}
	indexKey, err := versionIndexKey(version)
	if err != nil {
		return WriteResult{}, err
	}
	versionIndexRecord, err := encodeVersionIndex(row)
	if err != nil {
		return WriteResult{}, err
	}
	batch := e.db.NewBatch()
	if err := batch.Put(versionKey, versionRecord); err != nil {
		return WriteResult{}, fmt.Errorf("storage: stage version: %w", err)
	}
	if err := batch.Put(headKeyBytes, headRecord); err != nil {
		return WriteResult{}, fmt.Errorf("storage: stage head: %w", err)
	}
	if err := batch.Put(sequenceKeyBytes, sequenceRecord); err != nil {
		return WriteResult{}, fmt.Errorf("storage: stage sequence: %w", err)
	}
	if err := batch.Put(indexKey, versionIndexRecord); err != nil {
		return WriteResult{}, fmt.Errorf("storage: stage version index: %w", err)
	}
	if _, err := e.db.Apply(&batch); err != nil {
		mapped := mapEngineError("storage: commit", err)
		if errors.Is(err, engine.ErrPoisoned) {
			e.terminalErr = mapped
		}
		return WriteResult{}, mapped
	}
	e.sequence = sequence
	return WriteResult{Row: row, Created: missing}, nil
}

func (e *Engine) stateErr() error {
	if e.terminalErr != nil {
		return e.terminalErr
	}
	return ErrClosed
}

func mapEngineError(context string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, engine.ErrInvalidDataDir):
		return fmt.Errorf("%w: %v", ErrInsecureDataDir, err)
	case errors.Is(err, engine.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, engine.ErrClosed):
		return ErrClosed
	case errors.Is(err, engine.ErrPoisoned):
		return fmt.Errorf("%w: %v", ErrTerminal, err)
	case errors.Is(err, engine.ErrCorrupt):
		return fmt.Errorf("%w: %v", ErrCorrupt, err)
	default:
		return fmt.Errorf("%s: %w", context, err)
	}
}

func (e *Engine) versionUsedLocked(version uuidv7.UUID) (bool, error) {
	key, err := versionIndexKey(version)
	if err != nil {
		return false, err
	}
	value, err := e.db.Get(key)
	if errors.Is(err, engine.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, mapEngineError("storage: read version index", err)
	}
	_, decodeErr := decodeVersionIndex(value, version)
	if decodeErr != nil {
		return false, fmt.Errorf("%w: version index: %v", ErrCorrupt, decodeErr)
	}
	return true, nil
}

func (e *Engine) readHeadLocked(table, key string) (Row, error) {
	encodedKey, err := headKey(table, key)
	if err != nil {
		return Row{}, err
	}
	value, err := e.db.Get(encodedKey)
	if errors.Is(err, engine.ErrNotFound) {
		return Row{}, ErrNotFound
	}
	if err != nil {
		return Row{}, mapEngineError("storage: read head", err)
	}
	row, decodeErr := decodeRow(value, recordHead)
	if decodeErr != nil {
		return Row{}, fmt.Errorf("%w: head: %v", ErrCorrupt, decodeErr)
	}
	if row.Sequence == 0 || row.Sequence > e.sequence {
		return Row{}, fmt.Errorf("%w: head sequence", ErrCorrupt)
	}
	row.Table, row.Key = table, key
	return row, nil
}

func (e *Engine) readHistoryLocked(table, key string, sequence uint64) (Row, error) {
	prefix, err := historyPrefix(table, key)
	if err != nil {
		return Row{}, err
	}
	upper, err := historyKey(table, key, sequence+1)
	if err != nil {
		return Row{}, err
	}
	entry, err := e.db.Last(prefix, upper)
	if errors.Is(err, engine.ErrNotFound) {
		return Row{}, ErrNotFound
	}
	if err != nil {
		return Row{}, mapEngineError("storage: history iterator", err)
	}
	keyCopy := append([]byte(nil), entry.Key...)
	components, decodeErr := codec.DecodeTuple(keyCopy)
	if decodeErr != nil || len(components) != 4 {
		return Row{}, fmt.Errorf("%w: history key", ErrCorrupt)
	}
	storedTable, tableErr := components[1].Text()
	storedKey, keyErr := components[2].Text()
	storedSequence, sequenceErr := components[3].Uint64()
	if tableErr != nil || keyErr != nil || sequenceErr != nil || storedTable != table || storedKey != key || storedSequence == 0 || storedSequence > sequence || storedSequence > e.sequence {
		return Row{}, fmt.Errorf("%w: history key coordinates", ErrCorrupt)
	}
	decoded, rowErr := decodeRow(entry.Value, recordVersion)
	if rowErr != nil {
		return Row{}, fmt.Errorf("%w: history value: %v", ErrCorrupt, rowErr)
	}
	if decoded.Sequence != storedSequence {
		return Row{}, fmt.Errorf("%w: history key/value sequence mismatch", ErrCorrupt)
	}
	decoded.Table, decoded.Key = table, key
	return decoded, nil
}

// IsReservedTable is the single M2 coordinate contract for names owned by
// later-milestone endpoints. Both direct storage users and HTTP use it.
func IsReservedTable(table string) bool {
	switch table {
	case "_admin", "_cdc", "transactions":
		return true
	default:
		return false
	}
}

func validateCoordinates(table, key string) error {
	if table == "" || key == "" || !utf8.ValidString(table) || !utf8.ValidString(key) {
		return ErrInvalidCoordinates
	}
	if IsReservedTable(table) {
		return ErrInvalidCoordinates
	}
	if len(table) > MaxTableBytes || len([]byte(key)) > MaxKeyBytes || strings.ContainsRune(key, '/') {
		return ErrInvalidCoordinates
	}
	if table[0] < 'a' || table[0] > 'z' {
		return ErrInvalidCoordinates
	}
	for i := 1; i < len(table); i++ {
		if (table[i] < 'a' || table[i] > 'z') && (table[i] < '0' || table[i] > '9') && table[i] != '_' && table[i] != '-' {
			return ErrInvalidCoordinates
		}
	}
	return nil
}

func headKey(table, key string) ([]byte, error) {
	kind, err := codec.String("vbdb-head")
	if err != nil {
		return nil, err
	}
	tableComponent, err := codec.String(table)
	if err != nil {
		return nil, err
	}
	keyComponent, err := codec.String(key)
	if err != nil {
		return nil, err
	}
	return codec.EncodeTuple(kind, tableComponent, keyComponent)
}

func historyPrefix(table, key string) ([]byte, error) {
	kind, err := codec.String("vbdb-version")
	if err != nil {
		return nil, err
	}
	tableComponent, err := codec.String(table)
	if err != nil {
		return nil, err
	}
	keyComponent, err := codec.String(key)
	if err != nil {
		return nil, err
	}
	encoded, err := codec.EncodeTuple(kind, tableComponent, keyComponent)
	if err != nil {
		return nil, err
	}
	return encoded[:len(encoded)-1], nil
}

func historyKey(table, key string, sequence uint64) ([]byte, error) {
	kind, err := codec.String("vbdb-version")
	if err != nil {
		return nil, err
	}
	tableComponent, err := codec.String(table)
	if err != nil {
		return nil, err
	}
	keyComponent, err := codec.String(key)
	if err != nil {
		return nil, err
	}
	return codec.EncodeTuple(kind, tableComponent, keyComponent, codec.Uint64(sequence))
}

func versionIndexKey(version uuidv7.UUID) ([]byte, error) {
	kind, err := codec.String("vbdb-version-index")
	if err != nil {
		return nil, err
	}
	versionComponent := codec.Bytes(version[:])
	return codec.EncodeTuple(kind, versionComponent)
}

func sequenceKey() ([]byte, error) {
	kind, err := codec.String("vbdb-sequence")
	if err != nil {
		return nil, err
	}
	key, err := codec.EncodeTuple(kind)
	if err != nil {
		return nil, err
	}
	return key, nil
}

func encodeRow(kind byte, row Row) ([]byte, error) {
	if kind != recordHead && kind != recordVersion || len(row.Value) > MaxValueBytes {
		return nil, errors.New("storage: invalid row record")
	}
	if canonical, err := jsondoc.Canonicalize(row.Value); err != nil || !bytes.Equal(canonical, row.Value) {
		return nil, errors.New("storage: row value is not canonical JSON")
	}
	result := make([]byte, 0, 4+1+1+8+16+4+len(row.Value)+4)
	result = append(result, magic...)
	result = append(result, recordFormat, kind)
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], row.Sequence)
	result = append(result, number[:]...)
	result = append(result, row.Version[:]...)
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(row.Value)))
	result = append(result, length[:]...)
	result = append(result, row.Value...)
	return appendChecksum(result), nil
}

func decodeRow(encoded []byte, expectedKind byte) (Row, error) {
	const fixedBytes = 4 + 1 + 1 + 8 + 16 + 4 + 4
	if len(encoded) < fixedBytes || len(encoded) > maxRowRecordBytes || string(encoded[:4]) != magic || encoded[4] != recordFormat || encoded[5] != expectedKind {
		return Row{}, errors.New("invalid row record header")
	}
	valueLength := binary.BigEndian.Uint32(encoded[30:34])
	if valueLength > MaxValueBytes || int(valueLength)+fixedBytes != len(encoded) {
		return Row{}, errors.New("invalid row record length")
	}
	if !validChecksum(encoded) {
		return Row{}, errors.New("row record checksum mismatch")
	}
	version, err := uuidv7.UUIDFromBytes(encoded[14:30])
	if err != nil {
		return Row{}, fmt.Errorf("invalid row UUID: %w", err)
	}
	value := append([]byte(nil), encoded[34:34+valueLength]...)
	canonical, err := jsondoc.Canonicalize(value)
	if err != nil {
		return Row{}, fmt.Errorf("invalid JSON value: %w", err)
	}
	if !bytes.Equal(canonical, value) {
		return Row{}, errors.New("non-canonical JSON value")
	}
	return Row{Sequence: binary.BigEndian.Uint64(encoded[6:14]), Version: version, Value: value}, nil
}

func encodeVersionIndex(row Row) ([]byte, error) {
	history, err := historyKey(row.Table, row.Key, row.Sequence)
	if err != nil {
		return nil, err
	}
	if len(history) > maxHistoryKeyBytes {
		return nil, errors.New("storage: history key exceeds persisted index bound")
	}
	result := make([]byte, 0, 4+1+1+4+len(history)+16+4)
	result = append(result, magic...)
	result = append(result, recordFormat, recordVersionIndex)
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(history)))
	result = append(result, length[:]...)
	result = append(result, history...)
	result = append(result, row.Version[:]...)
	return appendChecksum(result), nil
}

func decodeVersionIndex(encoded []byte, expected uuidv7.UUID) ([]byte, error) {
	const fixedBytes = 4 + 1 + 1 + 4 + 16 + 4
	if len(encoded) < fixedBytes || len(encoded) > maxVersionIndexBytes || string(encoded[:4]) != magic || encoded[4] != recordFormat || encoded[5] != recordVersionIndex {
		return nil, errors.New("invalid version index header")
	}
	historyLength := binary.BigEndian.Uint32(encoded[6:10])
	if historyLength > uint32(maxHistoryKeyBytes) || int(historyLength)+fixedBytes != len(encoded) {
		return nil, errors.New("invalid version index length")
	}
	if !validChecksum(encoded) {
		return nil, errors.New("version index checksum mismatch")
	}
	version, err := uuidv7.UUIDFromBytes(encoded[10+historyLength : 26+historyLength])
	if err != nil || version != expected {
		return nil, errors.New("version index UUID mismatch")
	}
	history := append([]byte(nil), encoded[10:10+historyLength]...)
	components, err := codec.DecodeTuple(history)
	if err != nil || len(components) != 4 || components[0].Kind() != codec.StringKind {
		return nil, errors.New("version index history key is malformed")
	}
	kind, err := components[0].Text()
	if err != nil || kind != "vbdb-version" {
		return nil, errors.New("version index history kind is malformed")
	}
	table, key, err := decodePersistedCoordinates(components, 4)
	if err != nil {
		return nil, err
	}
	sequence, err := components[3].Uint64()
	if err != nil || sequence == 0 {
		return nil, errors.New("version index history sequence is malformed")
	}
	canonical, err := historyKey(table, key, sequence)
	if err != nil || !bytes.Equal(canonical, history) {
		return nil, errors.New("version index history key is not canonical")
	}
	return history, nil
}

func encodeSequence(sequence uint64) []byte {
	encoded := make([]byte, 0, 4+1+1+8+4)
	encoded = append(encoded, magic...)
	encoded = append(encoded, recordFormat, recordSequence)
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], sequence)
	encoded = append(encoded, number[:]...)
	return appendChecksum(encoded)
}

func decodeSequence(encoded []byte) (uint64, error) {
	if len(encoded) != 4+1+1+8+4 || string(encoded[:4]) != magic || encoded[4] != recordFormat || encoded[5] != recordSequence || !validChecksum(encoded) {
		return 0, errors.New("invalid sequence record")
	}
	return binary.BigEndian.Uint64(encoded[6:14]), nil
}

func appendChecksum(encoded []byte) []byte {
	checksum := crc32.Checksum(encoded, crcTable)
	var sum [4]byte
	binary.BigEndian.PutUint32(sum[:], checksum)
	return append(encoded, sum[:]...)
}

func validChecksum(encoded []byte) bool {
	if len(encoded) < 4 {
		return false
	}
	want := binary.BigEndian.Uint32(encoded[len(encoded)-4:])
	return crc32.Checksum(encoded[:len(encoded)-4], crcTable) == want
}

func hasUserKeys(db *engine.Engine) (hasKeys bool, retErr error) {
	stream, err := db.Stream(nil, nil)
	if err != nil {
		return false, mapEngineError("storage: inspect records", err)
	}
	defer func() {
		if closeErr := stream.Close(); closeErr != nil && retErr == nil {
			retErr = mapEngineError("storage: close record stream", closeErr)
		}
	}()
	_, ok, nextErr := stream.Next()
	if nextErr != nil {
		return false, mapEngineError("storage: inspect records", nextErr)
	}
	return ok, nil
}

func validatePersistedRecords(db *engine.Engine, sequence uint64) (retErr error) {
	// This makes a corrupt historical or version-index record an explicit open
	// failure rather than allowing it to masquerade as an absent row later.
	stream, err := db.Stream(nil, nil)
	if err != nil {
		return fmt.Errorf("%w: record iterator: %v", ErrCorrupt, err)
	}
	defer func() {
		if closeErr := stream.Close(); closeErr != nil && retErr == nil {
			retErr = fmt.Errorf("%w: close record iterator: %v", ErrCorrupt, closeErr)
		}
	}()
	type persistedKey struct {
		table string
		key   string
	}
	type latestHistory struct {
		sequence uint64
		row      Row
	}
	historyKeys := make(map[persistedKey]struct{})
	latestHistories := make(map[persistedKey]latestHistory)
	heads := make(map[persistedKey]Row)
	// This set is startup-only integrity state. It deliberately does not live
	// on Engine; constant-memory restart validation is deferred for M2.
	seenSequences := make(map[uint64]struct{})
	for {
		entry, valid, nextErr := stream.Next()
		if nextErr != nil {
			return fmt.Errorf("%w: record iterator: %v", ErrCorrupt, nextErr)
		}
		if !valid {
			break
		}
		key := append([]byte(nil), entry.Key...)
		components, decodeErr := codec.DecodeTuple(key)
		if decodeErr != nil {
			return fmt.Errorf("%w: key: %v", ErrCorrupt, decodeErr)
		}
		if len(components) == 0 || components[0].Kind() != codec.StringKind {
			return fmt.Errorf("%w: unknown key", ErrCorrupt)
		}
		kind, decodeErr := components[0].Text()
		if decodeErr != nil {
			return fmt.Errorf("%w: key kind: %v", ErrCorrupt, decodeErr)
		}
		value := entry.Value
		switch kind {
		case "vbdb-sequence":
			if len(components) != 1 {
				return fmt.Errorf("%w: sequence key shape", ErrCorrupt)
			}
			stored, sequenceErr := decodeSequence(value)
			if sequenceErr != nil || stored != sequence {
				return fmt.Errorf("%w: sequence record mismatch", ErrCorrupt)
			}
		case "vbdb-head":
			table, rowKey, coordinateErr := decodePersistedCoordinates(components, 3)
			if coordinateErr != nil {
				return fmt.Errorf("%w: head key shape", ErrCorrupt)
			}
			row, rowErr := decodeRow(value, recordHead)
			if rowErr != nil || row.Sequence == 0 || row.Sequence > sequence {
				return fmt.Errorf("%w: head record", ErrCorrupt)
			}
			headID := persistedKey{table: table, key: rowKey}
			heads[headID] = row
		case "vbdb-version":
			if len(components) != 4 {
				return fmt.Errorf("%w: history key shape", ErrCorrupt)
			}
			table, rowKey, coordinateErr := decodePersistedCoordinates(components, 4)
			if coordinateErr != nil {
				return fmt.Errorf("%w: history key shape", ErrCorrupt)
			}
			storedSequence, sequenceErr := components[3].Uint64()
			row, rowErr := decodeRow(value, recordVersion)
			if sequenceErr != nil || storedSequence == 0 || rowErr != nil || storedSequence != row.Sequence || storedSequence > sequence {
				return fmt.Errorf("%w: history record", ErrCorrupt)
			}
			encodedHistoryKey, historyErr := historyKey(table, rowKey, storedSequence)
			if historyErr != nil || !bytes.Equal(encodedHistoryKey, key) {
				return fmt.Errorf("%w: history key is not canonical", ErrCorrupt)
			}
			indexKey, indexErr := versionIndexKey(row.Version)
			if indexErr != nil {
				return fmt.Errorf("%w: version index key", ErrCorrupt)
			}
			indexValue, indexGetErr := db.Get(indexKey)
			if errors.Is(indexGetErr, engine.ErrNotFound) {
				return fmt.Errorf("%w: history has no version index", ErrCorrupt)
			}
			if indexGetErr != nil {
				return fmt.Errorf("%w: version index read: %v", ErrCorrupt, indexGetErr)
			}
			indexHistory, indexDecodeErr := decodeVersionIndex(indexValue, row.Version)
			if indexDecodeErr != nil || !bytes.Equal(indexHistory, key) {
				return fmt.Errorf("%w: version index mismatch", ErrCorrupt)
			}
			keyID := persistedKey{table: table, key: rowKey}
			if _, exists := seenSequences[storedSequence]; exists {
				return fmt.Errorf("%w: duplicate history sequence", ErrCorrupt)
			}
			seenSequences[storedSequence] = struct{}{}
			historyKeys[keyID] = struct{}{}
			latest, exists := latestHistories[keyID]
			if !exists || storedSequence > latest.sequence {
				latestHistories[keyID] = latestHistory{sequence: storedSequence, row: row}
			}
		case "vbdb-version-index":
			if len(components) != 2 || components[1].Kind() != codec.BytesKind {
				return fmt.Errorf("%w: version index key shape", ErrCorrupt)
			}
			versionBytes, versionErr := components[1].Bytes()
			version, versionParseErr := uuidv7.UUIDFromBytes(versionBytes)
			canonicalIndexKey, canonicalKeyErr := versionIndexKey(version)
			if versionErr != nil || versionParseErr != nil || canonicalKeyErr != nil || !bytes.Equal(canonicalIndexKey, key) {
				return fmt.Errorf("%w: version index key", ErrCorrupt)
			}
			history, indexErr := decodeVersionIndex(value, version)
			if indexErr != nil {
				return fmt.Errorf("%w: version index value", ErrCorrupt)
			}
			historyValue, historyGetErr := db.Get(history)
			if errors.Is(historyGetErr, engine.ErrNotFound) {
				return fmt.Errorf("%w: version index points to missing history", ErrCorrupt)
			}
			if historyGetErr != nil {
				return fmt.Errorf("%w: indexed history read: %v", ErrCorrupt, historyGetErr)
			}
			historyRow, historyDecodeErr := decodeRow(historyValue, recordVersion)
			if historyDecodeErr != nil || historyRow.Version != version {
				return fmt.Errorf("%w: version index history mismatch", ErrCorrupt)
			}
		default:
			return fmt.Errorf("%w: unknown record kind", ErrCorrupt)
		}
	}
	if (sequence == 0 && len(seenSequences) != 0) || (sequence != 0 && uint64(len(seenSequences)) != sequence) {
		return fmt.Errorf("%w: sequence coverage mismatch", ErrCorrupt)
	}
	for keyID, latest := range latestHistories {
		head, exists := heads[keyID]
		if !exists {
			return fmt.Errorf("%w: history has no current head", ErrCorrupt)
		}
		if head.Sequence != latest.row.Sequence || head.Version != latest.row.Version || !bytes.Equal(head.Value, latest.row.Value) {
			return fmt.Errorf("%w: head does not match latest history", ErrCorrupt)
		}
	}
	if len(heads) != len(historyKeys) {
		return fmt.Errorf("%w: orphan current head", ErrCorrupt)
	}
	return nil
}

func decodePersistedCoordinates(components []codec.Component, count int) (string, string, error) {
	if len(components) != count || components[1].Kind() != codec.StringKind || components[2].Kind() != codec.StringKind {
		return "", "", errors.New("table and key components must be strings")
	}
	table, err := components[1].Text()
	if err != nil {
		return "", "", err
	}
	key, err := components[2].Text()
	if err != nil {
		return "", "", err
	}
	if validateCoordinates(table, key) != nil {
		return "", "", ErrInvalidCoordinates
	}
	return table, key, nil
}
