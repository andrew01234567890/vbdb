package raftstore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
	"strings"

	"github.com/andrew01234567890/vbdb/internal/engine"
	"github.com/andrew01234567890/vbdb/internal/storage"
	"github.com/andrew01234567890/vbdb/pkg/codec"
	"github.com/andrew01234567890/vbdb/pkg/uuidv7"
	raft "go.etcd.io/raft/v3"
	pb "go.etcd.io/raft/v3/raftpb"
	"google.golang.org/protobuf/proto"
)

const (
	resultSuccess         = byte(1)
	resultPrecondition    = byte(2)
	resultVersionConflict = byte(3)
	stateFormat           = byte(1)
	snapshotMagic         = "VBS3"
	maxResultBytes        = 2<<20 + 64<<10
)

type rowVersion struct{ Row storage.Row }

type logicalState struct {
	rows        map[string]storage.Row
	history     map[string][]rowVersion
	results     map[uuidv7.UUID]Result
	versions    map[uuidv7.UUID]struct{}
	lastApplied uint64
	generation  uint64
}

func newLogicalState() *logicalState {
	return &logicalState{rows: make(map[string]storage.Row), history: make(map[string][]rowVersion), results: make(map[uuidv7.UUID]Result), versions: make(map[uuidv7.UUID]struct{})}
}

func (s *logicalState) clone() *logicalState {
	next := newLogicalState()
	next.lastApplied = s.lastApplied
	next.generation = s.generation
	for key, row := range s.rows {
		row.Value = append([]byte(nil), row.Value...)
		next.rows[key] = row
	}
	for key, history := range s.history {
		next.history[key] = make([]rowVersion, len(history))
		for i, version := range history {
			version.Row.Value = append([]byte(nil), version.Row.Value...)
			next.history[key][i] = version
		}
	}
	for id, result := range s.results {
		result.Row.Value = append([]byte(nil), result.Row.Value...)
		result.Command.Value = append([]byte(nil), result.Command.Value...)
		next.results[id] = result
	}
	for version := range s.versions {
		next.versions[version] = struct{}{}
	}
	return next
}

func stateKey(table, key string) ([]byte, error) {
	t, err := codec.String(table)
	if err != nil {
		return nil, err
	}
	k, err := codec.String(key)
	if err != nil {
		return nil, err
	}
	tuple, err := codec.EncodeTuple(t, k)
	if err != nil {
		return nil, err
	}
	return append([]byte(kRowPre), tuple...), nil
}
func historyKey3(table, key string, sequence uint64) ([]byte, error) {
	keyBytes, err := stateKey(table, key)
	if err != nil {
		return nil, err
	}
	result := append([]byte(kHistPre), keyBytes[len(kRowPre):]...)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], sequence)
	return append(result, n[:]...), nil
}
func dedupeKey(id uuidv7.UUID) []byte { return append(append([]byte(kDedupePre), id[:]...), 0) }
func resultKey(id uuidv7.UUID) []byte { return append([]byte(kResultPre), id[:]...) }

func statePrefix(generation uint64, prefix string) []byte {
	if generation == 0 {
		return []byte(prefix)
	}
	return []byte(fmt.Sprintf("m3/state/g/%020d/%s", generation, strings.TrimPrefix(prefix, "m3/state/")))
}

func stateKeyAt(generation uint64, table, key string) ([]byte, error) {
	base, err := stateKey(table, key)
	if err != nil {
		return nil, err
	}
	if generation == 0 {
		return base, nil
	}
	return append(statePrefix(generation, kRowPre), base[len(kRowPre):]...), nil
}

func historyKeyAt(generation uint64, table, key string, sequence uint64) ([]byte, error) {
	base, err := historyKey3(table, key, sequence)
	if err != nil {
		return nil, err
	}
	if generation == 0 {
		return base, nil
	}
	return append(statePrefix(generation, kHistPre), base[len(kHistPre):]...), nil
}

func dedupeKeyAt(generation uint64, id uuidv7.UUID) []byte {
	if generation == 0 {
		return dedupeKey(id)
	}
	return append(append(statePrefix(generation, kDedupePre), id[:]...), 0)
}

func resultKeyAt(generation uint64, id uuidv7.UUID) []byte {
	if generation == 0 {
		return resultKey(id)
	}
	return append(statePrefix(generation, kResultPre), id[:]...)
}

