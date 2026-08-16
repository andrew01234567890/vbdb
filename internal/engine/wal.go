package engine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"sort"
)

const (
	segmentMagic     = "VBSE"
	frameMagic       = "VBFR"
	walFormat        = byte(1)
	segmentKind      = byte(1)
	batchKind        = byte(1)
	segmentHeaderLen = 28
	frameHeaderLen   = 32
	frameTrailerLen  = 4
	minSegmentBytes  = segmentHeaderLen + frameHeaderLen + frameTrailerLen + 9
	maxUint32        = int64(1<<32 - 1)
	putOp            = byte(1)
	deleteOp         = byte(2)
)

var walCRC = crc32.MakeTable(crc32.Castagnoli)

type operation struct {
	kind  byte
	key   []byte
	value []byte
}

type state struct {
	values     map[string][]byte
	tombstones map[string]struct{}
	keys       []string
}

func emptyState() *state {
	return &state{values: make(map[string][]byte), tombstones: make(map[string]struct{})}
}

func candidateState(base *state, ops []operation, opts Options) (*state, error) {
	if len(ops) == 0 || len(ops) > opts.MaxBatchOps {
		return nil, ErrInvalidBatch
	}
	for _, op := range ops {
		if err := validateOperation(op, opts); err != nil {
			return nil, err
		}
	}
	seen := make(map[string]struct{}, len(ops))
	for _, op := range ops {
		key := string(op.key)
		if _, ok := seen[key]; ok {
			return nil, ErrDuplicateKey
		}
		seen[key] = struct{}{}
	}

	next := &state{values: make(map[string][]byte, len(base.values)+len(ops)), tombstones: make(map[string]struct{}, len(base.tombstones)+len(ops))}
	for key, value := range base.values {
		next.values[key] = value
	}
	for key := range base.tombstones {
		next.tombstones[key] = struct{}{}
	}
	for _, op := range ops {
		key := string(op.key)
		if op.kind == deleteOp {
			delete(next.values, key)
			next.tombstones[key] = struct{}{}
			continue
		}
		delete(next.tombstones, key)
		next.values[key] = append([]byte(nil), op.value...)
	}
	next.keys = make([]string, 0, len(next.values))
	for key := range next.values {
		next.keys = append(next.keys, key)
	}
	sort.Slice(next.keys, func(i, j int) bool { return bytes.Compare([]byte(next.keys[i]), []byte(next.keys[j])) < 0 })
	return next, nil
}

func entriesMap(st *state) map[string]memEntry {
	active := make(map[string]memEntry, len(st.values)+len(st.tombstones))
	for key, value := range st.values {
		active[key] = memEntry{kind: putOp, key: []byte(key), value: append([]byte(nil), value...)}
	}
	for key := range st.tombstones {
		active[key] = memEntry{kind: deleteOp, key: []byte(key)}
	}
	return active
}

func cloneMemtable(in map[string]memEntry) map[string]memEntry {
	out := make(map[string]memEntry, len(in))
	for key, entry := range in {
		out[key] = memEntry{kind: entry.kind, key: append([]byte(nil), entry.key...), value: append([]byte(nil), entry.value...)}
	}
	return out
}

func memtableBytes(active map[string]memEntry) int {
	n := 0
	for _, entry := range active {
		n += entry.size()
	}
	return n
}

func applyToMemtable(active map[string]memEntry, bytesUsed *int, ops []operation, opts Options) error {
	for _, op := range ops {
		if err := validateOperation(op, opts); err != nil {
			return err
		}
		key := string(op.key)
		if old, ok := active[key]; ok {
			*bytesUsed -= old.size()
		}
		entry := memEntry{kind: op.kind, key: append([]byte(nil), op.key...), value: append([]byte(nil), op.value...)}
		active[key] = entry
		*bytesUsed += entry.size()
	}
	return nil
}

