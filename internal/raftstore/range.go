package raftstore

// Range descriptors are the immutable routing vocabulary shared by the M4
// catalog, read fence, and split protocol.  This file deliberately has no
// Replica or filesystem dependency: malformed catalog bytes must be rejected
// before they can become routing authority.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
	"sync"
	"unicode/utf8"
)

const (
	rangeMagic          = "VBRG"
	rangeFormat         = byte(1)
	maxRangeDescriptors = 4096
	maxRangeIDBytes     = 128
	maxRangeKeyBytes    = 1024
	maxRangeCatalogSize = 8 << 20
	rangeInfinity       = ^uint32(0)
)

var rangeCRC = crc32.MakeTable(crc32.Castagnoli)

var (
	ErrRangeInvalid    = errors.New("raftstore: invalid range descriptor")
	ErrRangeOverlap    = errors.New("raftstore: overlapping range descriptors")
	ErrRangeGap        = errors.New("raftstore: range descriptor gap")
	ErrCatalogStale    = errors.New("raftstore: stale range catalog")
	ErrRangeMoved      = errors.New("raftstore: range moved")
	ErrRangeNotServing = errors.New("raftstore: range is not serving")
	ErrCatalogCorrupt  = errors.New("raftstore: corrupt range catalog")
)

// ServingPhase is intentionally small.  Copying and catch-up descriptors are
// never routable; only the complete cutover publishes Serving descriptors.
type ServingPhase uint8

const (
	RangeCopying ServingPhase = iota + 1
	RangeCatchingUp
	RangeServing
	RangeRetired
)

func (p ServingPhase) String() string {
	switch p {
	case RangeCopying:
		return "copying"
	case RangeCatchingUp:
		return "catching-up"
	case RangeServing:
		return "serving"
	case RangeRetired:
		return "retired"
	default:
		return "unknown"
	}
}

func validPhaseTransition(previous, next ServingPhase) bool {
	if previous == next {
		return true
	}
	switch previous {
	case RangeCopying:
		return next == RangeCatchingUp
	case RangeCatchingUp:
		return next == RangeServing
	case RangeServing:
		return next == RangeRetired
	default:
		return false
	}
}

// RangeDescriptor is a half-open [Start, End) span.  A nil End means positive
// infinity. RangeID never changes; Generation fences catalog changes,
// OwnerEpoch fences ownership/configuration changes, and GroupID identifies
// the Raft group independently of both.
type RangeDescriptor struct {
	RangeID    string
	Start      []byte
	End        []byte
	Generation uint64
	OwnerEpoch uint64
	GroupID    uint64
	Voters     []uint64
	Phase      ServingPhase
}

func (d RangeDescriptor) Clone() RangeDescriptor {
	d.Start = append([]byte(nil), d.Start...)
	d.End = append([]byte(nil), d.End...)
	d.Voters = append([]uint64(nil), d.Voters...)
	return d
}

func (d RangeDescriptor) Validate() error {
	if d.RangeID == "" || len(d.RangeID) > maxRangeIDBytes || !utf8.ValidString(d.RangeID) {
		return fmt.Errorf("%w: range id", ErrRangeInvalid)
	}
	if len(d.Start) > maxRangeKeyBytes || len(d.End) > maxRangeKeyBytes {
		return fmt.Errorf("%w: key bound", ErrRangeInvalid)
	}
	if d.End != nil && bytes.Compare(d.Start, d.End) >= 0 {
		return fmt.Errorf("%w: empty or reversed span", ErrRangeInvalid)
	}
	if d.Generation == 0 || d.OwnerEpoch == 0 || d.GroupID == 0 {
		return fmt.Errorf("%w: zero fence", ErrRangeInvalid)
	}
	if len(d.Voters) != 3 {
		return fmt.Errorf("%w: RF3 requires three voters", ErrRangeInvalid)
	}
	for i, voter := range d.Voters {
		if voter == 0 || (i > 0 && voter <= d.Voters[i-1]) {
			return fmt.Errorf("%w: voters must be sorted and distinct", ErrRangeInvalid)
		}
	}
	switch d.Phase {
	case RangeCopying, RangeCatchingUp, RangeServing, RangeRetired:
	default:
		return fmt.Errorf("%w: phase", ErrRangeInvalid)
	}
	return nil
}

