// Package idempotency contains the executable M1 contract model. It does not
// implement the later-milestone idempotency service.
package idempotency

import "testing"

type disposition uint8

const (
	dispositionApplied disposition = iota
	dispositionReplayed
	dispositionConflict
	dispositionRetired
	dispositionAdmissionExhausted
)

type result struct {
	digest   string
	status   uint16
	headers  []responseHeader
	body     string
	envelope string
}

type responseHeader struct {
	name  string
	value string
}

const (
	replayRecordMagic        = "VBR1"
	replayRecordMagicBytes   = len(replayRecordMagic)
	replayRecordVersionBytes = 1
	replayRecordLengthBytes  = 8
	replayStatusBytes        = 2
	replayHeaderCountBytes   = 4
	replayFieldLengthBytes   = 8
)

// ledger is the smallest durable model needed to exercise the post-GC
// identity rule. A real implementation may use a generation/expiry fence
// instead of retired, but it must expose the same safety outcome.
//
// The retired-evidence variant modeled here deliberately has no reclamation
// transition: once anti-reuse evidence is full, GC remains unavailable. Fresh
// identities may still be admitted while their complete result fits; when
// that retained-result budget fills, the scope is permanently unavailable to
// new identities until a durable fence/reclamation transition or capacity
// reconfiguration occurs.
type ledger struct {
	row                 string
	results             map[string]result
	retired             map[string]string
	antiReuseLimit      int
	fullResultLimit     int
	fullResultByteLimit int
	fullResultBytes     int
}

func newLedger(antiReuseLimit, fullResultLimit, fullResultByteLimit int) *ledger {
	return &ledger{
		results:             make(map[string]result),
		retired:             make(map[string]string),
		antiReuseLimit:      antiReuseLimit,
		fullResultLimit:     fullResultLimit,
		fullResultByteLimit: fullResultByteLimit,
	}
}

func defaultResult(digest, value string) result {
	return result{
		digest: digest,
		status: 200,
		headers: []responseHeader{
			{name: "Content-Type", value: "application/json"},
		},
		body:     value,
		envelope: value,
	}
}

func maxInt() int {
	return int(^uint(0) >> 1)
}

func addEncodedBytes(total *int, n int) bool {
	if total == nil || *total < 0 || n < 0 || n > maxInt()-*total {
		return false
	}
	*total += n
	return true
}

func addFramedString(total *int, value string) bool {
	return addEncodedBytes(total, replayFieldLengthBytes) && addEncodedBytes(total, len(value))
}

// encodedResultBytes is the exact size of replay-result record version 1:
// magic "VBR1", version 0x01, little-endian total-length[8], length-prefixed
// identity, length-prefixed digest, little-endian status[2], little-endian
// header-count[4], each length-prefixed header name/value, length-prefixed
// body, and length-prefixed response envelope. Every variable-length field
// includes its little-endian uint64 length prefix; no raw payload is admitted
// as a result.
func encodedResultBytes(identity string, prior result) (int, bool) {
	total := 0
	for _, fixed := range []int{
		replayRecordMagicBytes,
		replayRecordVersionBytes,
		replayRecordLengthBytes,
		replayStatusBytes,
		replayHeaderCountBytes,
	} {
		if !addEncodedBytes(&total, fixed) {
			return 0, false
		}
	}
	if !addFramedString(&total, identity) || !addFramedString(&total, prior.digest) {
		return 0, false
	}
	if uint64(len(prior.headers)) > uint64(^uint32(0)) {
		return 0, false
	}
	for _, header := range prior.headers {
		if !addFramedString(&total, header.name) || !addFramedString(&total, header.value) {
			return 0, false
		}
	}
	if !addFramedString(&total, prior.body) || !addFramedString(&total, prior.envelope) {
		return 0, false
	}
	return total, true
}

func resultBytes(identity, digest, value string) int {
	bytes, ok := encodedResultBytes(identity, defaultResult(digest, value))
	if !ok {
		return -1
	}
	return bytes
}

func (l *ledger) apply(identity, digest, value string) disposition {
	return l.applyResult(identity, defaultResult(digest, value))
}

