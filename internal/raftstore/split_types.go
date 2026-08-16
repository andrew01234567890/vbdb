package raftstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
	"unicode/utf8"

	"github.com/andrew01234567890/vbdb/internal/storage"
	"github.com/andrew01234567890/vbdb/pkg/uuidv7"
	pb "go.etcd.io/raft/v3/raftpb"
)

const (
	maxSplitRows            = 1 << 20
	maxSplitDeltaBytes      = 1 << 20
	maxSplitDeltas          = 1 << 20
	maxSplitSnapshotBytes   = 8 << 20
	maxSplitDeltaBytesTotal = 8 << 20
	maxSplitCopyChunkBytes  = 64 << 10
	splitChunkHeaderBytes   = 17
	maxSplitValidationBytes = 64 << 20
	maxSplitRetainedBytes   = 128 << 20
)

var splitCRC = crc32.MakeTable(crc32.Castagnoli)

var (
	ErrSplitActive     = errors.New("raftstore: split already active")
	ErrSplitNotActive  = errors.New("raftstore: split is not active")
	ErrSplitUnsafeCopy = errors.New("raftstore: no safe secondary snapshot source")
	ErrSplitChecksum   = errors.New("raftstore: split snapshot checksum mismatch")
	ErrSplitBarrier    = errors.New("raftstore: split barrier not caught up")
	ErrSplitGeneration = errors.New("raftstore: split generation or owner epoch mismatch")
	ErrSplitDeltaOrder = errors.New("raftstore: split delta order violation")
	ErrSplitPending    = errors.New("raftstore: split operation is awaiting child apply")
	ErrSplitQuorum     = errors.New("raftstore: split target quorum proof failed")
)

// SplitSnapshot is a bounded, immutable image copied before a child can serve.
// The checksum covers the complete source fence, barrier, and row payloads.
type SplitSnapshot struct {
	Source      RangeDescriptor
	Barrier     uint64
	SourceEpoch uint64
	Rows        []storage.Row
	Checksum    uint32
}

func (s SplitSnapshot) Clone() SplitSnapshot {
	s.Source = s.Source.Clone()
	s.Rows = cloneSplitRows(s.Rows)
	return s
}

func (s SplitSnapshot) calculateChecksum() uint32 {
	return crc32.Checksum(encodeSplitSnapshotBody(s), splitCRC)
}

func (s SplitSnapshot) Validate(source RangeDescriptor, expectedBarrier, expectedEpoch uint64) error {
	if err := source.Validate(); err != nil || source.Phase != RangeServing {
		return ErrSplitChecksum
	}
	if !EqualRangeDescriptor(s.Source, source) || s.Barrier != expectedBarrier || s.SourceEpoch != expectedEpoch || expectedEpoch != source.OwnerEpoch {
		return ErrSplitGeneration
	}
	if s.Checksum == 0 {
		return ErrSplitChecksum
	}
	if err := validateSplitRows(s.Rows, s.Source, s.Barrier); err != nil {
		return err
	}
	if s.calculateChecksum() != s.Checksum {
		return ErrSplitChecksum
	}
	encoded, err := splitSnapshotEncodedSize(s.Source, s.Rows)
	if err != nil {
		return err
	}
	if encoded > maxSplitSnapshotBytes {
		return ErrBackpressure
	}
	return nil
}

// CopyChunks emits fixed bounded units. The receiver accepts reordering and
// exact duplicates, but validates every chunk before allocating reassembly.
func (s SplitSnapshot) CopyChunks() ([][]byte, error) {
	if err := s.Validate(s.Source, s.Barrier, s.SourceEpoch); err != nil {
		return nil, err
	}
	payload := encodeSplitSnapshot(s)
	chunkPayload := maxSplitCopyChunkBytes - splitChunkHeaderBytes
	total := (len(payload) + chunkPayload - 1) / chunkPayload
	if total == 0 || total > maxSplitSnapshotBytes/chunkPayload+1 {
		return nil, ErrBackpressure
	}
	chunks := make([][]byte, 0, total)
	for index, offset := 0, 0; offset < len(payload); index, offset = index+1, offset+chunkPayload {
		end := offset + chunkPayload
		if end > len(payload) {
			end = len(payload)
		}
		chunk := make([]byte, splitChunkHeaderBytes+end-offset)
		copy(chunk[:4], "VBCP")
		chunk[4] = 1
		binary.BigEndian.PutUint32(chunk[5:9], uint32(total))
		binary.BigEndian.PutUint32(chunk[9:13], uint32(index))
		binary.BigEndian.PutUint32(chunk[13:17], uint32(end-offset))
		copy(chunk[splitChunkHeaderBytes:], payload[offset:end])
		chunks = append(chunks, chunk)
	}
	return chunks, nil
}

