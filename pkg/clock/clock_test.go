package clock

import (
	"sync"
	"testing"
	"time"
)

func TestRealClockNowTimerAndStop(t *testing.T) {
	real := Real{}
	before := time.Now()
	now := real.Now()
	after := time.Now()
	if now.Before(before) || now.After(after) {
		t.Fatalf("Real.Now() = %s, outside [%s, %s]", now, before, after)
	}

	timer := real.NewTimer(10 * time.Millisecond)
	select {
	case <-timer.C():
		if timer.Stop() {
			t.Fatal("Stop reported true after the timer value was received")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Real timer did not fire within the outer timeout")
	}

	stopped := real.NewTimer(time.Hour)
	if !stopped.Stop() {
		t.Fatal("Stop reported false for a running timer")
	}
	if stopped.Stop() {
		t.Fatal("second Stop reported true")
	}
}

func TestManualTimerChannelCapacityAndPrune(t *testing.T) {
	manual := NewManual(time.Unix(0, 0))
	timer := manual.NewTimer(time.Hour)
	if got := cap(timer.C()); got != 1 {
		t.Fatalf("manual timer channel capacity = %d, want one buffered slot", got)
	}
	if got := manual.Prune(); got != 1 {
		t.Fatalf("Prune removed %d timers, want one", got)
	}
	if timer.Stop() {
		t.Fatal("pruned timer still reported active")
	}
}

func TestRealTimerChannelSemanticsArePinned(t *testing.T) {
	// The module's godebug directive makes this synchronous (cap 0). Running
	// this package with GODEBUG=asynctimerchan=1 intentionally fails here and
	// in the unread-expiration parity tests; that override is operator-owned.
	timer := Real{}.NewTimer(time.Hour)
	defer timer.Stop()
	if got := cap(timer.C()); got != 0 {
		t.Fatalf("real timer channel capacity = %d, want synchronous capacity 0", got)
	}
}

func TestRealAndManualUnreadExpiredStopParity(t *testing.T) {
	manual := NewManual(time.Unix(100, 0))
	manualTimer := manual.NewTimer(time.Second)
	manual.Advance(2 * time.Second)

	realTimer := Real{}.NewTimer(20 * time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	if !manualTimer.Stop() {
		t.Fatal("manual Stop reported false for an unread expiration")
	}
	if !realTimer.Stop() {
		t.Fatal("real Stop reported false for an unread expiration")
	}
}

func TestRealAndManualUnreadExpiredResetParity(t *testing.T) {
	manual := NewManual(time.Unix(100, 0))
	manualTimer := manual.NewTimer(time.Second)
	manual.Advance(2 * time.Second)

	realTimer := Real{}.NewTimer(20 * time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	if !manualTimer.Reset(time.Second) {
		t.Fatal("manual Reset reported false for an unread expiration")
	}
	if !realTimer.Reset(20 * time.Millisecond) {
		t.Fatal("real Reset reported false for an unread expiration")
	}
	manual.Advance(time.Second)
	select {
	case <-manualTimer.C():
	case <-time.After(5 * time.Second):
		t.Fatal("manual reset timer did not fire")
	}
	select {
	case <-realTimer.C():
	case <-time.After(5 * time.Second):
		t.Fatal("real reset timer did not fire")
	}
}

func TestRealAndManualReceivedExpirationStopParity(t *testing.T) {
	manual := NewManual(time.Unix(100, 0))
	manualTimer := manual.NewTimer(time.Second)
	manual.Advance(time.Second)
	<-manualTimer.C()
	if manualTimer.Stop() {
		t.Fatal("manual Stop reported true after receiving expiration")
	}

	realTimer := Real{}.NewTimer(20 * time.Millisecond)
	select {
	case <-realTimer.C():
	case <-time.After(5 * time.Second):
		t.Fatal("real timer did not expire")
	}
	if realTimer.Stop() {
		t.Fatal("real Stop reported true after receiving expiration")
	}
}

func TestManualTimerFiresAtDeadline(t *testing.T) {
	start := time.Unix(100, 0)
	c := NewManual(start)
	timer := c.NewTimer(5 * time.Second)
	c.Advance(4 * time.Second)
	select {
	case <-timer.C():
		t.Fatal("timer fired early")
	default:
	}
	c.Advance(time.Second)
	select {
	case got := <-timer.C():
		if !got.Equal(start.Add(5 * time.Second)) {
			t.Fatalf("timer fired at %s", got)
		}
	default:
		t.Fatal("timer did not fire at deadline")
	}
}

func TestManualTimerOvershootDeliversDeadline(t *testing.T) {
	start := time.Unix(100, 0)
	c := NewManual(start)
	timer := c.NewTimer(5 * time.Second)
	c.Advance(10 * time.Second)
	select {
	case got := <-timer.C():
		want := start.Add(5 * time.Second)
		if !got.Equal(want) {
			t.Fatalf("timer fired at %s, want deadline %s", got, want)
		}
	default:
		t.Fatal("timer did not fire after clock overshoot")
	}
}

func TestManualStopDrainsUnreadPendingTick(t *testing.T) {
	c := NewManual(time.Unix(100, 0))
	timer := c.NewTimer(time.Second)
	c.Advance(10 * time.Second)
	if !timer.Stop() {
		t.Fatal("Stop reported false for an unread fired timer")
	}
	select {
	case <-timer.C():
		t.Fatal("Stop left the pending tick queued")
	default:
	}
}

func TestManualResetDrainsUnreadPendingTick(t *testing.T) {
	start := time.Unix(100, 0)
	c := NewManual(start)
	timer := c.NewTimer(time.Second)
	c.Advance(10 * time.Second)
	if !timer.Reset(2 * time.Second) {
		t.Fatal("Reset did not report the unread expiration as previously active")
	}
	c.Advance(2 * time.Second)
	select {
	case got := <-timer.C():
		want := start.Add(12 * time.Second)
		if !got.Equal(want) {
			t.Fatalf("reset timer fired at %s, want %s", got, want)
		}
	default:
		t.Fatal("reset timer did not fire")
	}
}

func TestManualStopAndReset(t *testing.T) {
	c := NewManual(time.Unix(0, 0))
	timer := c.NewTimer(time.Second)
	if !timer.Stop() {
		t.Fatal("first Stop reported inactive")
	}
	if timer.Stop() {
		t.Fatal("second Stop reported active")
	}
	if timer.Reset(2 * time.Second) {
		t.Fatal("Reset after Stop reported active")
	}
	c.Advance(2 * time.Second)
	select {
	case <-timer.C():
	default:
		t.Fatal("reset timer did not fire")
	}
	if timer.Reset(0) {
		t.Fatal("Reset after firing reported active")
	}
	select {
	case <-timer.C():
	default:
		t.Fatal("zero-duration reset was not immediately ready")
	}
}

func TestManualZeroAndNegativeTimersAreImmediatelyReady(t *testing.T) {
	c := NewManual(time.Unix(42, 7))
	for _, duration := range []time.Duration{0, -time.Nanosecond} {
		timer := c.NewTimer(duration)
		select {
		case got := <-timer.C():
			if !got.Equal(c.Now()) {
				t.Fatalf("timer fired at %s, want %s", got, c.Now())
			}
		default:
			t.Fatalf("timer with duration %s was not immediately ready", duration)
		}
		if timer.Stop() {
			t.Fatalf("immediate timer with duration %s reported active", duration)
		}
	}
}

func TestManualResetAfterStopIsImmediatelyReady(t *testing.T) {
	c := NewManual(time.Unix(0, 0))
	timer := c.NewTimer(time.Hour)
	if !timer.Stop() {
		t.Fatal("timer was not active before Stop")
	}
	if timer.Reset(-time.Second) {
		t.Fatal("Reset after Stop reported active")
	}
	select {
	case <-timer.C():
	default:
		t.Fatal("negative-duration reset was not immediately ready")
	}
}

func TestManualTimerDeliveryPanicsIfChannelIsNotEmpty(t *testing.T) {
	c := NewManual(time.Unix(0, 0))
	timer := c.NewTimer(time.Second).(*manualTimer)
	timer.ch <- c.Now()
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("timer delivery silently accepted a full channel")
		}
	}()
	c.Advance(time.Second)
}

func TestManualConcurrentAccess(t *testing.T) {
	c := NewManual(time.Unix(0, 0))
	const workers = 16
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				timer := c.NewTimer(time.Nanosecond)
				_ = c.Now()
				_ = timer.Stop()
				_ = timer.Reset(time.Nanosecond)
				_ = timer.Stop()
			}
		}()
	}
	for i := 0; i < workers; i++ {
		c.Advance(time.Nanosecond)
	}
	wg.Wait()
}

func TestManualRejectsBackwardsTime(t *testing.T) {
	c := NewManual(time.Unix(0, 0))
	defer func() {
		if recover() == nil {
			t.Fatal("Advance accepted a negative duration")
		}
	}()
	c.Advance(-time.Nanosecond)
}