func (l *ledger) applyResult(identity string, candidate result) disposition {
	if prior, ok := l.results[identity]; ok {
		if prior.digest != candidate.digest {
			return dispositionConflict
		}
		return dispositionReplayed
	}
	if _, ok := l.retired[identity]; ok {
		return dispositionRetired
	}
	projectedBytes, ok := encodedResultBytes(identity, candidate)
	if !ok || l.fullResultLimit < 0 || l.fullResultByteLimit < 0 ||
		len(l.results) >= l.fullResultLimit || l.fullResultBytes < 0 ||
		l.fullResultBytes > l.fullResultByteLimit {
		return dispositionAdmissionExhausted
	}
	// Subtract first so a corrupt or near-MaxInt counter cannot wrap the
	// projection check and acknowledge a mutation over the configured bound.
	if projectedBytes > l.fullResultByteLimit-l.fullResultBytes {
		return dispositionAdmissionExhausted
	}
	l.row = candidate.body
	l.results[identity] = candidate
	l.fullResultBytes += projectedBytes
	return dispositionApplied
}

// gcFullResult refuses to remove a result unless its durable anti-reuse
// evidence can be admitted atomically with the removal.
func (l *ledger) gcFullResult(identity string) bool {
	prior, ok := l.results[identity]
	if !ok || len(l.retired) >= l.antiReuseLimit {
		return false
	}
	priorBytes, ok := encodedResultBytes(identity, prior)
	if !ok || l.fullResultBytes < 0 || l.fullResultByteLimit < 0 ||
		l.fullResultBytes > l.fullResultByteLimit || priorBytes > l.fullResultBytes {
		return false
	}
	l.retired[identity] = prior.digest
	delete(l.results, identity)
	l.fullResultBytes -= priorBytes
	return true
}

// restart retains only state that was durably recorded before the crash and
// derives the byte counter from the recovered complete records. A malformed
// or overflowing recovered set leaves a corrupt marker so admission fails
// closed rather than trusting a persisted counter.
func (l *ledger) restart() *ledger {
	recovered := newLedger(l.antiReuseLimit, l.fullResultLimit, l.fullResultByteLimit)
	recovered.row = l.row
	var recoveredBytes int
	accountingOK := true
	for identity, prior := range l.results {
		recovered.results[identity] = prior
		priorBytes, ok := encodedResultBytes(identity, prior)
		if !ok || !addEncodedBytes(&recoveredBytes, priorBytes) {
			accountingOK = false
		}
	}
	for identity, digest := range l.retired {
		recovered.retired[identity] = digest
	}
	if accountingOK {
		recovered.fullResultBytes = recoveredBytes
	} else {
		recovered.fullResultBytes = -1
	}
	return recovered
}

func TestDelayedRetryAfterFullResultGCAndRestartCannotClobberLaterWrite(t *testing.T) {
	ledger := newLedger(1, 2, 1024)
	if got := ledger.apply("I", "A", "value-A"); got != dispositionApplied {
		t.Fatalf("initial I:A disposition = %d, want applied", got)
	}
	if got := ledger.apply("J", "B", "value-B"); got != dispositionApplied {
		t.Fatalf("later J:B disposition = %d, want applied", got)
	}
	if !ledger.gcFullResult("I") {
		t.Fatal("full-result GC did not durably retire I")
	}

	ledger = ledger.restart()
	if got := ledger.apply("I", "A", "value-A"); got != dispositionRetired {
		t.Fatalf("delayed I:A disposition = %d, want retired", got)
	}
	if ledger.row != "value-B" {
		t.Fatalf("delayed retry changed the later row to %q", ledger.row)
	}
}

