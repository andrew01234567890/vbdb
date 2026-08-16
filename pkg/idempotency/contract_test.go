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
)

type result struct {
	digest string
	value  string
}

// ledger is the smallest durable model needed to exercise the post-GC
// identity rule. A real implementation may use a generation/expiry fence
// instead of retired, but it must expose the same safety outcome.
type ledger struct {
	row            string
	results        map[string]result
	retired        map[string]string
	antiReuseLimit int
}

func newLedger(antiReuseLimit int) *ledger {
	return &ledger{
		results:        make(map[string]result),
		retired:        make(map[string]string),
		antiReuseLimit: antiReuseLimit,
	}
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
	l.row = value
	l.results[identity] = result{digest: digest, value: value}
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
	return true
}

// restart retains only state that was durably recorded before the crash.
func (l *ledger) restart() *ledger {
	recovered := newLedger(l.antiReuseLimit)
	recovered.row = l.row
	for identity, prior := range l.results {
		recovered.results[identity] = prior
	}
	for identity, digest := range l.retired {
		recovered.retired[identity] = digest
	}
	return recovered
}

func TestDelayedRetryAfterFullResultGCAndRestartCannotClobberLaterWrite(t *testing.T) {
	ledger := newLedger(1)
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
	ledger := newLedger(0)
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
