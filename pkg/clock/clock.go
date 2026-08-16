// Package clock provides an injectable wall clock and timer abstraction. The
// module pins Go's synchronous timer-channel semantics with
// godebug asynctimerchan=0; an operator-level GODEBUG override is outside this
// package's contract. The manual implementation is deterministic and safe for
// concurrent test code; production code should receive a Clock explicitly
// rather than using a process-global test hook.
package clock

import (
	"sync"
	"time"
)

// Timer is the subset of time.Timer behavior needed by VBDB. C is a method so
// implementations can be deterministic without exposing mutable internals.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(time.Duration) bool
}

// Clock supplies time and timers to code that needs deterministic tests.
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

var _ Clock = Real{}
var _ Clock = (*Manual)(nil)

// Real is the production clock backed by the standard library. Its channel
// capacity follows Go's asynctimerchan setting (the module pins synchronous
// capacity zero by default). Manual timers intentionally use a one-element
// buffered channel so Advance can deliver deterministically while holding the
// clock lock; callers must not use channel capacity as a clock contract.
type Real struct{}

func (Real) Now() time.Time { return time.Now() }

func (Real) NewTimer(duration time.Duration) Timer {
	return realTimer{timer: time.NewTimer(duration)}
}

type realTimer struct{ timer *time.Timer }

var _ Timer = realTimer{}
var _ Timer = (*manualTimer)(nil)

func (t realTimer) C() <-chan time.Time               { return t.timer.C }
func (t realTimer) Stop() bool                        { return t.timer.Stop() }
func (t realTimer) Reset(duration time.Duration) bool { return t.timer.Reset(duration) }

// Manual is a concurrency-safe clock controlled by Advance. Timers do not
// use goroutines: advancing time synchronously delivers every timer due at or
// before the new instant.
type Manual struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*manualTimer]struct{}
}

// Prune abandons all currently active timers and releases the Manual clock's
// references to them. It is useful for test teardown when a test intentionally
// leaves timers unread; normal code should Stop timers it owns. Timers already
// delivered or stopped are removed as part of their normal operation.
func (c *Manual) Prune() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := len(c.timers)
	for timer := range c.timers {
		timer.active = false
		delete(c.timers, timer)
	}
	return count
}

func NewManual(start time.Time) *Manual {
	return &Manual{now: start.Round(0), timers: make(map[*manualTimer]struct{})}
}

func (c *Manual) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward and fires all timers whose deadlines have
// arrived. A negative duration is a programmer error and panics rather than
// silently violating monotonic time assumptions.
func (c *Manual) Advance(duration time.Duration) {
	if duration < 0 {
		panic("clock: cannot move manual clock backwards")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
	for timer := range c.timers {
		if timer.active && !timer.deadline.After(c.now) {
			timer.active = false
			delete(c.timers, timer)
			timer.deliverLocked(timer.deadline)
		}
	}
}

func (c *Manual) NewTimer(duration time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timers == nil {
		c.timers = make(map[*manualTimer]struct{})
	}
	timer := &manualTimer{clock: c, ch: make(chan time.Time, 1)}
	timer.deadline = c.now.Add(duration)
	if duration <= 0 {
		// A zero or negative standard-library timer is ready immediately.
		// The buffered channel makes this delivery nonblocking and avoids a
		// goroutine in deterministic tests.
		timer.deliverLocked(c.now)
		return timer
	}
	timer.active = true
	c.timers[timer] = struct{}{}
	return timer
}

type manualTimer struct {
	clock    *Manual
	ch       chan time.Time
	deadline time.Time
	active   bool
	pending  bool
}

func (t *manualTimer) C() <-chan time.Time { return t.ch }

// deliverLocked records delivery under the clock lock. A full channel means a
// timer tick would be silently dropped, so fail loudly while the owning clock
// lock is held. pending remains true until Stop or Reset acknowledges the
// expiration; those methods drain an unread tick without relying on len as
// the source of truth.
func (t *manualTimer) deliverLocked(now time.Time) {
	if t.pending {
		panic("clock: timer pending-delivery invariant violated")
	}
	select {
	case t.ch <- now:
	default:
		panic("clock: timer channel invariant violated")
	}
	t.pending = true
}

func (t *manualTimer) acknowledgePendingLocked() bool {
	if !t.pending {
		return false
	}
	t.pending = false
	select {
	case <-t.ch:
		return true
	default:
		// The consumer already read the delivered tick. The pending state
		// still prevented a second delivery until this acknowledgement.
		return false
	}
}

func (t *manualTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if !t.active {
		return t.acknowledgePendingLocked()
	}
	if t.pending {
		panic("clock: active timer pending-delivery invariant violated")
	}
	t.active = false
	delete(t.clock.timers, t)
	return true
}

func (t *manualTimer) Reset(duration time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasActive := t.active
	if t.active && t.pending {
		panic("clock: active timer pending-delivery invariant violated")
	}
	if !t.active && t.pending {
		// An unread expiration counts as previously active. A consumed
		// expiration is acknowledged without changing that result.
		wasActive = t.acknowledgePendingLocked()
	}
	delete(t.clock.timers, t)
	t.deadline = t.clock.now.Add(duration)
	if duration <= 0 {
		t.active = false
		t.deliverLocked(t.clock.now)
		return wasActive
	}
	t.active = true
	t.clock.timers[t] = struct{}{}
	return wasActive
}