func BuildSplitSnapshot(source RangeDescriptor, barrier, sourceEpoch uint64, rows []storage.Row) (SplitSnapshot, error) {
	if err := source.Validate(); err != nil || source.Phase != RangeServing || barrier == 0 || sourceEpoch == 0 || sourceEpoch != source.OwnerEpoch {
		return SplitSnapshot{}, ErrSplitGeneration
	}
	ordered := cloneSplitRows(rows)
	sort.Slice(ordered, func(i, j int) bool { return splitRowLess(ordered[i], ordered[j]) })
	if err := validateSplitRows(ordered, source, barrier); err != nil {
		return SplitSnapshot{}, err
	}
	snapshot := SplitSnapshot{Source: source.Clone(), Barrier: barrier, SourceEpoch: sourceEpoch, Rows: ordered}
	snapshot.Checksum = snapshot.calculateChecksum()
	if size, err := splitSnapshotEncodedSize(source, ordered); err != nil || size > maxSplitSnapshotBytes {
		if err != nil {
			return SplitSnapshot{}, err
		}
		return SplitSnapshot{}, ErrBackpressure
	}
	return snapshot, nil
}

func receiveSplitChunks(chunks [][]byte) (SplitSnapshot, error) {
	chunkPayload := maxSplitCopyChunkBytes - splitChunkHeaderBytes
	maxChunks := maxSplitSnapshotBytes/chunkPayload + 1
	if len(chunks) == 0 || len(chunks) > maxChunks {
		return SplitSnapshot{}, ErrSplitChecksum
	}
	parts := make(map[uint32][]byte, len(chunks))
	var total uint32
	for _, chunk := range chunks {
		if len(chunk) < splitChunkHeaderBytes || len(chunk) > maxSplitCopyChunkBytes || !bytes.Equal(chunk[:4], []byte("VBCP")) || chunk[4] != 1 {
			return SplitSnapshot{}, ErrSplitChecksum
		}
		chunkTotal := binary.BigEndian.Uint32(chunk[5:9])
		index := binary.BigEndian.Uint32(chunk[9:13])
		length := binary.BigEndian.Uint32(chunk[13:17])
		if chunkTotal == 0 || int(chunkTotal) > maxChunks || index >= chunkTotal || length > uint32(chunkPayload) || int(length)+splitChunkHeaderBytes != len(chunk) {
			return SplitSnapshot{}, ErrSplitChecksum
		}
		if total == 0 {
			total = chunkTotal
		} else if total != chunkTotal {
			return SplitSnapshot{}, ErrSplitChecksum
		}
		part := append([]byte(nil), chunk[splitChunkHeaderBytes:]...)
		if previous, exists := parts[index]; exists {
			if !bytes.Equal(previous, part) {
				return SplitSnapshot{}, ErrSplitChecksum
			}
			continue
		}
		parts[index] = part
	}
	if uint32(len(parts)) != total {
		return SplitSnapshot{}, ErrSplitChecksum
	}
	totalBytes := 0
	for _, part := range parts {
		var err error
		totalBytes, err = splitMemoryAdd(totalBytes, len(part))
		if err != nil || totalBytes > maxSplitSnapshotBytes {
			return SplitSnapshot{}, ErrBackpressure
		}
	}
	assembled := make([]byte, totalBytes)
	position := 0
	for index := uint32(0); index < total; index++ {
		part, ok := parts[index]
		if !ok {
			return SplitSnapshot{}, ErrSplitChecksum
		}
		position += copy(assembled[position:], part)
	}
	return decodeSplitSnapshot(assembled)
}