func (s *logicalState) load(d *diskStore) (result error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	dedupeIDs := make(map[uuidv7.UUID]struct{})
	resultIDs := make(map[uuidv7.UUID]struct{})
	sequenceIDs := make(map[uint64]struct{})
	items, err := d.scan()
	if err != nil {
		return err
	}
	for _, item := range items {
		if bytes.Equal(item.Key, []byte(kStateGeneration)) {
			payload, err := unwrapDisk(item.Value, 10)
			if err != nil || len(payload) != 8 {
				return fmt.Errorf("%w: state generation", ErrCorrupt)
			}
			s.generation = binary.BigEndian.Uint64(payload)
		}
	}
	rowPrefix := statePrefix(s.generation, kRowPre)
	historyPrefix := statePrefix(s.generation, kHistPre)
	resultPrefix := statePrefix(s.generation, kResultPre)
	dedupePrefix := statePrefix(s.generation, kDedupePre)
	for _, item := range items {
		key := item.Key
		value := item.Value
		switch {
		case bytes.HasPrefix(key, rowPrefix):
			keyOffset := len(kRowPre)
			if s.generation != 0 {
				keyOffset = len(rowPrefix)
			}
			table, rowKey, err := decodeStateCoordinates(key[keyOffset:])
			if err != nil {
				return fmt.Errorf("%w: row key: %v", ErrCorrupt, err)
			}
			payload, err := unwrapDisk(value, 10)
			if err != nil {
				return fmt.Errorf("%w: row: %v", ErrCorrupt, err)
			}
			row, err := decodeStateRow(payload, table, rowKey)
			if err != nil {
				return fmt.Errorf("%w: row: %v", ErrCorrupt, err)
			}
			s.rows[rowID(table, rowKey)] = row
		case bytes.HasPrefix(key, historyPrefix):
			keyOffset := len(kHistPre)
			if s.generation != 0 {
				keyOffset = len(historyPrefix)
			}
			if len(key) < keyOffset+8 {
				return fmt.Errorf("%w: history key", ErrCorrupt)
			}
			table, rowKey, err := decodeStateCoordinates(key[keyOffset : len(key)-8])
			if err != nil {
				return fmt.Errorf("%w: history key: %v", ErrCorrupt, err)
			}
			payload, err := unwrapDisk(value, 10)
			if err != nil {
				return fmt.Errorf("%w: history: %v", ErrCorrupt, err)
			}
			row, err := decodeStateRow(payload, table, rowKey)
			if err != nil {
				return fmt.Errorf("%w: history: %v", ErrCorrupt, err)
			}
			keySequence := binary.BigEndian.Uint64(key[len(key)-8:])
			if keySequence == 0 || keySequence != row.Sequence {
				return fmt.Errorf("%w: history sequence mismatch", ErrCorrupt)
			}
			s.history[rowID(table, rowKey)] = append(s.history[rowID(table, rowKey)], rowVersion{Row: row})
		case bytes.HasPrefix(key, resultPrefix):
			keyOffset := len(kResultPre)
			if s.generation != 0 {
				keyOffset = len(resultPrefix)
			}
			if len(key) != keyOffset+16 {
				return fmt.Errorf("%w: result key", ErrCorrupt)
			}
			var id uuidv7.UUID
			copy(id[:], key[keyOffset:])
			payload, err := unwrapDisk(value, 10)
			if err != nil {
				return fmt.Errorf("%w: result: %v", ErrCorrupt, err)
			}
			result, err := decodeResult(payload)
			if err != nil {
				return fmt.Errorf("%w: result: %v", ErrCorrupt, err)
			}
			s.results[id] = result
			resultIDs[id] = struct{}{}
		case bytes.HasPrefix(key, dedupePrefix):
			// Dedupe keys intentionally carry the operation ID as a separate
			// checked key; the result value is authoritative.
			keyOffset := len(kDedupePre)
			if s.generation != 0 {
				keyOffset = len(dedupePrefix)
			}
			if len(key) != keyOffset+17 || key[len(key)-1] != 0 {
				return fmt.Errorf("%w: dedupe key", ErrCorrupt)
			}
			var id uuidv7.UUID
			copy(id[:], key[keyOffset:keyOffset+16])
			payload, err := unwrapDisk(value, 10)
			if err != nil || !bytes.Equal(payload, []byte{1}) {
				return fmt.Errorf("%w: dedupe payload", ErrCorrupt)
			}
			dedupeIDs[id] = struct{}{}
		case bytes.Equal(key, []byte(kStateMeta)):
			payload, err := unwrapDisk(value, 10)
			if err != nil || len(payload) != 8 {
				return fmt.Errorf("%w: state metadata", ErrCorrupt)
			}
			s.lastApplied = binary.BigEndian.Uint64(payload)
		default:
			// Raft records were already envelope-checked by diskStore.load;
			// reject every other key rather than silently treating an unknown
			// persisted keyspace as absent state.
			if !isKnownRaftKey(key) && !isAnyLogicalKey(key) {
				return fmt.Errorf("%w: unknown state keyspace", ErrCorrupt)
			}
		}
	}
	for key, history := range s.history {
		sort.Slice(history, func(i, j int) bool { return history[i].Row.Sequence < history[j].Row.Sequence })
		s.history[key] = history
		if len(history) == 0 || history[len(history)-1].Row.Sequence > s.lastApplied {
			return fmt.Errorf("%w: history bounds", ErrCorrupt)
		}
		for index := 1; index < len(history); index++ {
			if history[index-1].Row.Sequence == history[index].Row.Sequence {
				return fmt.Errorf("%w: duplicate history sequence", ErrCorrupt)
			}
		}
		for _, version := range history {
			if _, exists := sequenceIDs[version.Row.Sequence]; exists {
				return fmt.Errorf("%w: duplicate history sequence", ErrCorrupt)
			}
			sequenceIDs[version.Row.Sequence] = struct{}{}
			if _, exists := s.versions[version.Row.Version]; exists {
				return fmt.Errorf("%w: duplicate row version", ErrCorrupt)
			}
			s.versions[version.Row.Version] = struct{}{}
		}
	}
	historyVersions := make(map[uuidv7.UUID]struct{}, len(s.versions))
	for version := range s.versions {
		historyVersions[version] = struct{}{}
	}
	for key, row := range s.rows {
		if row.Sequence == 0 || row.Sequence > s.lastApplied {
			return fmt.Errorf("%w: row bounds", ErrCorrupt)
		}
		history := s.history[key]
		if len(history) == 0 {
			return fmt.Errorf("%w: row has no history", ErrCorrupt)
		}
		latest := history[len(history)-1].Row
		if latest.Sequence != row.Sequence || latest.Version != row.Version || !bytes.Equal(latest.Value, row.Value) {
			return fmt.Errorf("%w: head/history mismatch", ErrCorrupt)
		}
	}
	for key := range s.history {
		if _, ok := s.rows[key]; !ok {
			return fmt.Errorf("%w: history has no head", ErrCorrupt)
		}
	}
	for id, result := range s.results {
		if result.Command.OperationID != id {
			return fmt.Errorf("%w: result operation id mismatch", ErrCorrupt)
		}
		if result.Succeeded() {
			if result.Row.Sequence == 0 || result.Row.Sequence > s.lastApplied || result.Row.Table != result.Command.Table || result.Row.Key != result.Command.Key {
				return fmt.Errorf("%w: result bounds", ErrCorrupt)
			}
			history := s.history[rowID(result.Row.Table, result.Row.Key)]
			found := false
			hadEarlierSuccess := false
			for _, version := range history {
				if version.Row.Sequence == result.Row.Sequence && version.Row.Version == result.Row.Version && bytes.Equal(version.Row.Value, result.Row.Value) {
					found = true
				}
				if version.Row.Sequence < result.Row.Sequence {
					hadEarlierSuccess = true
				}
			}
			if !found {
				return fmt.Errorf("%w: result row is absent from history", ErrCorrupt)
			}
			if result.Created == hadEarlierSuccess {
				return fmt.Errorf("%w: result created flag disagrees with history", ErrCorrupt)
			}
		} else if result.PreconditionFailed() || result.VersionConflict() {
			if result.Created || result.Row.Sequence != 0 || result.Row.Table != "" || result.Row.Key != "" || len(result.Row.Value) != 0 {
				return fmt.Errorf("%w: precondition result row", ErrCorrupt)
			}
		} else {
			return fmt.Errorf("%w: result status", ErrCorrupt)
		}
	}
	versions, err := collectVersions(historyVersions, s.results)
	if err != nil {
		return err
	}
	s.versions = versions
	for id := range dedupeIDs {
		if _, ok := s.results[id]; !ok {
			return fmt.Errorf("%w: dedupe result missing", ErrCorrupt)
		}
	}
	for id := range resultIDs {
		if _, ok := dedupeIDs[id]; !ok {
			return fmt.Errorf("%w: result dedupe missing", ErrCorrupt)
		}
	}
	if s.lastApplied != d.applied {
		return fmt.Errorf("%w: applied index mismatch", ErrCorrupt)
	}
	return nil
}

