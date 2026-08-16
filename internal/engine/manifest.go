package engine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

const (
	manifestMagic     = "VBMF"
	manifestVersion   = byte(1)
	manifestHeaderLen = 44
	currentMagic      = "VBCT"
	currentVersion    = byte(1)
	currentHeaderLen  = 8
	manifestMaxBytes  = 1 << 20
)

type manifest struct {
	generation uint64
	flushedLSN uint64
	files      []sstFile
	nextSST    uint64
}

func validManifestName(name string) bool {
	return len(name) == len("MANIFEST-")+20 && strings.HasPrefix(name, "MANIFEST-") && allDigits(name[len("MANIFEST-"):])
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parseNumberName(name, prefix, suffix string) (uint64, bool) {
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return 0, false
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	if len(digits) != 20 || !allDigits(digits) {
		return 0, false
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	return n, err == nil
}

func openManifest(fsys rootFS, opts Options) (manifest, error) {
	if err := ensureDirectory(fsys, "sst"); err != nil {
		return manifest{}, err
	}
	current, err := readRootFile(fsys, "CURRENT", 4096)
	if errors.Is(err, fs.ErrNotExist) {
		// A crash can leave a complete manifest after its durable rename but
		// before CURRENT became visible. Recover the highest complete manifest
		// and republish CURRENT; only a directory with no manifest is fresh.
		name, listErr := highestManifest(fsys)
		if listErr != nil {
			return manifest{}, listErr
		}
		if name != "" {
			m, readErr := readManifest(fsys, name, opts)
			if readErr != nil {
				return manifest{}, readErr
			}
			if err := validateSSTFiles(m.files, fsys, opts); err != nil {
				return manifest{}, err
			}
			currentData, currentErr := encodeCurrent(name)
			if currentErr != nil {
				return manifest{}, currentErr
			}
			if currentErr := writeAtomicRoot(fsys, ".CURRENT.tmp", "CURRENT", currentData); currentErr != nil {
				return manifest{}, currentErr
			}
			if cleanupErr := cleanupSSTDirectory(fsys, m.files); cleanupErr != nil {
				return manifest{}, cleanupErr
			}
			if cleanupErr := cleanupManifests(fsys, name); cleanupErr != nil {
				return manifest{}, cleanupErr
			}
			return m, nil
		}
		if cleanupErr := cleanupSSTDirectory(fsys, nil); cleanupErr != nil {
			return manifest{}, cleanupErr
		}
		return manifest{nextSST: 1}, nil
	}
	if err != nil {
		return manifest{}, fmt.Errorf("%w: read CURRENT: %v", ErrCorrupt, err)
	}
	manifestName, err := decodeCurrent(current)
	if err != nil {
		return manifest{}, err
	}
	m, err := readManifest(fsys, manifestName, opts)
	if err != nil {
		return manifest{}, err
	}
	if err := validateSSTFiles(m.files, fsys, opts); err != nil {
		return manifest{}, err
	}
	if err := cleanupSSTDirectory(fsys, m.files); err != nil {
		return manifest{}, err
	}
	if err := cleanupManifests(fsys, manifestName); err != nil {
		return manifest{}, err
	}
	return m, nil
}

func readManifest(fsys rootFS, name string, opts Options) (manifest, error) {
	if !validManifestName(name) {
		return manifest{}, fmt.Errorf("%w: invalid CURRENT manifest name", ErrCorrupt)
	}
	data, err := readRootFile(fsys, name, manifestMaxBytes)
	if err != nil {
		return manifest{}, fmt.Errorf("%w: read manifest: %v", ErrCorrupt, err)
	}
	if len(data) < manifestHeaderLen || string(data[:4]) != manifestMagic || data[4] != manifestVersion || binary.BigEndian.Uint16(data[6:8]) != 0 {
		return manifest{}, fmt.Errorf("%w: invalid manifest header", ErrCorrupt)
	}
	if binary.BigEndian.Uint32(data[40:44]) != crc32.Checksum(data[:40], walCRC) {
		return manifest{}, fmt.Errorf("%w: manifest header checksum", ErrCorrupt)
	}
	generation := binary.BigEndian.Uint64(data[8:16])
	manifestID, ok := parseNumberName(name, "MANIFEST-", "")
	if !ok || manifestID != generation {
		return manifest{}, fmt.Errorf("%w: manifest name/generation mismatch", ErrCorrupt)
	}
	flushedLSN := binary.BigEndian.Uint64(data[16:24])
	fileCount := binary.BigEndian.Uint32(data[24:28])
	bodyLen := int(binary.BigEndian.Uint32(data[28:32]))
	if binary.BigEndian.Uint32(data[36:40]) != 0 || fileCount > uint32(opts.MaxSSTFiles) || fileCount > maxManifestFiles || bodyLen < 0 || manifestHeaderLen+bodyLen != len(data) || binary.BigEndian.Uint32(data[32:36]) != crc32.Checksum(data[manifestHeaderLen:], walCRC) {
		return manifest{}, fmt.Errorf("%w: invalid manifest bounds/checksum", ErrCorrupt)
	}
	files := make([]sstFile, 0, fileCount)
	pos := manifestHeaderLen
	var maxID uint64
	seenL1 := false
	var lastL1Max []byte
	for i := uint32(0); i < fileCount; i++ {
		if len(data)-pos < 20 {
			return manifest{}, fmt.Errorf("%w: truncated manifest member", ErrCorrupt)
		}
		nameLen := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		level := data[pos+2]
		if data[pos+3] != 0 || nameLen < 1 || nameLen > maxSSTNameLen || len(data)-pos < 20+nameLen {
			return manifest{}, fmt.Errorf("%w: invalid manifest member bounds", ErrCorrupt)
		}
		memberName := string(data[pos+20 : pos+20+nameLen])
		if !validSSTName(memberName) || level > 1 {
			return manifest{}, fmt.Errorf("%w: invalid manifest member name/level", ErrCorrupt)
		}
		if level == 0 && seenL1 {
			return manifest{}, fmt.Errorf("%w: L0 member follows L1", ErrCorrupt)
		}
		entryCount := binary.BigEndian.Uint32(data[pos+4 : pos+8])
		fileBytes := int64(binary.BigEndian.Uint64(data[pos+8 : pos+16]))
		minLen := int(binary.BigEndian.Uint16(data[pos+16 : pos+18]))
		maxLen := int(binary.BigEndian.Uint16(data[pos+18 : pos+20]))
		if minLen < 1 || maxLen < 1 || minLen > opts.MaxKeyBytes || maxLen > opts.MaxKeyBytes {
			return manifest{}, fmt.Errorf("%w: invalid manifest key bounds", ErrCorrupt)
		}
		// The member name occupies the first variable field; key fields follow it.
		keyPos := pos + 20 + nameLen
		memberEnd := keyPos + minLen + maxLen
		if memberEnd > len(data) || entryCount == 0 || fileBytes < sstHeaderLen+sstFooterLen || fileBytes > int64(opts.MaxSSTBytes) {
			return manifest{}, fmt.Errorf("%w: invalid manifest member metadata", ErrCorrupt)
		}
		files = append(files, sstFile{name: memberName, level: level, entryCount: entryCount, bytes: fileBytes, minKey: append([]byte(nil), data[keyPos:keyPos+minLen]...), maxKey: append([]byte(nil), data[keyPos+minLen:memberEnd]...)})
		if level == 1 {
			if seenL1 && bytes.Compare(lastL1Max, data[keyPos:keyPos+minLen]) >= 0 {
				return manifest{}, fmt.Errorf("%w: overlapping L1 ranges", ErrCorrupt)
			}
			seenL1 = true
			lastL1Max = append(lastL1Max[:0], data[keyPos+minLen:memberEnd]...)
		}
		if id, ok := parseNumberName(memberName, "sst-", ".sst"); ok && id >= maxID {
			maxID = id + 1
		}
		pos = memberEnd
	}
	if pos != len(data) {
		return manifest{}, fmt.Errorf("%w: trailing manifest bytes", ErrCorrupt)
	}
	return manifest{generation: generation, flushedLSN: flushedLSN, files: files, nextSST: maxIDOrOne(maxID)}, nil
}

func maxIDOrOne(id uint64) uint64 {
	if id == 0 {
		return 1
	}
	return id
}

func highestManifest(fsys rootFS) (string, error) {
	entries, err := fsys.ReadDir(".")
	if err != nil {
		return "", fmt.Errorf("%w: list root: %v", ErrFilesystem, err)
	}
	var best string
	var bestID uint64
	for _, entry := range entries {
		name := entry.Name()
		id, ok := parseNumberName(name, "MANIFEST-", "")
		if !ok {
			continue
		}
		if best == "" || id > bestID {
			best, bestID = name, id
		}
	}
	return best, nil
}

func encodeManifest(m manifest) ([]byte, error) {
	var body bytes.Buffer
	for _, file := range m.files {
		if !validSSTName(file.name) || len(file.name) > maxSSTNameLen || file.level > 1 || len(file.minKey) < 1 || len(file.maxKey) < 1 || len(file.minKey) > int(^uint16(0)) || len(file.maxKey) > int(^uint16(0)) {
			return nil, fmt.Errorf("%w: invalid manifest file", ErrCorrupt)
		}
		var fixed [20]byte
		binary.BigEndian.PutUint16(fixed[0:2], uint16(len(file.name)))
		fixed[2] = file.level
		binary.BigEndian.PutUint32(fixed[4:8], file.entryCount)
		binary.BigEndian.PutUint64(fixed[8:16], uint64(file.bytes))
		binary.BigEndian.PutUint16(fixed[16:18], uint16(len(file.minKey)))
		binary.BigEndian.PutUint16(fixed[18:20], uint16(len(file.maxKey)))
		body.Write(fixed[:])
		body.WriteString(file.name)
		body.Write(file.minKey)
		body.Write(file.maxKey)
	}
	if body.Len() > manifestMaxBytes-manifestHeaderLen || len(m.files) > maxManifestFiles {
		return nil, fmt.Errorf("%w: manifest too large", ErrBatchTooLarge)
	}
	data := make([]byte, manifestHeaderLen+body.Len())
	copy(data[:4], manifestMagic)
	data[4] = manifestVersion
	binary.BigEndian.PutUint64(data[8:16], m.generation)
	binary.BigEndian.PutUint64(data[16:24], m.flushedLSN)
	binary.BigEndian.PutUint32(data[24:28], uint32(len(m.files)))
	binary.BigEndian.PutUint32(data[28:32], uint32(body.Len()))
	copy(data[manifestHeaderLen:], body.Bytes())
	binary.BigEndian.PutUint32(data[32:36], crc32.Checksum(data[manifestHeaderLen:], walCRC))
	binary.BigEndian.PutUint32(data[40:44], crc32.Checksum(data[:40], walCRC))
	return data, nil
}

func encodeCurrent(name string) ([]byte, error) {
	if !validManifestName(name) || len(name) > int(^uint16(0)) {
		return nil, fmt.Errorf("%w: invalid CURRENT target", ErrCorrupt)
	}
	data := make([]byte, currentHeaderLen+len(name)+4)
	copy(data[:4], currentMagic)
	data[4] = currentVersion
	binary.BigEndian.PutUint16(data[6:8], uint16(len(name)))
	copy(data[8:], name)
	binary.BigEndian.PutUint32(data[len(data)-4:], crc32.Checksum(data[:len(data)-4], walCRC))
	return data, nil
}

func decodeCurrent(data []byte) (string, error) {
	if len(data) < currentHeaderLen+4 || string(data[:4]) != currentMagic || data[4] != currentVersion || data[5] != 0 || binary.BigEndian.Uint32(data[len(data)-4:]) != crc32.Checksum(data[:len(data)-4], walCRC) {
		return "", fmt.Errorf("%w: invalid CURRENT", ErrCorrupt)
	}
	nameLen := int(binary.BigEndian.Uint16(data[6:8]))
	if nameLen < 1 || currentHeaderLen+nameLen+4 != len(data) {
		return "", fmt.Errorf("%w: invalid CURRENT length", ErrCorrupt)
	}
	name := string(data[8 : 8+nameLen])
	if !validManifestName(name) {
		return "", fmt.Errorf("%w: invalid CURRENT target", ErrCorrupt)
	}
	return name, nil
}

func readRootFile(fsys rootFS, name string, max int64) ([]byte, error) {
	f, err := fsys.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < 0 || info.Size() > max {
		return nil, fmt.Errorf("%w: file exceeds bound", ErrCorrupt)
	}
	data := make([]byte, info.Size())
	if err := readFullAt(f, data, 0); err != nil {
		return nil, err
	}
	return data, nil
}

func publishManifest(fsys rootFS, m manifest) error {
	data, err := encodeManifest(m)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("MANIFEST-%020d", m.generation)
	if err := writeAtomicRoot(fsys, "."+name+".tmp", name, data); err != nil {
		return err
	}
	current, err := encodeCurrent(name)
	if err != nil {
		return err
	}
	if err := writeAtomicRoot(fsys, ".CURRENT.tmp", "CURRENT", current); err != nil {
		return err
	}
	return nil
}

func writeAtomicRoot(fsys rootFS, temp, final string, data []byte) error {
	_ = fsys.Remove(temp)
	f, err := fsys.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("%w: create atomic file: %v", ErrFilesystem, err)
	}
	if err := writeFullAt(f, data, 0); err != nil {
		_ = f.Close()
		_ = fsys.Remove(temp)
		return fmt.Errorf("%w: write atomic file: %v", ErrFilesystem, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = fsys.Remove(temp)
		return fmt.Errorf("%w: sync atomic file: %v", ErrFilesystem, err)
	}
	if err := f.Close(); err != nil {
		_ = fsys.Remove(temp)
		return fmt.Errorf("%w: close atomic file: %v", ErrFilesystem, err)
	}
	if err := fsys.Rename(temp, final); err != nil {
		_ = fsys.Remove(temp)
		return fmt.Errorf("%w: rename atomic file: %v", ErrFilesystem, err)
	}
	return syncDirectory(fsys, ".")
}

func cleanupSSTDirectory(fsys rootFS, files []sstFile) error {
	keep := make(map[string]struct{}, len(files))
	for _, file := range files {
		keep[file.name] = struct{}{}
	}
	entries, err := fsys.ReadDir("sst")
	if err != nil {
		return fmt.Errorf("%w: list SST directory: %v", ErrFilesystem, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".tmp") || (validSSTName(name) && func() bool { _, ok := keep[name]; return !ok }()) {
			if err := fsys.Remove("sst/" + name); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%w: remove orphan SST: %v", ErrFilesystem, err)
			}
			continue
		}
		if !validSSTName(name) {
			return fmt.Errorf("%w: unexpected SST member %q", ErrCorrupt, name)
		}
	}
	return syncDirectory(fsys, "sst")
}

func cleanupManifests(fsys rootFS, current string) error {
	entries, err := fsys.ReadDir(".")
	if err != nil {
		return fmt.Errorf("%w: list root: %v", ErrFilesystem, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if validManifestName(name) && name != current {
			if err := fsys.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%w: remove obsolete manifest: %v", ErrFilesystem, err)
			}
		}
	}
	return syncDirectory(fsys, ".")
}