func (d RangeDescriptor) Contains(key []byte) bool {
	return bytes.Compare(key, d.Start) >= 0 && (d.End == nil || bytes.Compare(key, d.End) < 0)
}

func equalUint64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func EqualRangeDescriptor(a, b RangeDescriptor) bool {
	return a.RangeID == b.RangeID && bytes.Equal(a.Start, b.Start) && bytes.Equal(a.End, b.End) &&
		a.Generation == b.Generation && a.OwnerEpoch == b.OwnerEpoch && a.GroupID == b.GroupID &&
		equalUint64(a.Voters, b.Voters) && a.Phase == b.Phase
}

// RangeMovedError carries an owned copy of the current route. A caller may
// refresh its cache and retry the same operation identity without guessing.
type RangeMovedError struct {
	RequestedRangeID    string
	RequestedGeneration uint64
	RequestedOwnerEpoch uint64
	RequestedGroupID    uint64
	Newest              []RangeDescriptor
}

func (e *RangeMovedError) Error() string {
	return fmt.Sprintf("%v: range=%s generation=%d epoch=%d group=%d", ErrRangeMoved, e.RequestedRangeID, e.RequestedGeneration, e.RequestedOwnerEpoch, e.RequestedGroupID)
}

func (e *RangeMovedError) Unwrap() error { return ErrRangeMoved }

func (e *RangeMovedError) Clone() *RangeMovedError {
	if e == nil {
		return nil
	}
	out := &RangeMovedError{RequestedRangeID: e.RequestedRangeID, RequestedGeneration: e.RequestedGeneration, RequestedOwnerEpoch: e.RequestedOwnerEpoch, RequestedGroupID: e.RequestedGroupID, Newest: make([]RangeDescriptor, len(e.Newest))}
	for i := range e.Newest {
		out.Newest[i] = e.Newest[i].Clone()
	}
	return out
}

// RangeCatalog is an atomically replaced routing image. history contains the
// latest fence for every range ID, including retired source tombstones.
type RangeCatalog struct {
	mu          sync.RWMutex
	version     uint64
	descriptors []RangeDescriptor
	byID        map[string]RangeDescriptor
	history     map[string]RangeDescriptor
}

func NewRangeCatalog(version uint64, descriptors []RangeDescriptor) (*RangeCatalog, error) {
	c := &RangeCatalog{history: make(map[string]RangeDescriptor)}
	if err := c.replaceLocked(version, descriptors, false); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *RangeCatalog) Version() uint64 {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.version
}

func (c *RangeCatalog) Descriptors() []RangeDescriptor {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneDescriptors(c.descriptors)
}

func cloneDescriptors(in []RangeDescriptor) []RangeDescriptor {
	out := make([]RangeDescriptor, len(in))
	for i := range in {
		out[i] = in[i].Clone()
	}
	return out
}