// collectVersions rebuilds the shard-global row-version reservation set. A
// version-conflict result may intentionally repeat an earlier candidate, but
// two non-conflict commands may not reserve the same version.
func collectVersions(historyVersions map[uuidv7.UUID]struct{}, results map[uuidv7.UUID]Result) (map[uuidv7.UUID]struct{}, error) {
	versions := make(map[uuidv7.UUID]struct{}, len(historyVersions)+len(results))
	for version := range historyVersions {
		versions[version] = struct{}{}
	}
	nonConflicts := make(map[uuidv7.UUID]struct{})
	for _, result := range results {
		version := result.Command.Version
		if !result.VersionConflict() {
			if _, exists := nonConflicts[version]; exists {
				return nil, fmt.Errorf("%w: duplicate row-version command", ErrCorrupt)
			}
			if result.PreconditionFailed() {
				if _, exists := historyVersions[version]; exists {
					return nil, fmt.Errorf("%w: precondition reused row version", ErrCorrupt)
				}
			}
			nonConflicts[version] = struct{}{}
		}
		versions[version] = struct{}{}
	}
	for _, result := range results {
		if result.VersionConflict() {
			version := result.Command.Version
			if _, inHistory := historyVersions[version]; !inHistory {
				if _, inResult := nonConflicts[version]; !inResult {
					return nil, fmt.Errorf("%w: version conflict has no prior reservation", ErrCorrupt)
				}
			}
		}
	}
	return versions, nil
}

func decodeStateCoordinates(encoded []byte) (string, string, error) {
	components, err := codec.DecodeTuple(encoded)
	if err != nil || len(components) != 2 {
		return "", "", errors.New("invalid state tuple")
	}
	if components[0].Kind() != codec.StringKind || components[1].Kind() != codec.StringKind {
		return "", "", errors.New("state coordinates are not strings")
	}
	table, err := components[0].Text()
	if err != nil {
		return "", "", err
	}
	key, err := components[1].Text()
	if err != nil {
		return "", "", err
	}
	if !validTable(table) || !validKey(key) {
		return "", "", errors.New("state coordinates bounds")
	}
	return table, key, nil
}

// rowID is a length-delimited binary identity rendered as a string. A plain
// table+NUL+key delimiter is unsafe because NUL is valid UTF-8 in both fields.
func rowID(table, key string) string {
	var encoded [8]byte
	binary.BigEndian.PutUint32(encoded[:4], uint32(len(table)))
	binary.BigEndian.PutUint32(encoded[4:], uint32(len(key)))
	return string(encoded[:]) + table + key
}