func decodeSplitSnapshot(encoded []byte) (SplitSnapshot, error) {
	decoder := splitDecoder{encoded: encoded, limit: len(encoded)}
	if len(encoded) < 4+1+4 || !bytes.Equal(encoded[:4], []byte("VBS4")) {
		return SplitSnapshot{}, ErrSplitChecksum
	}
	decoder.position = 4
	if _, err := decoder.byte(); err != nil {
		return SplitSnapshot{}, ErrSplitChecksum
	}
	if encoded[4] != 1 {
		return SplitSnapshot{}, ErrSplitChecksum
	}
	source, err := decodeRangeDescriptor(&decoder)
	if err != nil {
		return SplitSnapshot{}, ErrSplitChecksum
	}
	barrier, err := decoder.u64()
	if err != nil {
		return SplitSnapshot{}, ErrSplitChecksum
	}
	epoch, err := decoder.u64()
	if err != nil {
		return SplitSnapshot{}, ErrSplitChecksum
	}
	count, err := decoder.u32()
	if err != nil || count > maxSplitRows || int(count) > decoder.remaining()/16 {
		return SplitSnapshot{}, ErrBackpressure
	}
	if scratch, scratchErr := splitRowValidationScratchMemoryCount(int(count)); scratchErr != nil || scratch > maxSplitValidationBytes {
		return SplitSnapshot{}, ErrBackpressure
	}
	rows := make([]storage.Row, int(count))
	for i := range rows {
		table, tableErr := decoder.string(maxCommandTable)
		key, keyErr := decoder.string(maxCommandKey)
		version, versionErr := decoder.take(16)
		sequence, sequenceErr := decoder.u64()
		value, valueErr := decoder.bytes(maxSplitDeltaBytes)
		if tableErr != nil || keyErr != nil || versionErr != nil || sequenceErr != nil || valueErr != nil {
			return SplitSnapshot{}, ErrSplitChecksum
		}
		var id uuidv7.UUID
		copy(id[:], version)
		rows[i] = storage.Row{Table: string(table), Key: string(key), Version: id, Sequence: sequence, Value: value}
	}
	if decoder.remaining() != 4 {
		return SplitSnapshot{}, ErrSplitChecksum
	}
	checksum, err := decoder.u32()
	if err != nil || decoder.remaining() != 0 {
		return SplitSnapshot{}, ErrSplitChecksum
	}
	snapshot := SplitSnapshot{Source: source, Barrier: barrier, SourceEpoch: epoch, Rows: rows, Checksum: checksum}
	if err := snapshot.Validate(source, barrier, epoch); err != nil {
		return SplitSnapshot{}, err
	}
	return snapshot, nil
}

// SplitDelta is the ordered, source-fence-bound suffix after a snapshot.
type SplitDelta struct {
	Sequence                uint64
	SourceRangeID           string
	SourceStart             []byte
	SourceEnd               []byte
	SourceEndIsInfinity     bool
	SourceGeneration        uint64
	SourceEpoch             uint64
	SourceOwnerEpoch        uint64
	SourceGroupID           uint64
	SourceVoters            []uint64
	SourceConfigFingerprint [32]byte
	SourcePhase             ServingPhase
	Command                 Command
	Result                  Result
}

func cloneSplitDelta(delta SplitDelta) SplitDelta {
	delta.SourceStart = append([]byte(nil), delta.SourceStart...)
	delta.SourceEnd = append([]byte(nil), delta.SourceEnd...)
	delta.SourceVoters = append([]uint64(nil), delta.SourceVoters...)
	delta.Command = cloneCommand(delta.Command)
	delta.Result = cloneResult(delta.Result)
	return delta
}

func splitConfigFingerprint(source RangeDescriptor) [32]byte {
	var encoded bytes.Buffer
	encodeRangeDescriptor(&encoded, source)
	return sha256.Sum256(encoded.Bytes())
}

func splitDeltaFor(source RangeDescriptor, epoch, sequence uint64, command Command, result Result) SplitDelta {
	return SplitDelta{Sequence: sequence, SourceRangeID: source.RangeID, SourceStart: append([]byte(nil), source.Start...), SourceEnd: append([]byte(nil), source.End...), SourceEndIsInfinity: source.End == nil, SourceGeneration: source.Generation, SourceEpoch: epoch, SourceOwnerEpoch: source.OwnerEpoch, SourceGroupID: source.GroupID, SourceVoters: append([]uint64(nil), source.Voters...), SourceConfigFingerprint: splitConfigFingerprint(source), SourcePhase: source.Phase, Command: cloneCommand(command), Result: cloneResult(result)}
}

