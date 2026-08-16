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
	digest string
	value  string
}

// ledger is the smallest durable model needed to exercise the post-GC
// identity rule. A real implementation may use a generation/expiry fence
// instead of retired, but it must expose the same safety outcome.
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

func resultBytes(identity, digest, value string) int {
	return len(identity) + len(digest) + len(value)
}

func (l *ledger) apply(identity, digest, value string) disposition {
	if prior, ok := l.results[identity]; ok {
		if prior.digest != digest {
			return dispositionConflict
		}
		return dispositionReplayed
	}
	if _, ok := l.retired[identity]; ok {
		return dispositionRetired
	}
	projectedBytes := resultBytes(identity, digest, value)
	if len(l.results) >= l.fullResultLimit ||
		l.fullResultBytes+projectedBytes > l.fullResultByteLimit {
		return dispositionAdmissionExhausted
	}
	l.row = value
	l.results[identity] = result{digest: digest, value: value}
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
	l.retired[identity] = prior.digest
	delete(l.results, identity)
	l.fullResultBytes -= resultBytes(identity, prior.digest, prior.value)
	return true
}

// restart retains only state that was durably recorded before the crash.
func (l *ledger) restart() *ledger {
	recovered := newLedger(l.antiReuseLimit, l.fullResultLimit, l.fullResultByteLimit)
	recovered.row = l.row
	recovered.fullResultBytes = l.fullResultBytes
	for identity, prior := range l.results {
		recovered.results[identity] = prior
	}
	for identity, digest := range l.retired {
		recovered.retired[identity] = digest
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