func encodeStateRow(row storage.Row) ([]byte, error) {
	if err := validateCanonicalValue(row.Value); err != nil {
		return nil, fmt.Errorf("state row value: %v", err)
	}
	result := make([]byte, 0, 8+16+4+len(row.Value))
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], row.Sequence)
	result = append(result, n[:]...)
	result = append(result, row.Version[:]...)
	binary.BigEndian.PutUint32(n[:4], uint32(len(row.Value)))
	result = append(result, n[:4]...)
	return append(result, row.Value...), nil
}
func decodeStateRow(encoded []byte, table, key string) (storage.Row, error) {
	if len(encoded) < 28 {
		return storage.Row{}, errors.New("state row too short")
	}
	length := int(binary.BigEndian.Uint32(encoded[24:28]))
	if length < 0 || len(encoded) != 28+length {
		return storage.Row{}, errors.New("state row length")
	}
	var version uuidv7.UUID
	copy(version[:], encoded[8:24])
	if _, err := uuidv7.UUIDFromBytes(version[:]); err != nil {
		return storage.Row{}, err
	}
	sequence := binary.BigEndian.Uint64(encoded[:8])
	if sequence == 0 {
		return storage.Row{}, errors.New("state row sequence is zero")
	}
	if err := validateCanonicalValue(encoded[28:]); err != nil {
		return storage.Row{}, fmt.Errorf("state row value: %v", err)
	}
	return storage.Row{Table: table, Key: key, Sequence: sequence, Version: version, Value: append([]byte(nil), encoded[28:]...)}, nil
}

// Result is durable operation identity. Precondition and row-version
// conflicts are explicit applied results, not transport failures.
type Result struct {
	Command Command
	Row     storage.Row
	Created bool
	Status  byte
}

func cloneResult(result Result) Result {
	result.Command.Value = append([]byte(nil), result.Command.Value...)
	if result.Command.Condition.IfMatch != nil {
		match := *result.Command.Condition.IfMatch
		result.Command.Condition.IfMatch = &match
	}
	result.Row.Value = append([]byte(nil), result.Row.Value...)
	return result
}

func (r Result) Succeeded() bool          { return r.Status == resultSuccess }
func (r Result) PreconditionFailed() bool { return r.Status == resultPrecondition }
func (r Result) VersionConflict() bool    { return r.Status == resultVersionConflict }

func encodeResult(result Result) ([]byte, error) {
	command, err := EncodeCommand(result.Command)
	if err != nil {
		return nil, err
	}
	if result.Status != resultSuccess && result.Status != resultPrecondition && result.Status != resultVersionConflict {
		return nil, errors.New("invalid result status")
	}
	if result.Status == resultPrecondition || result.Status == resultVersionConflict {
		if result.Created || result.Row.Sequence != 0 || len(result.Row.Value) != 0 || result.Row.Table != "" || result.Row.Key != "" {
			return nil, errors.New("precondition result contains a row")
		}
	} else if result.Row.Sequence == 0 {
		return nil, errors.New("successful result has no row")
	} else if result.Row.Table != result.Command.Table || result.Row.Key != result.Command.Key {
		return nil, errors.New("successful result coordinates mismatch")
	} else if result.Row.Version != result.Command.Version {
		return nil, errors.New("successful result version mismatch")
	} else if !bytes.Equal(result.Row.Value, result.Command.Value) {
		return nil, errors.New("successful result value mismatch")
	}
	var row []byte
	if result.Succeeded() {
		row, err = encodeStateRow(result.Row)
		if err != nil {
			return nil, err
		}
	}
	encoded := make([]byte, 0, 1+1+4+len(command)+4+len(row))
	encoded = append(encoded, stateFormat, result.Status)
	if result.Created {
		encoded = append(encoded, 1)
	} else {
		encoded = append(encoded, 0)
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(command)))
	encoded = append(encoded, n[:]...)
	encoded = append(encoded, command...)
	binary.BigEndian.PutUint32(n[:], uint32(len(row)))
	encoded = append(encoded, n[:]...)
	if len(encoded)+len(row) > maxResultBytes {
		return nil, errors.New("result exceeds bounded record size")
	}
	return append(encoded, row...), nil
}
func decodeResult(encoded []byte) (Result, error) {
	if len(encoded) < 11 || encoded[0] != stateFormat || (encoded[1] != resultSuccess && encoded[1] != resultPrecondition && encoded[1] != resultVersionConflict) || (encoded[2] != 0 && encoded[2] != 1) {
		return Result{}, errors.New("invalid result header")
	}
	commandLen := int(binary.BigEndian.Uint32(encoded[3:7]))
	if commandLen <= 0 || len(encoded) < 7+commandLen+4 {
		return Result{}, errors.New("invalid result command")
	}
	command, err := DecodeCommand(encoded[7 : 7+commandLen])
	if err != nil {
		return Result{}, err
	}
	rowLenOffset := 7 + commandLen
	rowLen := int(binary.BigEndian.Uint32(encoded[rowLenOffset : rowLenOffset+4]))
	if len(encoded) != rowLenOffset+4+rowLen {
		return Result{}, errors.New("invalid result row")
	}
	result := Result{Command: command, Created: encoded[2] != 0, Status: encoded[1]}
	if (result.Status == resultPrecondition || result.Status == resultVersionConflict) && (result.Created || rowLen != 0) {
		return Result{}, errors.New("precondition result contains a row")
	}
	if result.Status == resultSuccess && rowLen == 0 {
		return Result{}, errors.New("successful result has no row")
	}
	if rowLen != 0 {
		row, err := decodeStateRow(encoded[rowLenOffset+4:], command.Table, command.Key)
		if err != nil {
			return Result{}, err
		}
		if result.Status == resultSuccess && (row.Version != command.Version || !bytes.Equal(row.Value, command.Value)) {
			return Result{}, errors.New("successful result row mismatch")
		}
		result.Row = row
	}
	return result, nil
}

