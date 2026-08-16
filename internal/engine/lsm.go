package engine

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"sort"
)

const l0CompactionTrigger = 4

// flushActive turns the bounded mutable memtable into one immutable L0 SST.
// The WAL and manifest are advanced only after the SST and successor WAL have
// been synchronized. Old files remain until the successor manifest is
// durable and all snapshot/reader pins have been released.
func (e *Engine) flushActive() error {
	if len(e.active) == 0 {
		return nil
	}
	entries := entriesFromActive(e.active)
	sort.Slice(entries, func(i, j int) bool { return bytes.Compare(entries[i].key, entries[j].key) < 0 })
	old := e.manifest
	id := old.nextSST
	if id == 0 {
		id = 1
	}
	newFile, err := writeSST(e.fs, entries, 0, id, e.lsn, e.opts)
	if err != nil {
		return err
	}
	if err := e.wal.rotate(e.lsn + 1); err != nil {
		return fmt.Errorf("%w: rotate WAL for checkpoint: %v", ErrFilesystem, err)
	}
	files := make([]sstFile, 0, len(old.files)+1)
	for _, file := range old.files {
		if file.level == 0 {
			files = append(files, copySSTFile(file))
		}
	}
	files = append(files, newFile)
	for _, file := range old.files {
		if file.level == 1 {
			files = append(files, copySSTFile(file))
		}
	}
	if len(files) > e.opts.MaxSSTFiles {
		return fmt.Errorf("%w: flush would exceed manifest SST limit", ErrBatchTooLarge)
	}
	updated := manifest{generation: old.generation + 1, flushedLSN: e.lsn, files: files, nextSST: id + 1}
	if err := publishManifest(e.fs, updated); err != nil {
		return err
	}
	e.manifest = updated
	e.active = make(map[string]memEntry)
	e.activeBytes = 0
	if err := e.wal.removeBefore(e.wal.seg); err != nil {
		return err
	}
	if countLevel(files, 0) >= l0CompactionTrigger || len(files) >= e.opts.MaxSSTFiles {
		return e.compactLevels()
	}
	return nil
}

func (e *Engine) checkpoint(full bool) error {
	if !full && len(e.active) == 0 {
		return nil
	}
	if len(e.active) == 0 {
		return nil
	}
	return e.flushActive()
}

// compactLevels merges all live L0 records and the non-overlapping L1 base,
// choosing the newest record for each key. Output is partitioned into
// size-bounded L1 files and is generated with one source block plus one output
// chunk in memory. Tombstones are safe to discard because every older source
// is included in this compaction; snapshots retain the old files.
func (e *Engine) compactLevels() error {
	if countLevel(e.manifest.files, 0) == 0 {
		return nil
	}
	view := &engineView{owner: e, fs: e.fs, opt: e.opts, files: make([]sstFile, len(e.manifest.files))}
	for i, file := range e.manifest.files {
		view.files[i] = copySSTFile(file)
	}
	sources, err := buildSources(view, nil)
	if err != nil {
		return err
	}
	defer func() {
		for i := range sources {
			sources[i].close()
		}
	}()

	chunk := make([]sstEntry, 0)
	chunkBytes := 0
	nextID := e.manifest.nextSST
	outputs := make([]sstFile, 0)
	var flushEntries func([]sstEntry) error
	flushEntries = func(entries []sstEntry) error {
		if len(entries) == 0 {
			return nil
		}
		// Preflight the exact encoded size. The approximate chunk target below
		// is intentionally conservative, but this split is the final guard for
		// key/index/record CRC overhead and unusual key distributions.
		if _, _, err := encodeSST(entries, 1, nextID, e.lsn, e.opts); err != nil {
			if len(entries) == 1 {
				return err
			}
			mid := len(entries) / 2
			if err := flushEntries(entries[:mid]); err != nil {
				return err
			}
			return flushEntries(entries[mid:])
		}
		file, err := writeSST(e.fs, entries, 1, nextID, e.lsn, e.opts)
		if err != nil {
			return err
		}
		outputs = append(outputs, file)
		nextID++
		return nil
	}
	flushChunk := func() error {
		entries := chunk
		chunk = make([]sstEntry, 0)
		chunkBytes = 0
		return flushEntries(entries)
	}
	// Leave room for the SST header/index/footer. A single record may exceed
	// this target, in which case writeSST accepts one bounded oversized block.
	target := e.opts.MaxSSTBytes - e.opts.SSTBlockBytes - sstHeaderLen - sstFooterLen - 64
	if target < 1 {
		target = e.opts.SSTBlockBytes
	}
	for {
		entry, ok, err := nextMergedRaw(sources)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		if entry.kind == deleteOp {
			continue
		}
		entryBytes := entry.size()
		if len(chunk) > 0 && chunkBytes+entryBytes > target {
			if err := flushChunk(); err != nil {
				return err
			}
		}
		chunk = append(chunk, entry)
		chunkBytes += entryBytes
	}
	if err := flushChunk(); err != nil {
		return err
	}
	capacity := e.opts.MaxSSTFiles - l0CompactionTrigger
	if len(outputs) > capacity {
		return fmt.Errorf("%w: compaction produced too many SST files", ErrBatchTooLarge)
	}

	old := e.manifest
	updated := manifest{generation: old.generation + 1, flushedLSN: e.lsn, files: outputs, nextSST: nextID}
	if err := publishManifest(e.fs, updated); err != nil {
		return err
	}
	e.manifest = updated
	for _, file := range old.files {
		e.obsolete = append(e.obsolete, copySSTFile(file))
	}
	if err := e.cleanupObsoleteLocked(); err != nil {
		return err
	}
	if err := cleanupManifests(e.fs, fmt.Sprintf("MANIFEST-%020d", updated.generation)); err != nil {
		return err
	}
	return nil
}

func (e *Engine) cleanupObsoleteLocked() error {
	if e.pins != 0 || len(e.obsolete) == 0 {
		return nil
	}
	current := make(map[string]struct{}, len(e.manifest.files))
	for _, file := range e.manifest.files {
		current[file.name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(e.obsolete))
	for _, file := range e.obsolete {
		if _, ok := current[file.name]; ok {
			continue
		}
		if _, ok := seen[file.name]; ok {
			continue
		}
		seen[file.name] = struct{}{}
		if err := e.fs.Remove("sst/" + file.name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: remove obsolete SST: %v", ErrFilesystem, err)
		}
	}
	e.obsolete = nil
	return syncDirectory(e.fs, "sst")
}

func countLevel(files []sstFile, level byte) int {
	n := 0
	for _, file := range files {
		if file.level == level {
			n++
		}
	}
	return n
}
