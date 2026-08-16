package engine

import (
	"bytes"
	"sort"
)

// engineView is the immutable input set for one read. The active memtable is
// copied at view creation (and is bounded by MemtableBytes); immutable table
// metadata is copied without loading table contents. SST readers load at most
// one checksummed block at a time.
type engineView struct {
	owner  *Engine
	fs     rootFS
	opt    Options
	active []sstEntry
	files  []sstFile
}

func (e *Engine) newViewLocked() *engineView {
	active := entriesFromActive(e.active)
	sort.Slice(active, func(i, j int) bool { return bytes.Compare(active[i].key, active[j].key) < 0 })
	files := make([]sstFile, len(e.manifest.files))
	for i, file := range e.manifest.files {
		files[i] = copySSTFile(file)
	}
	return &engineView{owner: e, fs: e.fs, opt: e.opts, active: active, files: files}
}

func copySSTFile(file sstFile) sstFile {
	file.minKey = append([]byte(nil), file.minKey...)
	file.maxKey = append([]byte(nil), file.maxKey...)
	return file
}

func (e *Engine) releaseView(v *engineView) {
	if e == nil || v == nil {
		return
	}
	e.mu.Lock()
	if e.pins > 0 {
		e.pins--
	}
	if e.pins == 0 {
		_ = e.cleanupObsoleteLocked()
	}
	e.mu.Unlock()
}

func (e *Engine) pinViewLocked(v *engineView) {
	e.pins++
	v.owner = e
}

type sourceCursor struct {
	active []sstEntry
	reader *sstReader
	index  int
	cur    sstEntry
	have   bool
	prio   int
}

func (c *sourceCursor) init(start []byte) error {
	if c.reader != nil {
		if err := c.reader.seek(start); err != nil {
			return err
		}
	}
	if c.active != nil {
		c.index = sort.Search(len(c.active), func(i int) bool {
			return start == nil || bytes.Compare(c.active[i].key, start) >= 0
		})
	}
	return c.advance()
}

func (c *sourceCursor) advance() error {
	if c.reader != nil {
		entry, ok, err := c.reader.next()
		if err != nil {
			return err
		}
		if !ok {
			c.have = false
			return nil
		}
		c.cur = entry
		c.have = true
		return nil
	}
	if c.index >= len(c.active) {
		c.have = false
		return nil
	}
	c.cur = copyEntry(c.active[c.index])
	c.index++
	c.have = true
	return nil
}

func (c *sourceCursor) close() {
	if c.reader != nil {
		_ = c.reader.close()
		c.reader = nil
	}
}

func copyEntry(entry sstEntry) sstEntry {
	return sstEntry{kind: entry.kind, key: append([]byte(nil), entry.key...), value: append([]byte(nil), entry.value...)}
}

func buildSources(v *engineView, start []byte) ([]sourceCursor, error) {
	sources := make([]sourceCursor, 0, len(v.files)+1)
	if len(v.active) > 0 {
		sources = append(sources, sourceCursor{active: v.active, prio: 0})
	}
	priority := len(sources)
	// Manifest L0 members are oldest-to-newest. Newest wins, so expose them
	// in reverse order. L1 ranges are non-overlapping and are older than all
	// L0 records.
	for i := len(v.files) - 1; i >= 0; i-- {
		file := v.files[i]
		if file.level != 0 {
			continue
		}
		reader, err := openSSTReader(v.fs, file, v.opt, false)
		if err != nil {
			for j := range sources {
				sources[j].close()
			}
			return nil, err
		}
		sources = append(sources, sourceCursor{reader: reader, prio: priority})
		priority++
	}
	for _, file := range v.files {
		if file.level != 1 {
			continue
		}
		reader, err := openSSTReader(v.fs, file, v.opt, false)
		if err != nil {
			for j := range sources {
				sources[j].close()
			}
			return nil, err
		}
		sources = append(sources, sourceCursor{reader: reader, prio: priority})
		priority++
	}
	for i := range sources {
		if err := sources[i].init(start); err != nil {
			for j := range sources {
				sources[j].close()
			}
			return nil, err
		}
	}
	return sources, nil
}

func lookupView(v *engineView, key []byte) ([]byte, error) {
	if err := validateKey(key, v.opt.MaxKeyBytes); err != nil {
		return nil, err
	}
	for _, entry := range v.active {
		if bytes.Equal(entry.key, key) {
			if entry.kind == deleteOp {
				return nil, ErrNotFound
			}
			return append([]byte(nil), entry.value...), nil
		}
	}
	for i := len(v.files) - 1; i >= 0; i-- {
		file := v.files[i]
		if file.level != 0 || bytes.Compare(key, file.minKey) < 0 || bytes.Compare(key, file.maxKey) > 0 {
			continue
		}
		entry, found, err := lookupSST(v.fs, file, key, v.opt)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if entry.kind == deleteOp {
			return nil, ErrNotFound
		}
		return append([]byte(nil), entry.value...), nil
	}
	for _, file := range v.files {
		if file.level != 1 || bytes.Compare(key, file.minKey) < 0 || bytes.Compare(key, file.maxKey) > 0 {
			continue
		}
		entry, found, err := lookupSST(v.fs, file, key, v.opt)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, ErrNotFound
		}
		if entry.kind == deleteOp {
			return nil, ErrNotFound
		}
		return append([]byte(nil), entry.value...), nil
	}
	return nil, ErrNotFound
}

func validateRange(opts Options, start, end []byte) error {
	if start != nil {
		if err := validateKey(start, opts.MaxKeyBytes); err != nil {
			return err
		}
	}
	if end != nil {
		if err := validateKey(end, opts.MaxKeyBytes); err != nil {
			return err
		}
	}
	return nil
}

func nextMergedRaw(sources []sourceCursor) (sstEntry, bool, error) {
	var key []byte
	for i := range sources {
		if !sources[i].have {
			continue
		}
		if key == nil || bytes.Compare(sources[i].cur.key, key) < 0 {
			key = sources[i].cur.key
		}
	}
	if key == nil {
		return sstEntry{}, false, nil
	}
	winner := -1
	for i := range sources {
		if !sources[i].have || !bytes.Equal(sources[i].cur.key, key) {
			continue
		}
		if winner < 0 || sources[i].prio < sources[winner].prio {
			winner = i
		}
	}
	chosen := copyEntry(sources[winner].cur)
	for i := range sources {
		if sources[i].have && bytes.Equal(sources[i].cur.key, key) {
			if err := sources[i].advance(); err != nil {
				return sstEntry{}, false, err
			}
		}
	}
	return chosen, true, nil
}