func (s *logicalState) apply(d *diskStore, entries []*pb.Entry, confState *pb.ConfState, confStateIndex uint64) (map[uuidv7.UUID]Result, map[uuidv7.UUID]error, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failed != nil {
		return nil, nil, d.failed
	}
	if confState != nil {
		if err := validateConfState(confState); err != nil {
			return nil, nil, fmt.Errorf("%w: conf state: %v", ErrCorrupt, err)
		}
	}
	next := s.clone()
	results := make(map[uuidv7.UUID]Result)
	conflicts := make(map[uuidv7.UUID]error)
	for _, entry := range entries {
		if entry.GetIndex() != next.lastApplied+1 {
			return nil, nil, fmt.Errorf("%w: apply index %d after %d", ErrCorrupt, entry.GetIndex(), next.lastApplied)
		}
		if entry.GetType() == pb.EntryNormal && len(entry.GetData()) > 0 {
			command, err := DecodeCommand(entry.GetData())
			if err != nil {
				return nil, nil, err
			}
			if existing, ok := next.results[command.OperationID]; ok {
				stored, err := EncodeCommand(existing.Command)
				if err != nil {
					return nil, nil, fmt.Errorf("%w: stored result command: %v", ErrCorrupt, err)
				}
				if !bytes.Equal(stored, entry.GetData()) {
					// A conflicting command is a deterministic client error, not
					// replica corruption. The entry still advances lastApplied;
					// notify its waiter after the same durable state batch.
					conflicts[command.OperationID] = ErrOperationConflict
					next.lastApplied = entry.GetIndex()
					continue
				}
				results[command.OperationID] = existing
				next.lastApplied = entry.GetIndex()
				continue
			}
			if _, reused := next.versions[command.Version]; reused {
				result := Result{Command: command, Status: resultVersionConflict}
				next.results[command.OperationID] = result
				results[command.OperationID] = result
				next.lastApplied = entry.GetIndex()
				continue
			}
			// Reserve every command's candidate version, including a
			// deterministic precondition failure. A later command cannot reuse
			// that UUID to manufacture an ABA-equivalent ETag.
			next.versions[command.Version] = struct{}{}
			id := rowID(command.Table, command.Key)
			current, exists := next.rows[id]
			failed := (command.Condition.CreateOnly && exists) || (command.Condition.IfMatch != nil && (!exists || current.Version != *command.Condition.IfMatch))
			result := Result{Command: command, Status: resultSuccess}
			if failed {
				result.Status = resultPrecondition
			} else {
				row := storage.Row{Table: command.Table, Key: command.Key, Version: command.Version, Sequence: entry.GetIndex(), Value: append([]byte(nil), command.Value...)}
				result.Row = row
				result.Created = !exists
				next.rows[id] = row
				next.history[id] = append(next.history[id], rowVersion{Row: row})
			}
			next.results[command.OperationID] = result
			results[command.OperationID] = result
		}
		next.lastApplied = entry.GetIndex()
	}
	batch := d.db.NewBatch()
	for key, row := range next.rows {
		old, existed := s.rows[key]
		if !existed || old.Sequence != row.Sequence || !bytes.Equal(old.Value, row.Value) {
			keyBytes, err := stateKeyAt(s.generation, row.Table, row.Key)
			if err != nil {
				return nil, nil, err
			}
			encodedRow, err := encodeStateRow(row)
			if err != nil {
				return nil, nil, err
			}
			if err := batch.Put(keyBytes, wrapDisk(10, encodedRow)); err != nil {
				return nil, nil, err
			}
		}
	}
	for id, result := range next.results {
		if _, existed := s.results[id]; !existed {
			encodedResult, err := encodeResult(result)
			if err != nil {
				return nil, nil, err
			}
			if err := batch.Put(resultKeyAt(s.generation, id), wrapDisk(10, encodedResult)); err != nil {
				return nil, nil, err
			}
			if err := batch.Put(dedupeKeyAt(s.generation, id), wrapDisk(10, []byte{1})); err != nil {
				return nil, nil, err
			}
		}
	}
	for key, history := range next.history {
		oldLen := len(s.history[key])
		for _, version := range history[oldLen:] {
			keyBytes, err := historyKeyAt(s.generation, version.Row.Table, version.Row.Key, version.Row.Sequence)
			if err != nil {
				return nil, nil, err
			}
			encodedRow, err := encodeStateRow(version.Row)
			if err != nil {
				return nil, nil, err
			}
			if err := batch.Put(keyBytes, wrapDisk(10, encodedRow)); err != nil {
				return nil, nil, err
			}
		}
	}
	var applied [8]byte
	binary.BigEndian.PutUint64(applied[:], next.lastApplied)
	if err := batch.Put([]byte(kApplied), wrapDisk(2, applied[:])); err != nil {
		return nil, nil, err
	}
	if err := batch.Put([]byte(kStateMeta), wrapDisk(10, applied[:])); err != nil {
		return nil, nil, err
	}
	if confState != nil {
		if confStateIndex == 0 {
			return nil, nil, fmt.Errorf("%w: conf state index is zero", ErrCorrupt)
		}
		payload, err := proto.Marshal(confState)
		if err != nil {
			return nil, nil, err
		}
		if err := batch.Put([]byte(kConfState), wrapDisk(3, payload)); err != nil {
			return nil, nil, err
		}
		var confIndex [8]byte
		binary.BigEndian.PutUint64(confIndex[:], confStateIndex)
		if err := batch.Put([]byte(kConfIndex), wrapDisk(8, confIndex[:])); err != nil {
			return nil, nil, err
		}
	}
	if len(entries) != 0 {
		if err := d.applyBatch(&batch, "state-apply"); err != nil {
			return nil, nil, err
		}
	}
	*s = *next
	d.applied = next.lastApplied
	if confState != nil {
		d.confState = proto.Clone(confState).(*pb.ConfState)
		d.confStateIndex = confStateIndex
	}
	return results, conflicts, nil
}

