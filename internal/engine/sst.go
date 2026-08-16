package engine

// This file contains the intentionally small v1 SST format. It uses bounded,
// independently checksummed blocks rather than a streaming record file: a
// reader can reject a malformed length before allocating, and a corrupt block
// cannot be mistaken for an empty range. There is no compression or mmap in
// this first owned format.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"sort"
)

const (
	sstMagic         = "VBS1"
	sstVersion       = byte(1)
	sstHeaderLen     = 40
	blockMagic       = "BLK1"
	blockHeaderLen   = 16
	footerMagic      = "VSF1"
	sstFooterLen     = 32
	maxSSTNameLen    = 255
	maxManifestFiles = 4096
)

type memEntry struct {
	kind  byte
	key   []byte
	value []byte
}

func (m memEntry) size() int { return 1 + 4 + 4 + len(m.key) + len(m.value) }

type sstEntry = memEntry

type sstFile struct {
	name       string
	level      byte
	minKey     []byte
	maxKey     []byte
	entryCount uint32
	bytes      int64
}

func sstName(id uint64) string     { return fmt.Sprintf("sst/sst-%020d.sst", id) }
func sstBaseName(id uint64) string { return fmt.Sprintf("sst-%020d.sst", id) }

func validSSTName(name string) bool {
	if len(name) != len("sst-")+20+len(".sst") || len(name) > maxSSTNameLen ||
		len(name) < 9 || name[:4] != "sst-" || name[len(name)-4:] != ".sst" {
		return false
	}
	for _, c := range name[4 : len(name)-4] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func encodeSST(entries []sstEntry, level byte, id, maxLSN uint64, opts Options) ([]byte, sstFile, error) {
	if len(entries) == 0 {
		return nil, sstFile{}, fmt.Errorf("%w: empty SST", ErrInvalidBatch)
	}
	if uint64(len(entries)) > uint64(^uint32(0)) {
		return nil, sstFile{}, fmt.Errorf("%w: too many SST entries", ErrBatchTooLarge)
	}
	sort.Slice(entries, func(i, j int) bool { return bytes.Compare(entries[i].key, entries[j].key) < 0 })
	for i := range entries {
		if err := validateOperation(operation{kind: entries[i].kind, key: entries[i].key, value: entries[i].value}, opts); err != nil {
			return nil, sstFile{}, err
		}
		if i > 0 && bytes.Equal(entries[i-1].key, entries[i].key) {
			return nil, sstFile{}, fmt.Errorf("%w: duplicate SST key", ErrDuplicateKey)
		}
	}

	var body bytes.Buffer
	indexes := make([]sstBlockIndex, 0, (len(entries)+31)/32)
	for start := 0; start < len(entries); {
		blockStart := body.Len()
		var payload bytes.Buffer
		count := 0
		for start+count < len(entries) {
			entry := entries[start+count]
			recordLen := 1 + 4 + 4 + len(entry.key) + len(entry.value) + 4
			if recordLen > maxSSTBlockPayload(opts) {
				return nil, sstFile{}, fmt.Errorf("%w: entry exceeds SST block", ErrBatchTooLarge)
			}
			if payload.Len() > 0 && payload.Len()+recordLen > opts.SSTBlockBytes {
				break
			}
			record := make([]byte, recordLen)
			record[0] = entry.kind
			binary.BigEndian.PutUint32(record[1:5], uint32(len(entry.key)))
			binary.BigEndian.PutUint32(record[5:9], uint32(len(entry.value)))
			copy(record[9:], entry.key)
			copy(record[9+len(entry.key):], entry.value)
			binary.BigEndian.PutUint32(record[len(record)-4:], crc32.Checksum(record[:len(record)-4], walCRC))
			payload.Write(record)
			count++
		}
		if count == 0 {
			return nil, sstFile{}, fmt.Errorf("%w: SST block made no progress", ErrBatchTooLarge)
		}
		block := make([]byte, blockHeaderLen+payload.Len())
		copy(block[:4], blockMagic)
		binary.BigEndian.PutUint32(block[4:8], uint32(payload.Len()))
		binary.BigEndian.PutUint32(block[8:12], uint32(count))
		binary.BigEndian.PutUint32(block[12:16], crc32.Checksum(payload.Bytes(), walCRC))
		copy(block[blockHeaderLen:], payload.Bytes())
		body.Write(block)
		indexes = append(indexes, sstBlockIndex{offset: uint64(blockStart), length: uint32(len(block)), firstKey: append([]byte(nil), entries[start].key...), lastKey: append([]byte(nil), entries[start+count-1].key...)})
		start += count
	}

	indexOffset := uint64(sstHeaderLen + body.Len())
	var index bytes.Buffer
	for _, block := range indexes {
		if len(block.firstKey) > opts.MaxKeyBytes || len(block.lastKey) > opts.MaxKeyBytes {
			return nil, sstFile{}, ErrInvalidKey
		}
		var hdr [20]byte
		binary.BigEndian.PutUint64(hdr[:8], block.offset)
		binary.BigEndian.PutUint32(hdr[8:12], block.length)
		binary.BigEndian.PutUint32(hdr[12:16], uint32(len(block.firstKey)))
		binary.BigEndian.PutUint32(hdr[16:20], uint32(len(block.lastKey)))
		index.Write(hdr[:])
		index.Write(block.firstKey)
		index.Write(block.lastKey)
	}
	if index.Len() > int(^uint32(0)) || len(indexes) > int(^uint32(0)) || body.Len() > int(^uint32(0)) {
		return nil, sstFile{}, fmt.Errorf("%w: SST index too large", ErrBatchTooLarge)
	}
	bodyCRC := crc32.Checksum(body.Bytes(), walCRC)
	var footer [sstFooterLen]byte
	copy(footer[:4], footerMagic)
	footer[4] = sstVersion
	binary.BigEndian.PutUint64(footer[8:16], indexOffset)
	binary.BigEndian.PutUint32(footer[16:20], uint32(index.Len()))
	binary.BigEndian.PutUint32(footer[20:24], uint32(len(indexes)))
	binary.BigEndian.PutUint32(footer[24:28], bodyCRC)
	binary.BigEndian.PutUint32(footer[28:32], crc32.Checksum(footer[:28], walCRC))

	file := make([]byte, sstHeaderLen+body.Len()+index.Len()+len(footer))
	copy(file[0:4], sstMagic)
	file[4] = sstVersion
	file[5] = level
	binary.BigEndian.PutUint64(file[8:16], id)
	binary.BigEndian.PutUint64(file[16:24], maxLSN)
	binary.BigEndian.PutUint32(file[24:28], uint32(len(entries)))
	binary.BigEndian.PutUint32(file[28:32], uint32(len(indexes)))
	binary.BigEndian.PutUint32(file[32:36], uint32(body.Len()))
	binary.BigEndian.PutUint32(file[36:40], crc32.Checksum(file[:36], walCRC))
	off := sstHeaderLen
	copy(file[off:], body.Bytes())
	off += body.Len()
	copy(file[off:], index.Bytes())
	off += index.Len()
	copy(file[off:], footer[:])
	if int64(len(file)) > int64(opts.MaxSSTBytes) {
		return nil, sstFile{}, fmt.Errorf("%w: SST exceeds configured bound", ErrBatchTooLarge)
	}
	meta := sstFile{name: sstBaseName(id), level: level, minKey: append([]byte(nil), entries[0].key...), maxKey: append([]byte(nil), entries[len(entries)-1].key...), entryCount: uint32(len(entries)), bytes: int64(len(file))}
	return file, meta, nil
}

type sstBlockIndex struct {
	offset   uint64
	length   uint32
	firstKey []byte
	lastKey  []byte
}

type sstBlockMeta struct {
	offset uint64
	length uint32
	count  uint32
	first  []byte
	last   []byte
}

func maxSSTBlockPayload(opts Options) int {
	// A bounded value may legitimately exceed the normal block target. It is
	// placed in one oversized block whose size is still bounded by key/value
	// limits, rather than rejected by the target block size.
	maxRecord := 1 + 4 + 4 + opts.MaxKeyBytes + opts.MaxValueBytes + 4
	if maxRecord > opts.SSTBlockBytes {
		return maxRecord
	}
	return opts.SSTBlockBytes
}

func writeSST(fsys rootFS, entries []sstEntry, level byte, id, maxLSN uint64, opts Options) (sstFile, error) {
	data, meta, err := encodeSST(entries, level, id, maxLSN, opts)
	if err != nil {
		return sstFile{}, err
	}
	tmp := "sst/." + meta.name + ".tmp"
	path := "sst/" + meta.name
	_ = fsys.Remove(tmp)
	f, err := fsys.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return sstFile{}, fmt.Errorf("%w: create SST: %v", ErrFilesystem, err)
	}
	if err := writeFullAt(f, data, 0); err != nil {
		_ = f.Close()
		_ = fsys.Remove(tmp)
		return sstFile{}, fmt.Errorf("%w: write SST: %v", ErrFilesystem, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = fsys.Remove(tmp)
		return sstFile{}, fmt.Errorf("%w: sync SST: %v", ErrFilesystem, err)
	}
	if err := f.Close(); err != nil {
		_ = fsys.Remove(tmp)
		return sstFile{}, fmt.Errorf("%w: close SST: %v", ErrFilesystem, err)
	}
	if err := fsys.Rename(tmp, path); err != nil {
		_ = fsys.Remove(tmp)
		return sstFile{}, fmt.Errorf("%w: publish SST: %v", ErrFilesystem, err)
	}
	if err := syncDirectory(fsys, "sst"); err != nil {
		return sstFile{}, err
	}
	return meta, nil
}

func entriesFromActive(active map[string]memEntry) []sstEntry {
	entries := make([]sstEntry, 0, len(active))
	for _, entry := range active {
		entries = append(entries, sstEntry{kind: entry.kind, key: append([]byte(nil), entry.key...), value: append([]byte(nil), entry.value...)})
	}
	return entries
}
