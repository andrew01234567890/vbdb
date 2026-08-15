// Package clock provides an injectable wall clock and timer abstraction. The
// manual implementation is deterministic and safe for concurrent test code;
// production code should receive a Clock explicitly rather than using a
// process-global test hook.
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

// Real is the production clock backed by the standard library.
type Real struct{}

func (Real) Now() time.Time { return time.Now() }

func (Real) NewTimer(duration time.Duration) Timer {
	return realTimer{timer: time.NewTimer(duration)}
}

type realTimer struct{ timer *time.Timer }

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
	c.now = c.now.Add(duration)
	for timer := range c.timers {
		if timer.active && !timer.deadline.After(c.now) {
			timer.active = false
			delete(c.timers, timer)
			// The channel has capacity one and this send is performed while
			// holding c.mu, so a concurrent Reset cannot race the delivery.
			timer.ch <- c.now
		}
	}
	c.mu.Unlock()
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
		timer.ch <- c.now
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
}

func (t *manualTimer) C() <-chan time.Time { return t.ch }

func (t *manualTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if !t.active {
		return false
	}
	t.active = false
	delete(t.clock.timers, t)
	return true
}

func (t *manualTimer) Reset(duration time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasActive := t.active
	if !wasActive {
		// Match the useful part of time.Timer's reset contract for this
		// deterministic implementation: discard an already queued event.
		select {
		case <-t.ch:
		default:
		}
	}
	delete(t.clock.timers, t)
	t.deadline = t.clock.now.Add(duration)
	if duration <= 0 {
		t.active = false
		t.ch <- t.clock.now
		return wasActive
	}
	t.active = true
	t.clock.timers[t] = struct{}{}
	return wasActive
}