func (s *logicalState) get(table, key string) (storage.Row, error) {
	row, ok := s.rows[rowID(table, key)]
	if !ok {
		return storage.Row{}, storage.ErrNotFound
	}
	row.Value = append([]byte(nil), row.Value...)
	return row, nil
}
func (s *logicalState) exportSnapshot() ([]byte, error) {
	var out bytes.Buffer
	out.WriteString(snapshotMagic)
	out.WriteByte(stateFormat)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], s.lastApplied)
	out.Write(n[:])
	ids := make([]string, 0, len(s.rows))
	for id := range s.rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	putU32(&out, uint32(len(ids)))
	for _, id := range ids {
		row := s.rows[id]
		putString(&out, row.Table)
		putString(&out, row.Key)
		encodedRow, err := encodeStateRow(row)
		if err != nil {
			return nil, err
		}
		out.Write(encodedRow)
	}
	historyKeys := make([]string, 0, len(s.history))
	for id := range s.history {
		historyKeys = append(historyKeys, id)
	}
	sort.Strings(historyKeys)
	count := 0
	for _, id := range historyKeys {
		count += len(s.history[id])
	}
	putU32(&out, uint32(count))
	for _, id := range historyKeys {
		for _, version := range s.history[id] {
			putString(&out, version.Row.Table)
			putString(&out, version.Row.Key)
			encodedRow, err := encodeStateRow(version.Row)
			if err != nil {
				return nil, err
			}
			out.Write(encodedRow)
		}
	}
	resultIDs := make([]uuidv7.UUID, 0, len(s.results))
	for id := range s.results {
		resultIDs = append(resultIDs, id)
	}
	sort.Slice(resultIDs, func(i, j int) bool { return bytes.Compare(resultIDs[i][:], resultIDs[j][:]) < 0 })
	putU32(&out, uint32(len(resultIDs)))
	for _, id := range resultIDs {
		out.Write(id[:])
		result, err := encodeResult(s.results[id])
		if err != nil {
			return nil, err
		}
		putU32(&out, uint32(len(result)))
		out.Write(result)
	}
	checksum := crc32.Checksum(out.Bytes(), commandCRC)
	binary.BigEndian.PutUint32(n[:4], checksum)
	out.Write(n[:4])
	return out.Bytes(), nil
}
func putU32(out *bytes.Buffer, value uint32) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], value)
	out.Write(n[:])
}
func putString(out *bytes.Buffer, value string) {
	putU32(out, uint32(len(value)))
	out.WriteString(value)
}