func TestAntiReuseAdmissionExhaustionKeepsFullResultAndFailsClosed(t *testing.T) {
	ledger := newLedger(0, 2, 1024)
	if got := ledger.apply("I", "A", "value-A"); got != dispositionApplied {
		t.Fatalf("initial I:A disposition = %d, want applied", got)
	}
	if ledger.gcFullResult("I") {
		t.Fatal("GC removed a full result without anti-reuse capacity")
	}
	if got := ledger.apply("I", "A", "value-A"); got != dispositionReplayed {
		t.Fatalf("retry after failed GC disposition = %d, want replayed", got)
	}
	if ledger.row != "value-A" {
		t.Fatalf("failed GC changed the row to %q", ledger.row)
	}
	if got := ledger.apply("J", "B", "value-B"); got != dispositionApplied {
		t.Fatalf("fresh J:B with retained-result capacity = %d, want applied", got)
	}
	rowBefore := ledger.row
	bytesBefore := ledger.fullResultBytes
	resultsBefore := len(ledger.results)
	if got := ledger.apply("K", "C", "value-C"); got != dispositionAdmissionExhausted {
		t.Fatalf("fresh K:C after retained-result capacity filled = %d, want exhausted", got)
	}
	if ledger.row != rowBefore || ledger.fullResultBytes != bytesBefore || len(ledger.results) != resultsBefore {
		t.Fatalf("anti-reuse-exhausted admission mutated state: row=%q bytes=%d results=%d", ledger.row, ledger.fullResultBytes, len(ledger.results))
	}
	ledger = ledger.restart()
	if got := ledger.apply("K", "C", "value-C"); got != dispositionAdmissionExhausted {
		t.Fatalf("fresh K:C after restart with no reclamation = %d, want exhausted", got)
	}
}

func TestFreshAdmissionExhaustionLeavesStateUnchanged(t *testing.T) {
	ledger := newLedger(1, 2, resultBytes("I", "A", "value-A")+resultBytes("J", "B", "value-B"))
	if got := ledger.apply("I", "A", "value-A"); got != dispositionApplied {
		t.Fatalf("initial I:A disposition = %d, want applied", got)
	}
	if got := ledger.apply("J", "B", "value-B"); got != dispositionApplied {
		t.Fatalf("initial J:B disposition = %d, want applied", got)
	}

	rowBefore := ledger.row
	bytesBefore := ledger.fullResultBytes
	resultsBefore := len(ledger.results)
	if got := ledger.apply("K", "C", "value-C"); got != dispositionAdmissionExhausted {
		t.Fatalf("fresh K:C disposition = %d, want admission exhausted", got)
	}
	if ledger.row != rowBefore || ledger.fullResultBytes != bytesBefore || len(ledger.results) != resultsBefore {
		t.Fatalf("rejected fresh admission mutated durable model: row=%q bytes=%d results=%d", ledger.row, ledger.fullResultBytes, len(ledger.results))
	}

	ledger = ledger.restart()
	if got := ledger.apply("K", "C", "value-C"); got != dispositionAdmissionExhausted {
		t.Fatalf("fresh K:C after restart disposition = %d, want admission exhausted", got)
	}
	if ledger.row != rowBefore {
		t.Fatalf("rejected post-restart admission changed row to %q", ledger.row)
	}
}

func TestFreshAdmissionByteExhaustionLeavesStateUnchanged(t *testing.T) {
	first := resultBytes("I", "A", "value-A")
	ledger := newLedger(1, 10, first)
	if got := ledger.apply("I", "A", "value-A"); got != dispositionApplied {
		t.Fatalf("initial I:A disposition = %d, want applied", got)
	}
	if got := ledger.apply("J", "B", "value-B"); got != dispositionAdmissionExhausted {
		t.Fatalf("fresh J:B disposition = %d, want admission exhausted", got)
	}
	if ledger.row != "value-A" || ledger.fullResultBytes != first || len(ledger.results) != 1 {
		t.Fatalf("byte-rejected admission mutated durable model: row=%q bytes=%d results=%d", ledger.row, ledger.fullResultBytes, len(ledger.results))
	}
}

func TestAntiReuseExhaustionRemainsUnavailableWithoutReclamation(t *testing.T) {
	first := resultBytes("I", "A", "value-A")
	ledger := newLedger(1, 1, first*2)
	if got := ledger.apply("I", "A", "value-A"); got != dispositionApplied {
		t.Fatalf("initial I:A disposition = %d, want applied", got)
	}
	if !ledger.gcFullResult("I") {
		t.Fatal("initial result did not move to retired evidence")
	}
	if got := ledger.apply("J", "B", "value-B"); got != dispositionApplied {
		t.Fatalf("fresh J:B while result budget remains = %d, want applied", got)
	}
	if ledger.gcFullResult("J") {
		t.Fatal("GC reclaimed a result after anti-reuse evidence became full")
	}
	if got := ledger.apply("K", "C", "value-C"); got != dispositionAdmissionExhausted {
		t.Fatalf("fresh K:C after retained budget filled = %d, want exhausted", got)
	}
	ledger = ledger.restart()
	if ledger.gcFullResult("J") {
		t.Fatal("restart created an anti-reuse reclamation path")
	}
	if got := ledger.apply("K", "C", "value-C"); got != dispositionAdmissionExhausted {
		t.Fatalf("fresh K:C after restart = %d, want exhausted", got)
	}
}

