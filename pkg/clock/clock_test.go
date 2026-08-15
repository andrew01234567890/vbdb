package clock

import (
	"sync"
	"testing"
	"time"
)

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