func (s *logicalState) installSnapshot(d *diskStore, snapshot *pb.Snapshot) error {
	// Ready remains owned by raft until Advance. Never normalize metadata in
	// place: the raft goroutine may still inspect the same protobuf while this
	// synchronous apply is preparing its durable state.
	snapshot = proto.Clone(snapshot).(*pb.Snapshot)
	metadata := pb.EnsureSnapshotMetadata(snapshot.Metadata)
	snapshot.Metadata = metadata
	if err := validateConfState(metadata.ConfState); err != nil {
		return fmt.Errorf("%w: snapshot conf state: %v", ErrCorrupt, err)
	}
	decoded, err := decodeSnapshot(snapshot.GetData())
	if err != nil {
		return err
	}
	if decoded.lastApplied != metadata.GetIndex() {
		return fmt.Errorf("%w: snapshot index mismatch", ErrCorrupt)
	}
	if metadata.GetIndex() == 0 || metadata.GetTerm() == 0 {
		return fmt.Errorf("%w: snapshot metadata is empty", ErrCorrupt)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failed != nil {
		return d.failed
	}
	// Keep a newer durable configuration when recovery is asked to install a
	// historical snapshot record. Accepted raft Ready snapshots are newer than
	// the current committed state, so this branch is defensive and is not a
	// claim that production Raft can deliver an older incoming snapshot.
	targetConfState := pb.EnsureConfState(metadata.ConfState)
	targetConfIndex := metadata.GetIndex()
	if d.confStateIndex > metadata.GetIndex() {
		targetConfState = proto.Clone(d.confState).(*pb.ConfState)
		targetConfIndex = d.confStateIndex
	}
	currentSnapshotIndex := d.snapshot.GetMetadata().GetIndex()
	if metadata.GetIndex() < currentSnapshotIndex {
		return raft.ErrSnapOutOfDate
	}
	if metadata.GetIndex() == currentSnapshotIndex && metadata.GetTerm() != d.snapshot.GetMetadata().GetTerm() {
		return fmt.Errorf("%w: snapshot term mismatch", ErrCorrupt)
	}
	newGeneration := s.generation + 1
	if newGeneration == 0 {
		return fmt.Errorf("%w: snapshot generation exhausted", ErrCorrupt)
	}
	batch := d.db.NewBatch()
	batchCount := 0
	flush := func() error {
		if err := d.applyBatch(&batch, "snapshot-stage"); err != nil {
			return err
		}
		batch = d.db.NewBatch()
		batchCount = 0
		return nil
	}
	putBounded := func(key, value []byte) error {
		if err := batch.Put(key, value); err == nil {
			batchCount++
			return nil
		} else if !errors.Is(err, engine.ErrBatchTooLarge) {
			return err
		}
		if err := flush(); err != nil {
			return err
		}
		if err := batch.Put(key, value); err != nil {
			return err
		}
		batchCount++
		return nil
	}
	rowIDs := make([]string, 0, len(decoded.rows))
	for id := range decoded.rows {
		rowIDs = append(rowIDs, id)
	}
	sort.Strings(rowIDs)
	for _, id := range rowIDs {
		row := decoded.rows[id]
		keyBytes, err := stateKeyAt(newGeneration, row.Table, row.Key)
		if err != nil {
			return err
		}
		encodedRow, err := encodeStateRow(row)
		if err != nil {
			return err
		}
		if err := putBounded(keyBytes, wrapDisk(10, encodedRow)); err != nil {
			return err
		}
	}
	historyIDs := make([]string, 0, len(decoded.history))
	for id := range decoded.history {
		historyIDs = append(historyIDs, id)
	}
	sort.Strings(historyIDs)
	for _, id := range historyIDs {
		for _, version := range decoded.history[id] {
			keyBytes, err := historyKeyAt(newGeneration, version.Row.Table, version.Row.Key, version.Row.Sequence)
			if err != nil {
				return err
			}
			encodedRow, err := encodeStateRow(version.Row)
			if err != nil {
				return err
			}
			if err := putBounded(keyBytes, wrapDisk(10, encodedRow)); err != nil {
				return err
			}
		}
	}
	resultIDs := make([]uuidv7.UUID, 0, len(decoded.results))
	for id := range decoded.results {
		resultIDs = append(resultIDs, id)
	}
	sort.Slice(resultIDs, func(i, j int) bool { return bytes.Compare(resultIDs[i][:], resultIDs[j][:]) < 0 })
	for _, id := range resultIDs {
		encodedResult, err := encodeResult(decoded.results[id])
		if err != nil {
			return err
		}
		if err := putBounded(resultKeyAt(newGeneration, id), wrapDisk(10, encodedResult)); err != nil {
			return err
		}
		if err := putBounded(dedupeKeyAt(newGeneration, id), wrapDisk(10, []byte{1})); err != nil {
			return err
		}
	}
	if batchCount != 0 {
		if err := flush(); err != nil {
			return err
		}
	}
	var applied [8]byte
	binary.BigEndian.PutUint64(applied[:], metadata.GetIndex())
	batch = d.db.NewBatch()
	if err := batch.Put([]byte(kApplied), wrapDisk(2, applied[:])); err != nil {
		return err
	}
	if err := batch.Put([]byte(kStateMeta), wrapDisk(10, applied[:])); err != nil {
		return err
	}
	payload, err := proto.Marshal(targetConfState)
	if err != nil {
		return err
	}
	if err := batch.Put([]byte(kConfState), wrapDisk(3, payload)); err != nil {
		return err
	}
	var confIndex [8]byte
	binary.BigEndian.PutUint64(confIndex[:], targetConfIndex)
	if err := batch.Put([]byte(kConfIndex), wrapDisk(8, confIndex[:])); err != nil {
		return err
	}
	var generation [8]byte
	binary.BigEndian.PutUint64(generation[:], newGeneration)
	if err := batch.Put([]byte(kStateGeneration), wrapDisk(10, generation[:])); err != nil {
		return err
	}
	if err := d.applyBatch(&batch, "snapshot-apply"); err != nil {
		return err
	}
	decoded.lastApplied = metadata.GetIndex()
	decoded.generation = newGeneration
	*s = *decoded
	d.applied = decoded.lastApplied
	d.confState = proto.Clone(targetConfState).(*pb.ConfState)
	d.confStateIndex = targetConfIndex
	return nil
}

func decodeSnapshot(encoded []byte) (*logicalState, error) {
	if len(encoded) > maxSnapshotBytes {
		return nil, fmt.Errorf("%w: snapshot exceeds bounded size", ErrCorrupt)
	}
	if len(encoded) < 4+1+8+4+4 || string(encoded[:4]) != snapshotMagic || encoded[4] != stateFormat {
		return nil, fmt.Errorf("%w: snapshot header", ErrCorrupt)
	}
	if crc32.Checksum(encoded[:len(encoded)-4], commandCRC) != binary.BigEndian.Uint32(encoded[len(encoded)-4:]) {
		return nil, fmt.Errorf("%w: snapshot checksum", ErrCorrupt)
	}
	position := 5
	s := newLogicalState()
	s.lastApplied = binary.BigEndian.Uint64(encoded[position : position+8])
	position += 8
	readU32 := func() (uint32, error) {
		if position+4 > len(encoded)-4 {
			return 0, errors.New("short snapshot")
		}
		v := binary.BigEndian.Uint32(encoded[position : position+4])
		position += 4
		return v, nil
	}
	readString := func() (string, error) {
		n, err := readU32()
		if err != nil || n > maxCommandKey+maxCommandTable {
			return "", errors.New("snapshot string")
		}
		if position+int(n) > len(encoded)-4 {
			return "", errors.New("short snapshot string")
		}
		v := string(encoded[position : position+int(n)])
		position += int(n)
		return v, nil
	}
	rows, err := readU32()
	if err != nil {
		return nil, err
	}
	previousID := ""
	for i := uint32(0); i < rows; i++ {
		table, err := readString()
		if err != nil {
			return nil, err
		}
		key, err := readString()
		if err != nil {
			return nil, err
		}
		if !validTable(table) || !validKey(key) {
			return nil, errors.New("snapshot coordinates")
		}
		if previousID != "" && rowID(table, key) <= previousID {
			return nil, errors.New("snapshot rows are not sorted")
		}
		previousID = rowID(table, key)
		if position+28 > len(encoded)-4 {
			return nil, errors.New("snapshot row")
		}
		rowLen := int(binary.BigEndian.Uint32(encoded[position+24 : position+28]))
		if position+28+rowLen > len(encoded)-4 {
			return nil, errors.New("snapshot row length")
		}
		row, err := decodeStateRow(encoded[position:position+28+rowLen], table, key)
		if err != nil {
			return nil, err
		}
		if row.Sequence > s.lastApplied {
			return nil, errors.New("snapshot row sequence is ahead of applied")
		}
		position += 28 + rowLen
		s.rows[rowID(table, key)] = row
	}
	histories, err := readU32()
	if err != nil {
		return nil, err
	}
	previousHistory := ""
	var previousSequence uint64
	sequenceIDs := make(map[uint64]struct{})
	for i := uint32(0); i < histories; i++ {
		table, err := readString()
		if err != nil {
			return nil, err
		}
		key, err := readString()
		if err != nil {
			return nil, err
		}
		if !validTable(table) || !validKey(key) {
			return nil, errors.New("snapshot history coordinates")
		}
		if position+28 > len(encoded)-4 {
			return nil, errors.New("snapshot history")
		}
		rowLen := int(binary.BigEndian.Uint32(encoded[position+24 : position+28]))
		if position+28+rowLen > len(encoded)-4 {
			return nil, errors.New("snapshot history length")
		}
		row, err := decodeStateRow(encoded[position:position+28+rowLen], table, key)
		if err != nil {
			return nil, err
		}
		if row.Sequence > s.lastApplied {
			return nil, errors.New("snapshot history sequence is ahead of applied")
		}
		if previousHistory != "" && (rowID(table, key) < previousHistory || (rowID(table, key) == previousHistory && previousSequence >= row.Sequence)) {
			return nil, errors.New("snapshot history is not sorted")
		}
		previousHistory = rowID(table, key)
		position += 28 + rowLen
		previousSequence = row.Sequence
		if _, exists := s.versions[row.Version]; exists {
			return nil, errors.New("snapshot duplicate row version")
		}
		if _, exists := sequenceIDs[row.Sequence]; exists {
			return nil, errors.New("snapshot duplicate history sequence")
		}
		sequenceIDs[row.Sequence] = struct{}{}
		s.versions[row.Version] = struct{}{}
		s.history[rowID(table, key)] = append(s.history[rowID(table, key)], rowVersion{Row: row})
	}
	results, err := readU32()
	if err != nil {
		return nil, err
	}
	var previousResult uuidv7.UUID
	for i := uint32(0); i < results; i++ {
		if position+16 > len(encoded)-4 {
			return nil, errors.New("snapshot result id")
		}
		var id uuidv7.UUID
		copy(id[:], encoded[position:position+16])
		position += 16
		n, err := readU32()
		if err != nil || position+int(n) > len(encoded)-4 {
			return nil, errors.New("snapshot result")
		}
		result, err := decodeResult(encoded[position : position+int(n)])
		if err != nil {
			return nil, err
		}
		if result.Command.OperationID != id {
			return nil, errors.New("snapshot result operation id mismatch")
		}
		position += int(n)
		if i != 0 && bytes.Compare(id[:], previousResult[:]) <= 0 {
			return nil, errors.New("snapshot results are not sorted")
		}
		previousResult = id
		s.results[id] = result
	}
	historyVersions := make(map[uuidv7.UUID]struct{}, len(s.versions))
	for version := range s.versions {
		historyVersions[version] = struct{}{}
	}
	versions, err := collectVersions(historyVersions, s.results)
	if err != nil {
		return nil, err
	}
	s.versions = versions
	if position != len(encoded)-4 {
		return nil, errors.New("snapshot trailing bytes")
	}
	for key, row := range s.rows {
		history := s.history[key]
		if len(history) == 0 {
			return nil, errors.New("snapshot row has no history")
		}
		latest := history[len(history)-1].Row
		if latest.Sequence != row.Sequence || latest.Version != row.Version || !bytes.Equal(latest.Value, row.Value) {
			return nil, errors.New("snapshot head/history mismatch")
		}
	}
	for key := range s.history {
		if _, ok := s.rows[key]; !ok {
			return nil, errors.New("snapshot history has no head")
		}
	}
	for _, result := range s.results {
		if result.Succeeded() {
			if result.Row.Sequence == 0 || result.Row.Sequence > s.lastApplied || result.Row.Table != result.Command.Table || result.Row.Key != result.Command.Key {
				return nil, errors.New("snapshot result row bounds")
			}
			found := false
			hadEarlierSuccess := false
			for _, version := range s.history[rowID(result.Row.Table, result.Row.Key)] {
				if version.Row.Sequence == result.Row.Sequence && version.Row.Version == result.Row.Version && bytes.Equal(version.Row.Value, result.Row.Value) {
					found = true
					break
				}
				if version.Row.Sequence < result.Row.Sequence {
					hadEarlierSuccess = true
				}
			}
			if !found {
				return nil, errors.New("snapshot result row is absent from history")
			}
			if result.Created == hadEarlierSuccess {
				return nil, errors.New("snapshot result created flag disagrees with history")
			}
		} else if (!result.PreconditionFailed() && !result.VersionConflict()) || result.Created || result.Row.Sequence != 0 || result.Row.Table != "" || result.Row.Key != "" || len(result.Row.Value) != 0 {
			return nil, errors.New("snapshot precondition result row")
		}
	}
	return s, nil
}