func splitDeltaDigest(delta SplitDelta) ([32]byte, error) {
	command, err := EncodeCommand(delta.Command)
	if err != nil {
		return [32]byte{}, err
	}
	result, err := encodeResult(delta.Result)
	if err != nil {
		return [32]byte{}, err
	}
	var encoded bytes.Buffer
	putSplitString(&encoded, delta.SourceRangeID)
	putSplitBytes(&encoded, delta.SourceStart)
	if delta.SourceEndIsInfinity {
		encoded.WriteByte(1)
	} else {
		encoded.WriteByte(0)
		putSplitBytes(&encoded, delta.SourceEnd)
	}
	for _, value := range []uint64{delta.Sequence, delta.SourceGeneration, delta.SourceEpoch, delta.SourceOwnerEpoch, delta.SourceGroupID} {
		var number [8]byte
		binary.BigEndian.PutUint64(number[:], value)
		encoded.Write(number[:])
	}
	for _, voter := range delta.SourceVoters {
		var number [8]byte
		binary.BigEndian.PutUint64(number[:], voter)
		encoded.Write(number[:])
	}
	encoded.Write(delta.SourceConfigFingerprint[:])
	encoded.WriteByte(byte(delta.SourcePhase))
	encoded.Write(command)
	encoded.Write(result)
	return sha256.Sum256(encoded.Bytes()), nil
}

// SplitTargetProof is the pre-cutover quorum/checksum evidence. Cutover is a
// later stack layer; this proof only certifies that prepared children remain
// non-serving until that layer publishes one complete catalog image.
type SplitTargetProof struct {
	Source        RangeDescriptor
	Left          RangeDescriptor
	Right         RangeDescriptor
	Barrier       uint64
	FinalSequence uint64
	SnapshotHash  [32]byte
	LeftHash      [32]byte
	RightHash     [32]byte
	ConfState     *pb.ConfState
	Quorum        bool
}

func (p SplitTargetProof) Validate() error {
	if err := p.Source.Validate(); err != nil || p.Source.Phase != RangeServing || p.Barrier == 0 || p.FinalSequence < p.Barrier {
		return ErrSplitQuorum
	}
	if p.Left.Phase != RangeCatchingUp || p.Right.Phase != RangeCatchingUp || len(p.Left.Voters) != 3 || !equalUint64(p.Left.Voters, p.Source.Voters) || !equalUint64(p.Right.Voters, p.Source.Voters) {
		return ErrSplitQuorum
	}
	if !bytes.Equal(p.Left.End, p.Right.Start) || !bytes.Equal(p.Left.Start, p.Source.Start) || !bytes.Equal(p.Right.End, p.Source.End) || p.Left.RangeID == p.Right.RangeID || p.Source.RangeID == p.Left.RangeID || p.Source.RangeID == p.Right.RangeID {
		return ErrSplitQuorum
	}
	if p.ConfState == nil || !equalUint64(p.ConfState.GetVoters(), p.Source.Voters) || !p.Quorum {
		return ErrSplitQuorum
	}
	return nil
}

type SplitReplica struct {
	ID                uint64
	RangeID           string
	GroupID           uint64
	Generation        uint64
	OwnerEpoch        uint64
	Voters            []uint64
	ConfigFingerprint [32]byte
	Term              uint64
	Applied           uint64
	Available         bool
	Rows              map[string]storage.Row
}

type splitOperationRecord struct {
	Sequence uint64
	Digest   [32]byte
	Result   Result
}

type splitOperation struct {
	source, left, right       RangeDescriptor
	barrier                   uint64
	snapshot                  SplitSnapshot
	leftRows, rightRows       map[string]storage.Row
	leftApplied, rightApplied uint64
	deltas                    map[uint64]SplitDelta
	deltaDigests              map[uint64][32]byte
	operationRecords          map[uuidv7.UUID]splitOperationRecord
}

func cloneSplitRows(rows []storage.Row) []storage.Row {
	out := make([]storage.Row, len(rows))
	for i := range rows {
		out[i] = rows[i]
		out[i].Value = append([]byte(nil), rows[i].Value...)
	}
	return out
}

type splitDecoder struct {
	encoded  []byte
	position int
	limit    int
}

