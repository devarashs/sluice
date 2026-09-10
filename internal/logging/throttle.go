package logging

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Throttle lets a hot path report a recurring problem at most once per
// interval, saying how many occurrences it swallowed, so a next hop that is
// down does not turn every one of thousands of connections per second into
// a log line.
type Throttle struct {
	interval   time.Duration
	now        func() time.Time
	mu         sync.Mutex
	next       time.Time
	suppressed int
}

// NewThrottle allows one record per interval.
func NewThrottle(interval time.Duration) *Throttle {
	return &Throttle{interval: interval, now: time.Now}
}

// Log emits the record when the interval has passed since the last one,
// adding a suppressed attribute with the count swallowed in between. The
// first call always logs.
func (t *Throttle) Log(logger *slog.Logger, level slog.Level, msg string, args ...any) {
	t.mu.Lock()
	now := t.now()
	if now.Before(t.next) {
		t.suppressed++
		t.mu.Unlock()
		return
	}
	suppressed := t.suppressed
	t.suppressed = 0
	t.next = now.Add(t.interval)
	t.mu.Unlock()

	if suppressed > 0 {
		args = append(args, "suppressed", suppressed)
	}
	logger.Log(context.Background(), level, msg, args...)
}