func (c *RangeCatalog) Replace(version uint64, descriptors []RangeDescriptor) error {
	if c == nil {
		return ErrCatalogCorrupt
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.replaceLocked(version, descriptors, true)
}

func (c *RangeCatalog) replaceLocked(version uint64, descriptors []RangeDescriptor, requireAdvance bool) error {
	if version == 0 || (requireAdvance && version <= c.version) {
		return fmt.Errorf("%w: version %d", ErrCatalogStale, version)
	}
	if len(descriptors) == 0 || len(descriptors) > maxRangeDescriptors {
		return fmt.Errorf("%w: descriptor count", ErrRangeInvalid)
	}
	next := cloneDescriptors(descriptors)
	for i := range next {
		if err := next[i].Validate(); err != nil {
			return err
		}
	}
	sort.Slice(next, func(i, j int) bool { return bytes.Compare(next[i].Start, next[j].Start) < 0 })
	byID := make(map[string]RangeDescriptor, len(next))
	for i, descriptor := range next {
		if _, exists := byID[descriptor.RangeID]; exists {
			return fmt.Errorf("%w: duplicate range id", ErrRangeInvalid)
		}
		if i == 0 && len(descriptor.Start) != 0 {
			return ErrRangeGap
		}
		if i > 0 {
			previous := next[i-1]
			if previous.End == nil || !bytes.Equal(previous.End, descriptor.Start) {
				if previous.End == nil || bytes.Compare(previous.End, descriptor.Start) > 0 {
					return ErrRangeOverlap
				}
				return ErrRangeGap
			}
		}
		if i == len(next)-1 && descriptor.End != nil {
			return ErrRangeGap
		}
		byID[descriptor.RangeID] = descriptor
	}
	if c.history == nil {
		c.history = make(map[string]RangeDescriptor)
	}
	if c.version != 0 {
		for id, old := range c.history {
			current, present := byID[id]
			if !present {
				if old.Phase != RangeRetired {
					old.Phase = RangeRetired
					c.history[id] = old
				}
				continue
			}
			if _, wasCurrent := c.byID[id]; !wasCurrent {
				return fmt.Errorf("%w: tombstone %s resurrected", ErrCatalogStale, id)
			}
			previous := c.byID[id]
			if !bytes.Equal(previous.Start, current.Start) || !bytes.Equal(previous.End, current.End) {
				return fmt.Errorf("%w: immutable span %s changed", ErrCatalogStale, id)
			}
			if current.Generation < old.Generation || current.OwnerEpoch < old.OwnerEpoch {
				return fmt.Errorf("%w: fence %s regressed", ErrCatalogStale, id)
			}
			if previous.Phase == RangeRetired && current.Phase != RangeRetired {
				return fmt.Errorf("%w: retired %s resurrected", ErrCatalogStale, id)
			}
			if !validPhaseTransition(previous.Phase, current.Phase) {
				return fmt.Errorf("%w: phase %s -> %s", ErrCatalogStale, previous.Phase, current.Phase)
			}
			if (previous.GroupID != current.GroupID || !equalUint64(previous.Voters, current.Voters)) && current.OwnerEpoch <= old.OwnerEpoch {
				return fmt.Errorf("%w: owner epoch did not advance for %s", ErrCatalogStale, id)
			}
		}
	}
	if len(c.history)+len(byID) > maxRangeDescriptors*2 {
		return ErrBackpressure
	}
	c.version = version
	c.descriptors = next
	c.byID = byID
	for id, descriptor := range byID {
		c.history[id] = descriptor.Clone()
	}
	return nil
}

func (c *RangeCatalog) descriptorForKey(key []byte) (RangeDescriptor, bool) {
	for _, descriptor := range c.descriptors {
		if descriptor.Contains(key) {
			return descriptor, true
		}
	}
	return RangeDescriptor{}, false
}

func (c *RangeCatalog) Route(key []byte) (RangeDescriptor, error) {
	if c == nil {
		return RangeDescriptor{}, ErrCatalogCorrupt
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	descriptor, found := c.descriptorForKey(key)
	if !found {
		return RangeDescriptor{}, ErrRangeGap
	}
	if descriptor.Phase != RangeServing {
		return RangeDescriptor{}, ErrRangeNotServing
	}
	return descriptor.Clone(), nil
}

// RouteAt requires every route-fence coordinate, including GroupID. It is
// intentionally non-variadic so a call site cannot omit group identity.
func (c *RangeCatalog) RouteAt(key []byte, rangeID string, generation, ownerEpoch, groupID uint64) (RangeDescriptor, error) {
	if c == nil {
		return RangeDescriptor{}, ErrCatalogCorrupt
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	current, found := c.descriptorForKey(key)
	if !found {
		return RangeDescriptor{}, ErrRangeGap
	}
	if current.RangeID != rangeID || current.Generation != generation || current.OwnerEpoch != ownerEpoch || current.GroupID != groupID || current.Phase != RangeServing {
		return RangeDescriptor{}, &RangeMovedError{RequestedRangeID: rangeID, RequestedGeneration: generation, RequestedOwnerEpoch: ownerEpoch, RequestedGroupID: groupID, Newest: []RangeDescriptor{current.Clone()}}
	}
	return current.Clone(), nil
}

func (c *RangeCatalog) VerifyGeneration(key []byte, descriptor RangeDescriptor) error {
	_, err := c.RouteAt(key, descriptor.RangeID, descriptor.Generation, descriptor.OwnerEpoch, descriptor.GroupID)
	return err
}

func (c *RangeCatalog) DescriptorByID(id string) (RangeDescriptor, bool) {
	if c == nil {
		return RangeDescriptor{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	descriptor, ok := c.byID[id]
	if !ok {
		descriptor, ok = c.history[id]
	}
	if ok {
		descriptor = descriptor.Clone()
	}
	return descriptor, ok
}

func (c *RangeCatalog) historyDescriptors() []RangeDescriptor {
	ids := make([]string, 0, len(c.history))
	for id := range c.history {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]RangeDescriptor, 0, len(ids))
	for _, id := range ids {
		out = append(out, c.history[id].Clone())
	}
	return out
}

// MarshalBinary emits one canonical, bounded, CRC32C-protected image. The
// history list is sorted by immutable ID and contains every current fence.
func (c *RangeCatalog) MarshalBinary() ([]byte, error) {
	if c == nil {
		return nil, ErrCatalogCorrupt
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.version == 0 || len(c.descriptors) == 0 || len(c.history) > maxRangeDescriptors*2 {
		return nil, ErrCatalogCorrupt
	}
	var body bytes.Buffer
	body.WriteString(rangeMagic)
	body.WriteByte(rangeFormat)
	var n8 [8]byte
	binary.BigEndian.PutUint64(n8[:], c.version)
	body.Write(n8[:])
	if err := writeCount(&body, len(c.descriptors)); err != nil {
		return nil, err
	}
	for _, descriptor := range c.descriptors {
		if err := writeDescriptor(&body, descriptor); err != nil {
			return nil, err
		}
	}
	history := c.historyDescriptors()
	if err := writeCount(&body, len(history)); err != nil {
		return nil, err
	}
	for _, descriptor := range history {
		if err := writeDescriptor(&body, descriptor); err != nil {
			return nil, err
		}
	}
	if body.Len()+4 > maxRangeCatalogSize {
		return nil, ErrBackpressure
	}
	checksum := crc32.Checksum(body.Bytes(), rangeCRC)
	binary.BigEndian.PutUint32(n8[:4], checksum)
	body.Write(n8[:4])
	return body.Bytes(), nil
}

func writeCount(out *bytes.Buffer, count int) error {
	if count < 0 || count > maxRangeDescriptors*2 {
		return ErrBackpressure
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(count))
	out.Write(n[:])
	return nil
}

func writeBytes(out *bytes.Buffer, value []byte, max int) error {
	if len(value) > max {
		return ErrRangeInvalid
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(value)))
	out.Write(n[:])
	out.Write(value)
	return nil
}

func writeDescriptor(out *bytes.Buffer, d RangeDescriptor) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if err := writeBytes(out, []byte(d.RangeID), maxRangeIDBytes); err != nil {
		return err
	}
	if err := writeBytes(out, d.Start, maxRangeKeyBytes); err != nil {
		return err
	}
	var end [4]byte
	if d.End == nil {
		binary.BigEndian.PutUint32(end[:], rangeInfinity)
		out.Write(end[:])
	} else if err := writeBytes(out, d.End, maxRangeKeyBytes); err != nil {
		return err
	}
	var n [8]byte
	for _, value := range []uint64{d.Generation, d.OwnerEpoch, d.GroupID} {
		binary.BigEndian.PutUint64(n[:], value)
		out.Write(n[:])
	}
	out.WriteByte(byte(d.Phase))
	if err := writeCount(out, len(d.Voters)); err != nil {
		return err
	}
	for _, voter := range d.Voters {
		binary.BigEndian.PutUint64(n[:], voter)
		out.Write(n[:])
	}
	return nil
}

func UnmarshalRangeCatalog(encoded []byte) (*RangeCatalog, error) {
	if len(encoded) < 5+8+4+4 || len(encoded) > maxRangeCatalogSize {
		return nil, ErrCatalogCorrupt
	}
	if !bytes.Equal(encoded[:4], []byte(rangeMagic)) || encoded[4] != rangeFormat {
		return nil, ErrCatalogCorrupt
	}
	if crc32.Checksum(encoded[:len(encoded)-4], rangeCRC) != binary.BigEndian.Uint32(encoded[len(encoded)-4:]) {
		return nil, ErrCatalogCorrupt
	}
	position := 5
	version, ok := readU64(encoded, &position)
	if !ok {
		return nil, ErrCatalogCorrupt
	}
	count, ok := readCount(encoded, &position)
	if !ok || count == 0 || count > maxRangeDescriptors {
		return nil, ErrCatalogCorrupt
	}
	descriptors := make([]RangeDescriptor, count)
	for i := range descriptors {
		var err error
		descriptors[i], err = readDescriptor(encoded, &position)
		if err != nil {
			return nil, ErrCatalogCorrupt
		}
	}
	historyCount, ok := readCount(encoded, &position)
	if !ok || historyCount > maxRangeDescriptors*2 {
		return nil, ErrCatalogCorrupt
	}
	history := make([]RangeDescriptor, historyCount)
	for i := range history {
		var err error
		history[i], err = readDescriptor(encoded, &position)
		if err != nil {
			return nil, ErrCatalogCorrupt
		}
	}
	if position != len(encoded)-4 {
		return nil, ErrCatalogCorrupt
	}
	catalog, err := NewRangeCatalog(version, descriptors)
	if err != nil {
		return nil, ErrCatalogCorrupt
	}
	seen := make(map[string]struct{}, len(history))
	for _, descriptor := range history {
		if _, exists := seen[descriptor.RangeID]; exists {
			return nil, ErrCatalogCorrupt
		}
		seen[descriptor.RangeID] = struct{}{}
		current, isCurrent := catalog.byID[descriptor.RangeID]
		if isCurrent {
			if !EqualRangeDescriptor(current, descriptor) {
				return nil, ErrCatalogCorrupt
			}
			continue
		}
		if descriptor.Phase != RangeRetired {
			return nil, ErrCatalogCorrupt
		}
		catalog.history[descriptor.RangeID] = descriptor.Clone()
	}
	canonical, err := catalog.MarshalBinary()
	if err != nil || !bytes.Equal(canonical, encoded) {
		return nil, ErrCatalogCorrupt
	}
	return catalog, nil
}

func readU64(encoded []byte, position *int) (uint64, bool) {
	if *position < 0 || *position+8 > len(encoded) {
		return 0, false
	}
	value := binary.BigEndian.Uint64(encoded[*position : *position+8])
	*position += 8
	return value, true
}

func readCount(encoded []byte, position *int) (int, bool) {
	if *position < 0 || *position+4 > len(encoded) {
		return 0, false
	}
	value := binary.BigEndian.Uint32(encoded[*position : *position+4])
	*position += 4
	if value > maxRangeDescriptors*2 {
		return 0, false
	}
	return int(value), true
}

func readBytes(encoded []byte, position *int, max int) ([]byte, bool) {
	if *position < 0 || *position+4 > len(encoded) {
		return nil, false
	}
	length := binary.BigEndian.Uint32(encoded[*position : *position+4])
	*position += 4
	if length > uint32(max) || int(length) > len(encoded)-*position {
		return nil, false
	}
	value := append([]byte(nil), encoded[*position:*position+int(length)]...)
	*position += int(length)
	return value, true
}

func readDescriptor(encoded []byte, position *int) (RangeDescriptor, error) {
	id, ok := readBytes(encoded, position, maxRangeIDBytes)
	if !ok {
		return RangeDescriptor{}, ErrCatalogCorrupt
	}
	start, ok := readBytes(encoded, position, maxRangeKeyBytes)
	if !ok || *position+4 > len(encoded) {
		return RangeDescriptor{}, ErrCatalogCorrupt
	}
	endLength := binary.BigEndian.Uint32(encoded[*position : *position+4])
	*position += 4
	var end []byte
	if endLength != rangeInfinity {
		if endLength > maxRangeKeyBytes || int(endLength) > len(encoded)-*position {
			return RangeDescriptor{}, ErrCatalogCorrupt
		}
		end = append([]byte(nil), encoded[*position:*position+int(endLength)]...)
		*position += int(endLength)
	}
	values := make([]uint64, 3)
	for i := range values {
		value, ok := readU64(encoded, position)
		if !ok {
			return RangeDescriptor{}, ErrCatalogCorrupt
		}
		values[i] = value
	}
	if *position >= len(encoded) {
		return RangeDescriptor{}, ErrCatalogCorrupt
	}
	phase := ServingPhase(encoded[*position])
	*position++
	voterCount, ok := readCount(encoded, position)
	if !ok || voterCount != 3 {
		return RangeDescriptor{}, ErrCatalogCorrupt
	}
	voters := make([]uint64, voterCount)
	for i := range voters {
		voters[i], ok = readU64(encoded, position)
		if !ok {
			return RangeDescriptor{}, ErrCatalogCorrupt
		}
	}
	descriptor := RangeDescriptor{RangeID: string(id), Start: start, End: end, Generation: values[0], OwnerEpoch: values[1], GroupID: values[2], Voters: voters, Phase: phase}
	if err := descriptor.Validate(); err != nil {
		return RangeDescriptor{}, ErrCatalogCorrupt
	}
	return descriptor, nil
}