func (d *splitDecoder) remaining() int { return d.limit - d.position }
func (d *splitDecoder) take(length int) ([]byte, error) {
	if length < 0 || d.position > d.limit-length {
		return nil, ErrSplitChecksum
	}
	value := append([]byte(nil), d.encoded[d.position:d.position+length]...)
	d.position += length
	return value, nil
}
func (d *splitDecoder) byte() (byte, error) {
	value, err := d.take(1)
	if err != nil {
		return 0, err
	}
	return value[0], nil
}
func (d *splitDecoder) u32() (uint32, error) {
	value, err := d.take(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(value), nil
}
func (d *splitDecoder) u64() (uint64, error) {
	value, err := d.take(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(value), nil
}
func (d *splitDecoder) bytes(max int) ([]byte, error) {
	length, err := d.u32()
	if err != nil || length > uint32(max) {
		return nil, ErrSplitChecksum
	}
	return d.take(int(length))
}
func (d *splitDecoder) string(max int) ([]byte, error) {
	value, err := d.bytes(max)
	if err != nil || !utf8.Valid(value) {
		return nil, ErrSplitChecksum
	}
	return value, nil
}

func putSplitBytes(out *bytes.Buffer, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	out.Write(length[:])
	out.Write(value)
}
func putSplitString(out *bytes.Buffer, value string) { putSplitBytes(out, []byte(value)) }

func encodeSplitSnapshot(snapshot SplitSnapshot) []byte {
	encoded := encodeSplitSnapshotBody(snapshot)
	var checksum [4]byte
	binary.BigEndian.PutUint32(checksum[:], snapshot.Checksum)
	return append(encoded, checksum[:]...)
}

func encodeSplitSnapshotBody(snapshot SplitSnapshot) []byte {
	var encoded bytes.Buffer
	encoded.WriteString("VBS4")
	encoded.WriteByte(1)
	encodeRangeDescriptor(&encoded, snapshot.Source)
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], snapshot.Barrier)
	encoded.Write(number[:])
	binary.BigEndian.PutUint64(number[:], snapshot.SourceEpoch)
	encoded.Write(number[:])
	var count [4]byte
	binary.BigEndian.PutUint32(count[:], uint32(len(snapshot.Rows)))
	encoded.Write(count[:])
	for _, row := range snapshot.Rows {
		putSplitString(&encoded, row.Table)
		putSplitString(&encoded, row.Key)
		encoded.Write(row.Version[:])
		binary.BigEndian.PutUint64(number[:], row.Sequence)
		encoded.Write(number[:])
		putSplitBytes(&encoded, row.Value)
	}
	return encoded.Bytes()
}

func encodeRangeDescriptor(out *bytes.Buffer, descriptor RangeDescriptor) {
	putSplitString(out, descriptor.RangeID)
	putSplitBytes(out, descriptor.Start)
	if descriptor.End == nil {
		var infinity [4]byte
		binary.BigEndian.PutUint32(infinity[:], ^uint32(0))
		out.Write(infinity[:])
	} else {
		putSplitBytes(out, descriptor.End)
	}
	var number [8]byte
	for _, value := range []uint64{descriptor.Generation, descriptor.OwnerEpoch, descriptor.GroupID} {
		binary.BigEndian.PutUint64(number[:], value)
		out.Write(number[:])
	}
	out.WriteByte(byte(descriptor.Phase))
	var count [4]byte
	binary.BigEndian.PutUint32(count[:], uint32(len(descriptor.Voters)))
	out.Write(count[:])
	for _, voter := range descriptor.Voters {
		binary.BigEndian.PutUint64(number[:], voter)
		out.Write(number[:])
	}
}

