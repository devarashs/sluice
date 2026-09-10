// Package backoff computes the growing, jittered delays a client waits
// between reconnect attempts, so a server that has just restarted is not hit
// by every client at the same instant.
package backoff

import (
	"math"
	"math/rand"
	"sync"
	"time"
)

// Backoff produces delays that grow from Min toward Max by Factor and carry
// jitter, so many clients reconnecting at once spread their attempts out
// instead of arriving in a thundering herd. It is safe for concurrent use.
type Backoff struct {
	min    time.Duration
	max    time.Duration
	factor float64

	mu      sync.Mutex
	attempt int
	rand    *rand.Rand
}

// New returns a Backoff. A min at or below zero becomes a small default, a
// max below min is raised to min, and a factor at or below one becomes two.
func New(min, max time.Duration, factor float64) *Backoff {
	if min <= 0 {
		min = 100 * time.Millisecond
	}
	if max < min {
		max = min
	}
	if factor <= 1 {
		factor = 2
	}
	return &Backoff{
		min:    min,
		max:    max,
		factor: factor,
		rand:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Duration returns the next delay and advances the attempt counter. The delay
// is drawn from the upper half of the current ceiling, so it never collapses
// to near-zero yet still spreads clients across a window: for a ceiling c the
// delay is uniform in [c/2, c], with c growing min, min*factor, ... capped at
// max.
func (b *Backoff) Duration() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	ceiling := b.ceilingLocked(b.attempt)
	b.attempt++

	half := ceiling / 2
	return half + time.Duration(b.rand.Int63n(int64(half)+1))
}

// Reset returns the backoff to its starting delay, called after a connection
// has stayed up long enough to count as healthy.
func (b *Backoff) Reset() {
	b.mu.Lock()
	b.attempt = 0
	b.mu.Unlock()
}

// Attempt is how many delays have been handed out since the last Reset.
func (b *Backoff) Attempt() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempt
}

// ceilingLocked is min*factor^attempt, capped at max, computed so a large
// attempt count cannot overflow the duration.
func (b *Backoff) ceilingLocked(attempt int) time.Duration {
	scaled := float64(b.min) * math.Pow(b.factor, float64(attempt))
	if scaled >= float64(b.max) || math.IsInf(scaled, 0) {
		return b.max
	}
	return time.Duration(scaled)
}