func validateOperation(op operation, opts Options) error {
	if op.kind != putOp && op.kind != deleteOp {
		return fmt.Errorf("%w: unknown operation kind", ErrCorrupt)
	}
	if err := validateKey(op.key, opts.MaxKeyBytes); err != nil {
		return err
	}
	if op.kind == deleteOp && len(op.value) != 0 {
		return fmt.Errorf("%w: delete has a value", ErrCorrupt)
	}
	if op.kind == putOp && len(op.value) > opts.MaxValueBytes {
		return ErrValueTooLarge
	}
	return nil
}

func canonicalOperations(batch *Batch, opts Options) ([]operation, int, error) {
	if batch == nil || len(batch.ops) == 0 || len(batch.ops) > opts.MaxBatchOps {
		return nil, 0, ErrInvalidBatch
	}
	var payloadLen int64
	for _, op := range batch.ops {
		if err := validateOperation(op, opts); err != nil {
			return nil, 0, err
		}
		payloadLen += int64(9 + len(op.key) + len(op.value))
		if payloadLen > int64(opts.MaxBatchBytes) || payloadLen > maxUint32 {
			return nil, 0, ErrBatchTooLarge
		}
	}
	seen := make(map[string]struct{}, len(batch.ops))
	for _, op := range batch.ops {
		key := string(op.key)
		if _, exists := seen[key]; exists {
			return nil, 0, ErrDuplicateKey
		}
		seen[key] = struct{}{}
	}
	ops := append([]operation(nil), batch.ops...)
	sort.Slice(ops, func(i, j int) bool { return bytes.Compare(ops[i].key, ops[j].key) < 0 })
	return ops, int(payloadLen), nil
}

func encodeFrame(lsn uint64, batch *Batch, opts Options) ([]byte, []operation, error) {
	ops, payloadLen, err := canonicalOperations(batch, opts)
	if err != nil {
		return nil, nil, err
	}
	frameLen := frameHeaderLen + payloadLen + frameTrailerLen
	if int64(frameLen) > int64(opts.SegmentBytes)+int64(opts.MaxBatchBytes) {
		return nil, nil, ErrBatchTooLarge
	}
	frame := make([]byte, frameLen)
	copy(frame[:4], frameMagic)
	frame[4] = walFormat
	frame[5] = batchKind
	// bytes 6..7 are reserved and must remain zero.
	binary.BigEndian.PutUint32(frame[8:12], uint32(payloadLen))
	binary.BigEndian.PutUint64(frame[12:20], lsn)
	binary.BigEndian.PutUint32(frame[20:24], uint32(len(ops)))
	binary.BigEndian.PutUint32(frame[24:28], ^uint32(payloadLen))
	binary.BigEndian.PutUint32(frame[28:32], crc32.Checksum(frame[:28], walCRC))
	off := frameHeaderLen
	for _, op := range ops {
		frame[off] = op.kind
		off++
		binary.BigEndian.PutUint32(frame[off:off+4], uint32(len(op.key)))
		off += 4
		binary.BigEndian.PutUint32(frame[off:off+4], uint32(len(op.value)))
		off += 4
		copy(frame[off:], op.key)
		off += len(op.key)
		copy(frame[off:], op.value)
		off += len(op.value)
	}
	binary.BigEndian.PutUint32(frame[off:off+4], crc32.Checksum(frame[:off], walCRC))
	return frame, ops, nil
}

func encodeSegmentHeader(segment, firstLSN uint64) []byte {
	header := make([]byte, segmentHeaderLen)
	copy(header[:4], segmentMagic)
	header[4] = walFormat
	header[5] = segmentKind
	binary.BigEndian.PutUint64(header[8:16], segment)
	binary.BigEndian.PutUint64(header[16:24], firstLSN)
	binary.BigEndian.PutUint32(header[24:28], crc32.Checksum(header[:24], walCRC))
	return header
}

type wal struct {
	fs     rootFS
	opts   Options
	file   File
	seg    uint64
	size   int64
	closed bool
}

