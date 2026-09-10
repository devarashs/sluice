package limits

import (
	"context"
	"sync/atomic"
)

// Concurrency caps how many connections a mode handles at once.
//
// Acquire blocks rather than rejecting. A listener that waits before calling
// Accept leaves excess clients in the kernel's accept backlog, where they
// wait for a slot without costing a descriptor or a goroutine, instead of
// being accepted only to be closed, which at high rates is pure churn.
//
// Because acceptors hold a slot while they wait in Accept, the number of
// held slots is not the number of connections in flight; a listener counts
// those itself.
type Concurrency struct {
	// slots is nil when there is no cap.
	slots chan struct{}
	limit int
	held  atomic.Int64
}

// NewConcurrency returns a cap of limit connections; limit <= 0 means none.
func NewConcurrency(limit int) *Concurrency {
	c := &Concurrency{limit: max(limit, 0)}
	if c.limit > 0 {
		c.slots = make(chan struct{}, c.limit)
	}
	return c
}

// Acquire takes a slot, waiting until one is free or ctx ends.
func (c *Concurrency) Acquire(ctx context.Context) error {
	if c.slots != nil {
		select {
		case c.slots <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.held.Add(1)
	return nil
}

// TryAcquire takes a slot only if one is free right now.
func (c *Concurrency) TryAcquire() bool {
	if c.slots != nil {
		select {
		case c.slots <- struct{}{}:
		default:
			return false
		}
	}
	c.held.Add(1)
	return true
}

// Release returns a slot taken by Acquire or TryAcquire.
func (c *Concurrency) Release() {
	c.held.Add(-1)
	if c.slots != nil {
		<-c.slots
	}
}

// Held is the number of slots currently taken, including those reserved by
// acceptors waiting for a connection.
func (c *Concurrency) Held() int {
	return int(c.held.Load())
}

// Limit is the cap, or 0 when there is none.
func (c *Concurrency) Limit() int {
	return c.limit
}