func TestFullResultRecordEncodingIsCompleteAndVersioned(t *testing.T) {
	identity := "request-1"
	candidate := result{
		digest: "digest-1",
		status: 201,
		headers: []responseHeader{
			{name: "Cache-Control", value: "no-store, private"},
			{name: "Content-Type", value: "application/json"},
		},
		body:     "body-bytes",
		envelope: `{"_version":"v1","value":"body-bytes"}`,
	}
	got, ok := encodedResultBytes(identity, candidate)
	if !ok {
		t.Fatal("complete replay result did not encode")
	}
	framed := func(value string) int { return replayFieldLengthBytes + len(value) }
	want := replayRecordMagicBytes + replayRecordVersionBytes + replayRecordLengthBytes +
		replayStatusBytes + replayHeaderCountBytes +
		framed(identity) + framed(candidate.digest) +
		framed(candidate.headers[0].name) + framed(candidate.headers[0].value) +
		framed(candidate.headers[1].name) + framed(candidate.headers[1].value) +
		framed(candidate.body) + framed(candidate.envelope)
	if got != want {
		t.Fatalf("encoded complete replay record bytes=%d, want exact versioned framing size %d", got, want)
	}

	withOnlyDigest := candidate
	withOnlyDigest.status = 0
	withOnlyDigest.headers = nil
	withOnlyDigest.body = ""
	withOnlyDigest.envelope = ""
	minimum, ok := encodedResultBytes(identity, withOnlyDigest)
	if !ok || got <= minimum {
		t.Fatalf("complete replay fields were not charged: complete=%d minimum=%d ok=%v", got, minimum, ok)
	}
}

func TestFullResultAdmissionBoundariesAndStateUnchanged(t *testing.T) {
	identity := "I"
	candidate := defaultResult("A", "value-A")
	projected, ok := encodedResultBytes(identity, candidate)
	if !ok {
		t.Fatal("test result did not encode")
	}

	t.Run("zero-count", func(t *testing.T) {
		ledger := newLedger(1, 0, projected)
		if got := ledger.applyResult(identity, candidate); got != dispositionAdmissionExhausted {
			t.Fatalf("zero-count disposition = %d, want exhausted", got)
		}
		if ledger.row != "" || len(ledger.results) != 0 || ledger.fullResultBytes != 0 {
			t.Fatalf("zero-count rejection mutated state: row=%q results=%d bytes=%d", ledger.row, len(ledger.results), ledger.fullResultBytes)
		}
	})

	t.Run("zero-bytes-and-oversized-single", func(t *testing.T) {
		for _, limit := range []int{0, projected - 1} {
			ledger := newLedger(1, 1, limit)
			if got := ledger.applyResult(identity, candidate); got != dispositionAdmissionExhausted {
				t.Fatalf("byte limit %d disposition = %d, want exhausted", limit, got)
			}
			if ledger.row != "" || len(ledger.results) != 0 || ledger.fullResultBytes != 0 {
				t.Fatalf("byte rejection mutated state at limit %d: row=%q results=%d bytes=%d", limit, ledger.row, len(ledger.results), ledger.fullResultBytes)
			}
		}
	})

	t.Run("exact-boundary", func(t *testing.T) {
		ledger := newLedger(1, 1, projected)
		if got := ledger.applyResult(identity, candidate); got != dispositionApplied {
			t.Fatalf("exact boundary disposition = %d, want applied", got)
		}
		if ledger.fullResultBytes != projected {
			t.Fatalf("exact boundary bytes=%d, want %d", ledger.fullResultBytes, projected)
		}
	})

	t.Run("count-full-and-byte-full", func(t *testing.T) {
		ledger := newLedger(1, 2, projected)
		if got := ledger.applyResult(identity, candidate); got != dispositionApplied {
			t.Fatalf("first boundary result disposition = %d, want applied", got)
		}
		rowBefore := ledger.row
		bytesBefore := ledger.fullResultBytes
		if got := ledger.apply("J", "B", "value-B"); got != dispositionAdmissionExhausted {
			t.Fatalf("byte-full disposition = %d, want exhausted", got)
		}
		if ledger.row != rowBefore || ledger.fullResultBytes != bytesBefore || len(ledger.results) != 1 {
			t.Fatalf("byte-full rejection mutated state: row=%q bytes=%d results=%d", ledger.row, ledger.fullResultBytes, len(ledger.results))
		}

		ledger = newLedger(1, 1, projected+resultBytes("J", "B", "value-B"))
		if got := ledger.applyResult(identity, candidate); got != dispositionApplied {
			t.Fatalf("count-boundary result disposition = %d, want applied", got)
		}
		rowBefore = ledger.row
		bytesBefore = ledger.fullResultBytes
		if got := ledger.apply("J", "B", "value-B"); got != dispositionAdmissionExhausted {
			t.Fatalf("count-full disposition = %d, want exhausted", got)
		}
		if ledger.row != rowBefore || ledger.fullResultBytes != bytesBefore || len(ledger.results) != 1 {
			t.Fatalf("count-full rejection mutated state: row=%q bytes=%d results=%d", ledger.row, ledger.fullResultBytes, len(ledger.results))
		}
	})
}