func openWAL(fsys rootFS, opts Options, initial *state, flushedLSN uint64) (*wal, *state, uint64, error) {
	if err := ensureDirectory(fsys, "wal"); err != nil {
		return nil, nil, 0, err
	}
	segments, err := listSegments(fsys)
	if err != nil {
		return nil, nil, 0, err
	}
	if initial == nil {
		initial = emptyState()
	}
	st := initial
	var lsn uint64
	for index, seg := range segments {
		last := index == len(segments)-1
		file, size, openErr := openExistingSegment(fsys, seg)
		if openErr != nil {
			return nil, nil, 0, openErr
		}
		if index == 0 {
			firstLSN, headerErr := segmentFirstLSN(file, size)
			if headerErr != nil {
				_ = file.Close()
				return nil, nil, 0, headerErr
			}
			if firstLSN == 0 || firstLSN > flushedLSN+1 {
				_ = file.Close()
				return nil, nil, 0, fmt.Errorf("%w: WAL starts after manifest checkpoint", ErrCorrupt)
			}
			lsn = firstLSN - 1
		}
		st, lsn, size, err = recoverSegment(file, size, seg, lsn, st, opts, last, flushedLSN)
		if err != nil {
			_ = file.Close()
			return nil, nil, 0, err
		}
		if size == 0 && last {
			_ = file.Close()
			if removeErr := fsys.Remove(filepathSegment(seg)); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
				return nil, nil, 0, fmt.Errorf("%w: remove empty tail: %v", ErrFilesystem, removeErr)
			}
			if syncErr := syncDirectory(fsys, "wal"); syncErr != nil {
				return nil, nil, 0, syncErr
			}
			segments = segments[:len(segments)-1]
			if len(segments) > 0 {
				previous := segments[len(segments)-1]
				previousFile, previousSize, previousErr := openExistingSegment(fsys, previous)
				if previousErr != nil {
					return nil, nil, 0, previousErr
				}
				return &wal{fs: fsys, opts: opts, file: previousFile, seg: previous, size: previousSize}, st, lsn, nil
			}
			continue
		}
		if !last && size == segmentHeaderLen {
			_ = file.Close()
			return nil, nil, 0, fmt.Errorf("%w: empty interior segment", ErrCorrupt)
		}
		if last {
			if lsn < flushedLSN {
				_ = file.Close()
				return nil, nil, 0, fmt.Errorf("%w: WAL ends before manifest checkpoint", ErrCorrupt)
			}
			return &wal{fs: fsys, opts: opts, file: file, seg: seg, size: size}, st, lsn, nil
		}
		if closeErr := file.Close(); closeErr != nil {
			return nil, nil, 0, fmt.Errorf("%w: close segment: %v", ErrFilesystem, closeErr)
		}
	}
	// No segments, or a lone empty tail, starts at segment one.
	file, size, err := createSegment(fsys, 1, flushedLSN+1)
	if err != nil {
		return nil, nil, 0, err
	}
	return &wal{fs: fsys, opts: opts, file: file, seg: 1, size: size}, st, flushedLSN, nil
}

func listSegments(fsys rootFS) ([]uint64, error) {
	entries, err := fsys.ReadDir("wal")
	if err != nil {
		return nil, fmt.Errorf("%w: list WAL: %v", ErrFilesystem, err)
	}
	segments := make([]uint64, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		seg, ok := validSegmentName(name)
		if !ok {
			return nil, fmt.Errorf("%w: unexpected WAL member %q", ErrCorrupt, name)
		}
		info, err := fsys.Lstat("wal/" + name)
		if err != nil {
			return nil, fmt.Errorf("%w: stat WAL member: %v", ErrFilesystem, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: WAL member %q is not a regular file", ErrCorrupt, name)
		}
		segments = append(segments, seg)
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i] < segments[j] })
	for i := 1; i < len(segments); i++ {
		if segments[i] != segments[0]+uint64(i) {
			return nil, fmt.Errorf("%w: WAL segment gap at %d", ErrCorrupt, i+1)
		}
	}
	return segments, nil
}

