package backoff

import (
	"testing"
	"time"
)

func TestDurationGrowsAndStaysWithinBounds(t *testing.T) {
	min, max := 100*time.Millisecond, 10*time.Second
	b := New(min, max, 2)

	var last time.Duration
	sawGrowth := false
	for attempt := 0; attempt < 20; attempt++ {
		ceiling := b.ceilingLocked(attempt)
		d := b.Duration()
		// The delay is drawn from [ceiling/2, ceiling].
		if d < ceiling/2 || d > ceiling {
			t.Fatalf("attempt %d: %v outside [%v, %v]", attempt, d, ceiling/2, ceiling)
		}
		if d > max {
			t.Fatalf("attempt %d: %v exceeds max %v", attempt, d, max)
		}
		if ceiling > last {
			sawGrowth = true
		}
		last = ceiling
	}
	if !sawGrowth {
		t.Fatal("ceiling never grew")
	}
	// After enough attempts the ceiling is pinned at max.
	if got := b.ceilingLocked(1000); got != max {
		t.Fatalf("ceiling at attempt 1000 = %v, want max %v", got, max)
	}
}

func TestResetReturnsToStart(t *testing.T) {
	b := New(time.Second, time.Minute, 2)
	for i := 0; i < 5; i++ {
		b.Duration()
	}
	if b.Attempt() != 5 {
		t.Fatalf("attempt = %d, want 5", b.Attempt())
	}
	b.Reset()
	if b.Attempt() != 0 {
		t.Fatalf("attempt after reset = %d, want 0", b.Attempt())
	}
	// The first ceiling after reset is the minimum again.
	if got := b.ceilingLocked(0); got != time.Second {
		t.Fatalf("first ceiling after reset = %v, want 1s", got)
	}
}

func TestNewNormalisesBadArguments(t *testing.T) {
	b := New(-1, -1, 0.5)
	if b.min <= 0 || b.max < b.min || b.factor <= 1 {
		t.Fatalf("bad args not normalised: %+v", b)
	}
	// It still produces a usable, bounded delay.
	if d := b.Duration(); d <= 0 || d > b.max {
		t.Fatalf("delay %v out of range", d)
	}
}

func TestDurationNeverExceedsMaxEvenWhenMinEqualsMax(t *testing.T) {
	b := New(time.Second, time.Second, 2)
	for i := 0; i < 10; i++ {
		if d := b.Duration(); d < 500*time.Millisecond || d > time.Second {
			t.Fatalf("delay %v outside [500ms, 1s] when min==max", d)
		}
	}
}
