package engine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sort"
)

// sstReader is a bounded table reader. The index is retained (bounded by the
// configured table size); data is loaded one block at a time.
type sstReader struct {
	fs         rootFS
	file       sstFile
	f          File
	opts       Options
	blocks     []sstBlockMeta
	entryCount uint32
	bodyLen    int64
	bodyCRC    uint32
	block      int
	entries    []sstEntry
	index      int
	closed     bool
}

func validateSSTFiles(files []sstFile, fsys rootFS, opts Options) error {
	for _, file := range files {
		reader, err := openSSTReader(fsys, file, opts, true)
		if err != nil {
			return err
		}
		if err := reader.close(); err != nil {
			return fmt.Errorf("%w: close SST: %v", ErrCorrupt, err)
		}
	}
	return nil
}

func openSSTReader(fsys rootFS, file sstFile, opts Options, validate bool) (*sstReader, error) {
	if !validSSTName(file.name) || file.level > 1 {
		return nil, fmt.Errorf("%w: invalid SST reference", ErrCorrupt)
	}
	f, err := fsys.OpenFile("sst/"+file.name, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open SST %q: %v", ErrCorrupt, file.name, err)
	}
	r := &sstReader{fs: fsys, file: file, f: f, opts: opts}
	if err := r.loadIndex(); err != nil {
		_ = f.Close()
		return nil, err
	}
	if validate {
		if err := r.validateAll(); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return r, nil
}

func (r *sstReader) loadIndex() error {
	info, err := r.f.Stat()
	if err != nil {
		return fmt.Errorf("%w: stat SST: %v", ErrCorrupt, err)
	}
	size := info.Size()
	if size < sstHeaderLen+sstFooterLen || size > int64(r.opts.MaxSSTBytes) || (r.file.bytes != 0 && r.file.bytes != size) {
		return fmt.Errorf("%w: invalid SST size", ErrCorrupt)
	}
	header := make([]byte, sstHeaderLen)
	if err := readFullAt(r.f, header, 0); err != nil {
		return fmt.Errorf("%w: read SST header: %v", ErrCorrupt, err)
	}
	if string(header[:4]) != sstMagic || header[4] != sstVersion || header[5] != r.file.level || binary.BigEndian.Uint16(header[6:8]) != 0 || binary.BigEndian.Uint32(header[36:40]) != crc32.Checksum(header[:36], walCRC) {
		return fmt.Errorf("%w: invalid SST header", ErrCorrupt)
	}
	id, ok := parseNumberName(r.file.name, "sst-", ".sst")
	if !ok || binary.BigEndian.Uint64(header[8:16]) != id {
		return fmt.Errorf("%w: SST name/id mismatch", ErrCorrupt)
	}
	entryCount := binary.BigEndian.Uint32(header[24:28])
	blockCount := binary.BigEndian.Uint32(header[28:32])
	bodyLen := int64(binary.BigEndian.Uint32(header[32:36]))
	if entryCount == 0 || blockCount == 0 || bodyLen < blockHeaderLen || int64(sstHeaderLen)+bodyLen+sstFooterLen > size {
		return fmt.Errorf("%w: invalid SST bounds", ErrCorrupt)
	}
	footerAt := size - sstFooterLen
	footer := make([]byte, sstFooterLen)
	if err := readFullAt(r.f, footer, footerAt); err != nil {
		return fmt.Errorf("%w: read SST footer: %v", ErrCorrupt, err)
	}
	if string(footer[:4]) != footerMagic || footer[4] != sstVersion || footer[5] != 0 || binary.BigEndian.Uint16(footer[6:8]) != 0 || binary.BigEndian.Uint32(footer[20:24]) != blockCount || binary.BigEndian.Uint32(footer[28:32]) != crc32.Checksum(footer[:28], walCRC) {
		return fmt.Errorf("%w: invalid SST footer", ErrCorrupt)
	}
	indexOffset := int64(binary.BigEndian.Uint64(footer[8:16]))
	indexLen := int64(binary.BigEndian.Uint32(footer[16:20]))
	if indexOffset != int64(sstHeaderLen)+bodyLen || indexLen < 1 || indexOffset+indexLen != footerAt || indexLen > int64(r.opts.MaxSSTBytes) {
		return fmt.Errorf("%w: invalid SST index bounds", ErrCorrupt)
	}
	index := make([]byte, indexLen)
	if err := readFullAt(r.f, index, indexOffset); err != nil {
		return fmt.Errorf("%w: read SST index: %v", ErrCorrupt, err)
	}
	r.blocks = make([]sstBlockMeta, 0, blockCount)
	pos := 0
	var expectedOffset uint64
	var previousLast []byte
	for i := uint32(0); i < blockCount; i++ {
		if len(index)-pos < 20 {
			return fmt.Errorf("%w: truncated SST index", ErrCorrupt)
		}
		offset := binary.BigEndian.Uint64(index[pos : pos+8])
		length := binary.BigEndian.Uint32(index[pos+8 : pos+12])
		firstLen := int(binary.BigEndian.Uint32(index[pos+12 : pos+16]))
		lastLen := int(binary.BigEndian.Uint32(index[pos+16 : pos+20]))
		pos += 20
		if firstLen < 1 || lastLen < 1 || firstLen > r.opts.MaxKeyBytes || lastLen > r.opts.MaxKeyBytes || len(index)-pos < firstLen+lastLen {
			return fmt.Errorf("%w: invalid SST index key bounds", ErrCorrupt)
		}
		first := append([]byte(nil), index[pos:pos+firstLen]...)
		last := append([]byte(nil), index[pos+firstLen:pos+firstLen+lastLen]...)
		pos += firstLen + lastLen
		if bytes.Compare(first, last) > 0 || (previousLast != nil && bytes.Compare(previousLast, first) >= 0) {
			return fmt.Errorf("%w: nonordered SST index", ErrCorrupt)
		}
		bodyOffset := int64(offset)
		if bodyOffset < 0 || bodyOffset+int64(length) > bodyLen || length < blockHeaderLen || offset != expectedOffset {
			return fmt.Errorf("%w: SST block index bounds", ErrCorrupt)
		}
		r.blocks = append(r.blocks, sstBlockMeta{offset: offset, length: length, first: first, last: last})
		expectedOffset += uint64(length)
		previousLast = last
	}
	if pos != len(index) || uint32(len(r.blocks)) != blockCount || int64(expectedOffset) != bodyLen {
		return fmt.Errorf("%w: SST index count mismatch", ErrCorrupt)
	}
	r.entryCount = entryCount
	r.bodyLen = bodyLen
	r.bodyCRC = binary.BigEndian.Uint32(footer[24:28])
	return nil
}

func (r *sstReader) validateAll() error {
	h := crc32.New(walCRC)
	var total uint32
	var previous []byte
	for i := range r.blocks {
		block, raw, err := r.readBlock(i)
		if err != nil {
			return err
		}
		_, _ = h.Write(raw)
		if len(block) == 0 || !bytes.Equal(block[0].key, r.blocks[i].first) || !bytes.Equal(block[len(block)-1].key, r.blocks[i].last) {
			return fmt.Errorf("%w: SST block metadata mismatch", ErrCorrupt)
		}
		for _, entry := range block {
			if previous != nil && bytes.Compare(previous, entry.key) >= 0 {
				return fmt.Errorf("%w: SST keys are not strictly ordered", ErrCorrupt)
			}
			previous = entry.key
		}
		total += uint32(len(block))
	}
	if total != r.entryCount || h.Sum32() != r.bodyCRC {
		return fmt.Errorf("%w: SST count/body checksum mismatch", ErrCorrupt)
	}
	if len(r.blocks) == 0 || !bytes.Equal(r.blocks[0].first, r.file.minKey) || !bytes.Equal(r.blocks[len(r.blocks)-1].last, r.file.maxKey) || r.file.entryCount != total {
		return fmt.Errorf("%w: SST manifest metadata mismatch", ErrCorrupt)
	}
	return nil
}

func (r *sstReader) readBlock(index int) ([]sstEntry, []byte, error) {
	if index < 0 || index >= len(r.blocks) {
		return nil, nil, io.EOF
	}
	meta := r.blocks[index]
	raw := make([]byte, meta.length)
	if err := readFullAt(r.f, raw, int64(sstHeaderLen)+int64(meta.offset)); err != nil {
		return nil, nil, fmt.Errorf("%w: read SST block: %v", ErrCorrupt, err)
	}
	if len(raw) < blockHeaderLen || string(raw[:4]) != blockMagic {
		return nil, nil, fmt.Errorf("%w: invalid SST block", ErrCorrupt)
	}
	payloadLen := int(binary.BigEndian.Uint32(raw[4:8]))
	count := binary.BigEndian.Uint32(raw[8:12])
	if payloadLen < 1 || payloadLen > maxSSTBlockPayload(r.opts) || count == 0 || count > uint32(payloadLen/13) || payloadLen+blockHeaderLen != len(raw) || binary.BigEndian.Uint32(raw[12:16]) != crc32.Checksum(raw[16:], walCRC) {
		return nil, nil, fmt.Errorf("%w: invalid SST block bounds/checksum", ErrCorrupt)
	}
	entries, err := decodeSSTBlock(raw[16:], count, r.opts)
	if err != nil {
		return nil, nil, err
	}
	return entries, raw, nil
}

func decodeSSTBlock(payload []byte, count uint32, opts Options) ([]sstEntry, error) {
	if count == 0 || count > uint32(len(payload)/13) {
		return nil, fmt.Errorf("%w: invalid SST entry count", ErrCorrupt)
	}
	entries := make([]sstEntry, 0, count)
	pos := 0
	for i := uint32(0); i < count; i++ {
		if len(payload)-pos < 1+4+4+4 {
			return nil, fmt.Errorf("%w: truncated SST entry", ErrCorrupt)
		}
		kind := payload[pos]
		keyLen := int(binary.BigEndian.Uint32(payload[pos+1 : pos+5]))
		valueLen := int(binary.BigEndian.Uint32(payload[pos+5 : pos+9]))
		recordLen := 1 + 4 + 4 + keyLen + valueLen + 4
		if keyLen < 1 || keyLen > opts.MaxKeyBytes || valueLen > opts.MaxValueBytes || (kind == deleteOp && valueLen != 0) || (kind != putOp && kind != deleteOp) || recordLen < 13 || recordLen > len(payload)-pos {
			return nil, fmt.Errorf("%w: invalid SST entry bounds", ErrCorrupt)
		}
		record := payload[pos : pos+recordLen]
		if binary.BigEndian.Uint32(record[len(record)-4:]) != crc32.Checksum(record[:len(record)-4], walCRC) {
			return nil, fmt.Errorf("%w: SST entry checksum", ErrCorrupt)
		}
		entries = append(entries, sstEntry{kind: kind, key: append([]byte(nil), record[9:9+keyLen]...), value: append([]byte(nil), record[9+keyLen:9+keyLen+valueLen]...)})
		pos += recordLen
	}
	if pos != len(payload) {
		return nil, fmt.Errorf("%w: SST block trailing bytes", ErrCorrupt)
	}
	for i := 1; i < len(entries); i++ {
		if bytes.Compare(entries[i-1].key, entries[i].key) >= 0 {
			return nil, fmt.Errorf("%w: SST block keys are not ordered", ErrCorrupt)
		}
	}
	return entries, nil
}

func (r *sstReader) seek(start []byte) error {
	if len(r.blocks) == 0 {
		return nil
	}
	i := 0
	if start != nil {
		i = sort.Search(len(r.blocks), func(i int) bool { return bytes.Compare(r.blocks[i].last, start) >= 0 })
	}
	r.block = i
	r.entries = nil
	r.index = 0
	if i >= len(r.blocks) {
		return nil
	}
	if err := r.loadCurrent(); err != nil {
		return err
	}
	if start != nil {
		r.index = sort.Search(len(r.entries), func(i int) bool { return bytes.Compare(r.entries[i].key, start) >= 0 })
	}
	return nil
}

func (r *sstReader) loadCurrent() error {
	entries, _, err := r.readBlock(r.block)
	if err != nil {
		return err
	}
	r.entries, r.index = entries, 0
	return nil
}

func (r *sstReader) next() (sstEntry, bool, error) {
	if r.closed {
		return sstEntry{}, false, ErrClosed
	}
	for r.block < len(r.blocks) {
		if r.entries == nil {
			if err := r.loadCurrent(); err != nil {
				return sstEntry{}, false, err
			}
		}
		if r.index < len(r.entries) {
			entry := copyEntry(r.entries[r.index])
			r.index++
			return entry, true, nil
		}
		r.block++
		r.entries = nil
		r.index = 0
	}
	return sstEntry{}, false, nil
}

func (r *sstReader) close() error {
	if r == nil || r.closed {
		return nil
	}
	r.closed = true
	return r.f.Close()
}

func lookupSST(fsys rootFS, file sstFile, key []byte, opts Options) (sstEntry, bool, error) {
	r, err := openSSTReader(fsys, file, opts, false)
	if err != nil {
		return sstEntry{}, false, err
	}
	defer r.close()
	if err := r.seek(key); err != nil {
		return sstEntry{}, false, err
	}
	entry, ok, err := r.next()
	if err != nil || !ok || !bytes.Equal(entry.key, key) {
		return sstEntry{}, false, err
	}
	return entry, true, nil
}