func decodeRangeDescriptor(decoder *splitDecoder) (RangeDescriptor, error) {
	rangeID, err := decoder.string(maxRangeIDBytes)
	if err != nil {
		return RangeDescriptor{}, err
	}
	start, err := decoder.bytes(maxRangeKeyBytes)
	if err != nil {
		return RangeDescriptor{}, err
	}
	endMarker, err := decoder.u32()
	if err != nil {
		return RangeDescriptor{}, err
	}
	var end []byte
	if endMarker != ^uint32(0) {
		if endMarker > maxRangeKeyBytes {
			return RangeDescriptor{}, ErrSplitChecksum
		}
		end, err = decoder.take(int(endMarker))
		if err != nil {
			return RangeDescriptor{}, err
		}
	}
	values := make([]uint64, 3)
	for i := range values {
		values[i], err = decoder.u64()
		if err != nil {
			return RangeDescriptor{}, err
		}
	}
	phase, err := decoder.byte()
	if err != nil {
		return RangeDescriptor{}, err
	}
	voterCount, err := decoder.u32()
	if err != nil || voterCount != 3 {
		return RangeDescriptor{}, ErrSplitChecksum
	}
	voters := make([]uint64, voterCount)
	for i := range voters {
		voters[i], err = decoder.u64()
		if err != nil {
			return RangeDescriptor{}, err
		}
	}
	descriptor := RangeDescriptor{RangeID: string(rangeID), Start: start, End: end, Generation: values[0], OwnerEpoch: values[1], GroupID: values[2], Voters: voters, Phase: ServingPhase(phase)}
	if err := descriptor.Validate(); err != nil {
		return RangeDescriptor{}, err
	}
	return descriptor, nil
}

func splitRowLess(a, b storage.Row) bool {
	if a.Table != b.Table {
		return a.Table < b.Table
	}
	return a.Key < b.Key
}

func cloneCommand(command Command) Command {
	command.Value = append([]byte(nil), command.Value...)
	if command.Condition.IfMatch != nil {
		match := *command.Condition.IfMatch
		command.Condition.IfMatch = &match
	}
	return command
}

func splitRowID(table, key string) string {
	return fmt.Sprintf("%d:%s%d:%s", len(table), table, len(key), key)
}

func validateSplitRows(rows []storage.Row, source RangeDescriptor, barrier uint64) error {
	if len(rows) > maxSplitRows {
		return ErrBackpressure
	}
	size, err := splitSnapshotBaseEncodedSize(source)
	if err != nil {
		return err
	}
	versions := make(map[uuidv7.UUID]struct{}, len(rows))
	sequences := make(map[uint64]struct{}, len(rows))
	coordinates := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if !validTable(row.Table) || !validKey(row.Key) || !source.Contains([]byte(row.Key)) || row.Sequence == 0 || row.Sequence > barrier || len(row.Value) > maxSplitDeltaBytes || validateCanonicalValue(row.Value) != nil {
			return ErrSplitChecksum
		}
		if _, err := uuidv7.UUIDFromBytes(row.Version[:]); err != nil {
			return ErrSplitChecksum
		}
		rowSize := 4 + len(row.Table) + 4 + len(row.Key) + 16 + 8 + 4 + len(row.Value)
		if rowSize < 0 || size > maxSplitSnapshotBytes-4-rowSize {
			return ErrBackpressure
		}
		size += rowSize
		if _, exists := versions[row.Version]; exists {
			return ErrSplitChecksum
		}
		versions[row.Version] = struct{}{}
		if _, exists := sequences[row.Sequence]; exists {
			return ErrSplitChecksum
		}
		sequences[row.Sequence] = struct{}{}
		id := splitRowID(row.Table, row.Key)
		if _, exists := coordinates[id]; exists {
			return ErrSplitChecksum
		}
		coordinates[id] = struct{}{}
	}
	if scratch, err := splitRowValidationScratchMemoryCount(len(rows)); err != nil || scratch > maxSplitValidationBytes {
		return ErrBackpressure
	}
	for i := 1; i < len(rows); i++ {
		if splitRowLess(rows[i], rows[i-1]) {
			return ErrSplitChecksum
		}
	}
	return nil
}

func splitSnapshotBaseEncodedSize(source RangeDescriptor) (int, error) {
	var encoded bytes.Buffer
	encodeRangeDescriptor(&encoded, source)
	return splitMemorySum(4+1, encoded.Len(), 8, 8, 4, 4)
}

func splitSnapshotEncodedSize(source RangeDescriptor, rows []storage.Row) (int, error) {
	size, err := splitSnapshotBaseEncodedSize(source)
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		size, err = splitMemorySum(size, 4+len(row.Table), 4+len(row.Key), 16, 8, 4+len(row.Value))
		if err != nil {
			return 0, err
		}
	}
	return splitMemoryAdd(size, 4)
}

func splitRowValidationScratchMemoryCount(count int) (int, error) {
	if count < 0 {
		return 0, ErrBackpressure
	}
	return splitMemorySum(count*64, count*32, count*32)
}
