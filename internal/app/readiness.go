package app

import (
	"errors"
	"sync"
)

// Readiness is the switch a mode flips once it can take traffic and flips
// back when it starts to stop, so a load balancer polling /readyz drains it
// before the listener closes.
type Readiness struct {
	mu  sync.Mutex
	err error
}

// NewReadiness starts not ready, because nothing is listening yet.
func NewReadiness() *Readiness {
	return &Readiness{err: errors.New("starting")}
}

// Set records the current state: nil for ready, an error saying why not.
func (r *Readiness) Set(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

// Check reports the current state and satisfies admin.ReadinessFunc.
func (r *Readiness) Check() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}