func TestFullResultAdmissionRejectsNearMaxIntAndCorruptCounters(t *testing.T) {
	candidate := defaultResult("A", "value-A")
	projected, ok := encodedResultBytes("I", candidate)
	if !ok || projected < 2 {
		t.Fatalf("unexpected projected result size %d (ok=%v)", projected, ok)
	}
	max := maxInt()
	for _, counter := range []int{max - 1, max, -1} {
		ledger := newLedger(1, 2, max)
		ledger.row = "stable"
		ledger.fullResultBytes = counter
		if got := ledger.applyResult("I", candidate); got != dispositionAdmissionExhausted {
			t.Errorf("counter %d disposition = %d, want exhausted", counter, got)
		}
		if ledger.row != "stable" || len(ledger.results) != 0 || ledger.fullResultBytes != counter {
			t.Errorf("counter %d rejection mutated state: row=%q results=%d bytes=%d", counter, ledger.row, len(ledger.results), ledger.fullResultBytes)
		}
	}
}

func TestGCAndRestartReaccountCompleteResultBytes(t *testing.T) {
	first := defaultResult("A", "value-A")
	second := defaultResult("B", "value-B")
	firstBytes, ok := encodedResultBytes("I", first)
	if !ok {
		t.Fatal("first result did not encode")
	}
	secondBytes, ok := encodedResultBytes("J", second)
	if !ok {
		t.Fatal("second result did not encode")
	}
	ledger := newLedger(1, 3, firstBytes+secondBytes+resultBytes("K", "C", "value-C"))
	if got := ledger.applyResult("I", first); got != dispositionApplied {
		t.Fatalf("first result disposition = %d, want applied", got)
	}
	if got := ledger.applyResult("J", second); got != dispositionApplied {
		t.Fatalf("second result disposition = %d, want applied", got)
	}
	if ledger.fullResultBytes != firstBytes+secondBytes {
		t.Fatalf("pre-GC bytes=%d, want %d", ledger.fullResultBytes, firstBytes+secondBytes)
	}
	if !ledger.gcFullResult("I") {
		t.Fatal("GC did not remove first result")
	}
	if ledger.fullResultBytes != secondBytes {
		t.Fatalf("post-GC bytes=%d, want %d", ledger.fullResultBytes, secondBytes)
	}

	ledger.fullResultBytes = maxInt()
	ledger = ledger.restart()
	if ledger.fullResultBytes != secondBytes {
		t.Fatalf("restart did not re-account complete records: bytes=%d, want %d", ledger.fullResultBytes, secondBytes)
	}
	if got := ledger.apply("K", "C", "value-C"); got != dispositionApplied {
		t.Fatalf("post-restart admission = %d, want applied", got)
	}
	if ledger.fullResultBytes != secondBytes+resultBytes("K", "C", "value-C") {
		t.Fatalf("post-restart bytes=%d, want %d", ledger.fullResultBytes, secondBytes+resultBytes("K", "C", "value-C"))
	}
}