func filepathSegment(seg uint64) string { return "wal/" + segmentName(seg) }

func openExistingSegment(fsys rootFS, seg uint64) (File, int64, error) {
	file, err := fsys.OpenFile(filepathSegment(seg), os.O_RDWR, 0o600)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: open segment: %v", ErrFilesystem, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, fmt.Errorf("%w: stat segment: %v", ErrFilesystem, err)
	}
	return file, info.Size(), nil
}

func createSegment(fsys rootFS, seg, firstLSN uint64) (File, int64, error) {
	file, err := fsys.OpenFile(filepathSegment(seg), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: create segment: %v", ErrFilesystem, err)
	}
	header := encodeSegmentHeader(seg, firstLSN)
	if err := writeFullAt(file, header, 0); err != nil {
		_ = file.Close()
		return nil, 0, fmt.Errorf("%w: write segment header: %v", ErrFilesystem, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, 0, fmt.Errorf("%w: sync segment header: %v", ErrFilesystem, err)
	}
	if err := syncDirectory(fsys, "wal"); err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	return file, int64(len(header)), nil
}

func syncDirectory(fsys rootFS, name string) error {
	dir, err := fsys.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("%w: open directory for sync: %v", ErrFilesystem, err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("%w: sync directory: %v", ErrFilesystem, err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("%w: close directory: %v", ErrFilesystem, err)
	}
	return nil
}

func recoverSegment(file File, size int64, seg, lastLSN uint64, st *state, opts Options, final bool, flushedLSN uint64) (*state, uint64, int64, error) {
	maxSize := int64(opts.SegmentBytes)
	minimumOversize := int64(opts.MaxBatchBytes + frameHeaderLen + frameTrailerLen + segmentHeaderLen)
	if minimumOversize > maxSize {
		maxSize = minimumOversize
	}
	if size < segmentHeaderLen {
		if !final {
			return nil, 0, 0, fmt.Errorf("%w: short interior segment header", ErrCorrupt)
		}
		if err := truncateAndSync(file, 0); err != nil {
			return nil, 0, 0, err
		}
		return st, lastLSN, 0, nil
	}
	if size > maxSize {
		return nil, 0, 0, fmt.Errorf("%w: segment exceeds configured bound", ErrCorrupt)
	}
	header := make([]byte, segmentHeaderLen)
	if err := readFullAt(file, header, 0); err != nil {
		return nil, 0, 0, fmt.Errorf("%w: segment header: %v", ErrCorrupt, err)
	}
	if string(header[:4]) != segmentMagic || header[4] != walFormat || header[5] != segmentKind ||
		binary.BigEndian.Uint16(header[6:8]) != 0 || binary.BigEndian.Uint32(header[24:28]) != crc32.Checksum(header[:24], walCRC) {
		return nil, 0, 0, fmt.Errorf("%w: invalid segment header", ErrCorrupt)
	}
	if binary.BigEndian.Uint64(header[8:16]) != seg || binary.BigEndian.Uint64(header[16:24]) != lastLSN+1 {
		return nil, 0, 0, fmt.Errorf("%w: non-contiguous segment header", ErrCorrupt)
	}
	offset := int64(segmentHeaderLen)
	for offset < size {
		remaining := size - offset
		if remaining < frameHeaderLen {
			if !final {
				return nil, 0, 0, fmt.Errorf("%w: short interior frame header", ErrCorrupt)
			}
			if err := truncateAndSync(file, offset); err != nil {
				return nil, 0, 0, err
			}
			return st, lastLSN, offset, nil
		}
		fh := make([]byte, frameHeaderLen)
		if err := readFullAt(file, fh, offset); err != nil {
			return nil, 0, 0, fmt.Errorf("%w: frame header: %v", ErrCorrupt, err)
		}
		if string(fh[:4]) != frameMagic || fh[4] != walFormat || fh[5] != batchKind ||
			binary.BigEndian.Uint16(fh[6:8]) != 0 || binary.BigEndian.Uint32(fh[24:28]) != ^binary.BigEndian.Uint32(fh[8:12]) || binary.BigEndian.Uint32(fh[28:32]) != crc32.Checksum(fh[:28], walCRC) {
			return nil, 0, 0, fmt.Errorf("%w: invalid frame header", ErrCorrupt)
		}
		payloadLen := int64(binary.BigEndian.Uint32(fh[8:12]))
		if payloadLen < 1 || payloadLen > int64(opts.MaxBatchBytes) {
			return nil, 0, 0, fmt.Errorf("%w: invalid frame length", ErrCorrupt)
		}
		frameLen := int64(frameHeaderLen) + payloadLen + frameTrailerLen
		if frameLen > remaining {
			if !final {
				return nil, 0, 0, fmt.Errorf("%w: short interior frame", ErrCorrupt)
			}
			if err := truncateAndSync(file, offset); err != nil {
				return nil, 0, 0, err
			}
			return st, lastLSN, offset, nil
		}
		frame := make([]byte, frameLen)
		if err := readFullAt(file, frame, offset); err != nil {
			return nil, 0, 0, fmt.Errorf("%w: frame: %v", ErrCorrupt, err)
		}
		if binary.BigEndian.Uint32(frame[len(frame)-4:]) != crc32.Checksum(frame[:len(frame)-4], walCRC) {
			return nil, 0, 0, fmt.Errorf("%w: frame checksum", ErrCorrupt)
		}
		if binary.BigEndian.Uint64(frame[12:20]) != lastLSN+1 {
			return nil, 0, 0, fmt.Errorf("%w: LSN gap or duplicate", ErrCorrupt)
		}
		ops, err := decodePayload(frame[frameHeaderLen:frameHeaderLen+int(payloadLen)], binary.BigEndian.Uint32(frame[20:24]), opts)
		if err != nil {
			return nil, 0, 0, err
		}
		if lastLSN+1 > flushedLSN {
			st, err = candidateState(st, ops, opts)
			if err != nil {
				return nil, 0, 0, fmt.Errorf("%w: recovered batch: %v", ErrCorrupt, err)
			}
		}
		lastLSN++
		offset += frameLen
	}
	return st, lastLSN, offset, nil
}

func segmentFirstLSN(file File, size int64) (uint64, error) {
	if size < segmentHeaderLen {
		return 0, fmt.Errorf("%w: short segment header", ErrCorrupt)
	}
	header := make([]byte, segmentHeaderLen)
	if err := readFullAt(file, header, 0); err != nil {
		return 0, fmt.Errorf("%w: segment header: %v", ErrCorrupt, err)
	}
	if string(header[:4]) != segmentMagic || header[4] != walFormat || header[5] != segmentKind ||
		binary.BigEndian.Uint16(header[6:8]) != 0 || binary.BigEndian.Uint32(header[24:28]) != crc32.Checksum(header[:24], walCRC) {
		return 0, fmt.Errorf("%w: invalid segment header", ErrCorrupt)
	}
	return binary.BigEndian.Uint64(header[16:24]), nil
}

func decodePayload(payload []byte, count uint32, opts Options) ([]operation, error) {
	if count == 0 || count > uint32(opts.MaxBatchOps) {
		return nil, fmt.Errorf("%w: invalid operation count", ErrCorrupt)
	}
	ops := make([]operation, 0, int(count))
	off := 0
	var previous []byte
	for i := uint32(0); i < count; i++ {
		if len(payload)-off < 9 {
			return nil, fmt.Errorf("%w: truncated operation", ErrCorrupt)
		}
		kind := payload[off]
		off++
		keyLen := int(binary.BigEndian.Uint32(payload[off : off+4]))
		off += 4
		valueLen := int(binary.BigEndian.Uint32(payload[off : off+4]))
		off += 4
		if keyLen == 0 || keyLen > opts.MaxKeyBytes || valueLen > opts.MaxValueBytes || (kind == deleteOp && valueLen != 0) || (kind != putOp && kind != deleteOp) {
			return nil, fmt.Errorf("%w: invalid operation bounds", ErrCorrupt)
		}
		if keyLen > len(payload)-off || valueLen > len(payload)-off-keyLen {
			return nil, fmt.Errorf("%w: operation exceeds payload", ErrCorrupt)
		}
		key := append([]byte(nil), payload[off:off+keyLen]...)
		off += keyLen
		value := append([]byte(nil), payload[off:off+valueLen]...)
		off += valueLen
		if previous != nil && bytes.Compare(previous, key) >= 0 {
			return nil, fmt.Errorf("%w: operations are not canonical", ErrCorrupt)
		}
		previous = key
		ops = append(ops, operation{kind: kind, key: key, value: value})
	}
	if off != len(payload) {
		return nil, fmt.Errorf("%w: trailing payload bytes", ErrCorrupt)
	}
	return ops, nil
}

func truncateAndSync(file File, size int64) error {
	if err := file.Truncate(size); err != nil {
		return fmt.Errorf("%w: truncate WAL tail: %v", ErrFilesystem, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("%w: sync WAL truncation: %v", ErrFilesystem, err)
	}
	return nil
}

func (w *wal) appendFrame(frame []byte, nextLSN uint64) error {
	if w.closed || w.file == nil {
		return ErrClosed
	}
	if w.size > segmentHeaderLen && w.size+int64(len(frame)) > int64(w.opts.SegmentBytes) {
		if err := w.file.Close(); err != nil {
			return err
		}
		file, size, err := createSegment(w.fs, w.seg+1, nextLSN)
		if err != nil {
			return err
		}
		w.file, w.seg, w.size = file, w.seg+1, size
	}
	if err := writeFullAt(w.file, frame, w.size); err != nil {
		return err
	}
	w.size += int64(len(frame))
	if err := w.file.Sync(); err != nil {
		return err
	}
	return nil
}

// rotate seals the current WAL file from the writer's point of view and
// creates a durable empty successor. The old file remains on disk until a
// manifest containing a checkpoint beyond it has been published.
func (w *wal) rotate(nextLSN uint64) error {
	if w.closed || w.file == nil {
		return ErrClosed
	}
	if w.size == segmentHeaderLen {
		return nil
	}
	if err := w.file.Sync(); err != nil {
		return err
	}
	if err := w.file.Close(); err != nil {
		return err
	}
	file, size, err := createSegment(w.fs, w.seg+1, nextLSN)
	if err != nil {
		return err
	}
	w.file, w.seg, w.size = file, w.seg+1, size
	return nil
}

func (w *wal) removeBefore(segment uint64) error {
	segments, err := listSegments(w.fs)
	if err != nil {
		return err
	}
	for _, seg := range segments {
		if seg >= segment {
			continue
		}
		if err := w.fs.Remove(filepathSegment(seg)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: remove WAL segment: %v", ErrFilesystem, err)
		}
	}
	return syncDirectory(w.fs, "wal")
}

func (w *wal) close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.file == nil {
		return nil
	}
	return w.file.Close()
}

func writeFullAt(file File, data []byte, offset int64) error {
	for len(data) > 0 {
		n, err := file.WriteAt(data, offset)
		if n < 0 || n > len(data) {
			return ErrShortWrite
		}
		if n > 0 {
			offset += int64(n)
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrShortWrite
		}
	}
	return nil
}

func readFullAt(file File, data []byte, offset int64) error {
	for len(data) > 0 {
		n, err := file.ReadAt(data, offset)
		if n < 0 || n > len(data) {
			return io.ErrUnexpectedEOF
		}
		if n > 0 {
			offset += int64(n)
			data = data[n:]
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(data) == 0 {
				return nil
			}
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}
