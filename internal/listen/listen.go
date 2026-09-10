// Package listen accepts TCP connections the way a process built for millions
// of them has to: several accept loops per address, a concurrency cap that
// makes the loop wait rather than accept-and-close, a per-client rate check
// before any goroutine is spent, TCP keepalive so dead peers are reaped by the
// kernel, and a shutdown that stops accepting first and gives connections in
// flight a bounded time to finish.
//
// On Linux each acceptor is its own socket bound with SO_REUSEPORT, so the
// kernel spreads incoming connections across independent accept queues.
// Elsewhere one socket is shared by all acceptors, which still overlaps the
// work of accepting with the work of handing off.
package listen

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/devarashs/sluice/internal/config"
	"github.com/devarashs/sluice/internal/limits"
)

// Config is the `listener` block shared by modes that accept connections.
type Config struct {
	// Acceptors is the number of accept loops per address. Default: the
	// number of CPUs, at most MaxAcceptors.
	Acceptors int `json:"acceptors"`
	// KeepAliveIdle is how long a connection may be silent before the kernel
	// starts probing it. Default 30s. Zero means the default; keepalive is
	// never disabled, because a silent dead peer otherwise holds a slot until
	// the idle timeout.
	KeepAliveIdle config.Duration `json:"keepAliveIdle"`
	// KeepAliveInterval is the gap between probes. Default 10s.
	KeepAliveInterval config.Duration `json:"keepAliveInterval"`
	// KeepAliveCount is how many unanswered probes declare the peer dead.
	// Default 3.
	KeepAliveCount int `json:"keepAliveCount"`
	// DrainTimeout is how long, once shutdown begins, connections in flight
	// may finish before they are closed. Default 30s.
	DrainTimeout config.Duration `json:"drainTimeout"`
}

// Defaults applied by Validate.
const (
	MaxAcceptors             = 64
	DefaultKeepAliveIdle     = 30 * time.Second
	DefaultKeepAliveInterval = 10 * time.Second
	DefaultKeepAliveCount    = 3
	DefaultDrainTimeout      = 30 * time.Second
)

// Validate applies defaults and rejects impossible values. prefix is the JSON
// path of this block, such as "listener", used in errors.
func (c *Config) Validate(prefix string) error {
	if c.Acceptors < 0 {
		return config.Invalid(prefix+".acceptors", "must not be negative, got %d", c.Acceptors)
	}
	if c.Acceptors > MaxAcceptors {
		return config.Invalid(prefix+".acceptors", "%d is more than the %d maximum", c.Acceptors, MaxAcceptors)
	}
	if c.Acceptors == 0 {
		c.Acceptors = min(runtime.NumCPU(), MaxAcceptors)
	}
	if c.KeepAliveIdle == 0 {
		c.KeepAliveIdle = config.Duration(DefaultKeepAliveIdle)
	}
	if c.KeepAliveInterval == 0 {
		c.KeepAliveInterval = config.Duration(DefaultKeepAliveInterval)
	}
	if c.KeepAliveCount < 0 {
		return config.Invalid(prefix+".keepAliveCount", "must not be negative, got %d", c.KeepAliveCount)
	}
	if c.KeepAliveCount == 0 {
		c.KeepAliveCount = DefaultKeepAliveCount
	}
	if c.DrainTimeout == 0 {
		c.DrainTimeout = config.Duration(DefaultDrainTimeout)
	}
	return nil
}

// Guards are the limits the accept loop enforces. Cap must not be nil; a
// nil PerClient allows every client.
type Guards struct {
	Cap       *limits.Concurrency
	PerClient *limits.PerClient
}

// Handler serves one accepted connection and must close it before returning.
// Its context ends when the drain timeout expires after shutdown began, at
// which point the handler is expected to stop promptly.
type Handler func(ctx context.Context, conn net.Conn)

// Stats counts what the accept loops did.
type Stats struct {
	// Accepted connections handed to a handler.
	Accepted uint64
	// RejectedByRate connections closed because the client exceeded its rate.
	RejectedByRate uint64
	// Active handlers running now.
	Active int
}

// Listener owns one or more bound sockets for a single address.
type Listener struct {
	cfg      Config
	guards   Guards
	logger   *slog.Logger
	sockets  []net.Listener
	handlers sync.WaitGroup
	accepted atomic.Uint64
	rejected atomic.Uint64
	// active counts handlers in flight. It is kept apart from the cap's held
	// slots, which also include acceptors waiting in Accept.
	active    atomic.Int64
	closeOnce sync.Once
}

// Listen binds addr with cfg.Acceptors sockets on Linux, or one socket
// elsewhere, and returns without accepting anything. cfg must have passed
// Validate.
func Listen(addr string, cfg Config, guards Guards, logger *slog.Logger) (*Listener, error) {
	if guards.Cap == nil {
		return nil, errors.New("listen: Guards.Cap must not be nil")
	}
	if cfg.Acceptors <= 0 {
		return nil, errors.New("listen: config must be validated first")
	}

	listenConfig := net.ListenConfig{
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     cfg.KeepAliveIdle.Duration(),
			Interval: cfg.KeepAliveInterval.Duration(),
			Count:    cfg.KeepAliveCount,
		},
	}
	sockets := 1
	if reusePortSupported {
		listenConfig.Control = reusePortControl
		sockets = cfg.Acceptors
	}

	l := &Listener{cfg: cfg, guards: guards, logger: logger}
	bindAddr := addr
	for i := 0; i < sockets; i++ {
		socket, err := listenConfig.Listen(context.Background(), "tcp", bindAddr)
		if err != nil {
			l.Close()
			return nil, fmt.Errorf("listen: %s: %w", addr, err)
		}
		l.sockets = append(l.sockets, socket)
		// With a kernel-chosen port, every further socket must bind the port
		// the first one got, or they would each get a different one.
		if i == 0 {
			bindAddr = socket.Addr().String()
		}
	}
	return l, nil
}

// Addr is the bound address, with the real port when 0 was requested.
func (l *Listener) Addr() net.Addr {
	return l.sockets[0].Addr()
}

// Sockets is how many bound sockets back this listener.
func (l *Listener) Sockets() int {
	return len(l.sockets)
}

// Close releases the sockets. Serve does this itself; Close is for a
// Listener that is never served.
func (l *Listener) Close() {
	l.closeOnce.Do(func() {
		for _, socket := range l.sockets {
			socket.Close()
		}
	})
}

// Stats reports the counters so far.
func (l *Listener) Stats() Stats {
	return Stats{
		Accepted:       l.accepted.Load(),
		RejectedByRate: l.rejected.Load(),
		Active:         int(l.active.Load()),
	}
}

// Serve runs the accept loops until ctx ends, then stops accepting, lets
// handlers in flight finish for the drain timeout, cancels the rest, and
// returns once every handler has returned. It returns nil on a clean stop
// and the first accept failure that was not caused by the stop otherwise.
func (l *Listener) Serve(ctx context.Context, handler Handler) error {
	handlerCtx, cancelHandlers := context.WithCancel(context.Background())
	defer cancelHandlers()

	acceptors := l.cfg.Acceptors
	if !reusePortSupported {
		// One socket, shared by all acceptors.
		acceptors = l.cfg.Acceptors
	}
	loopErrs := make(chan error, acceptors)
	var loops sync.WaitGroup
	for i := 0; i < acceptors; i++ {
		socket := l.sockets[i%len(l.sockets)]
		loops.Add(1)
		go func() {
			defer loops.Done()
			loopErrs <- l.acceptLoop(ctx, handlerCtx, socket, handler)
		}()
	}

	<-ctx.Done()
	l.Close()
	loops.Wait()
	close(loopErrs)
	var firstErr error
	for err := range loopErrs {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	l.drain(cancelHandlers)
	return firstErr
}

// drain waits for handlers up to the drain timeout, then cancels their
// context and waits for them to honour it.
func (l *Listener) drain(cancelHandlers context.CancelFunc) {
	done := make(chan struct{})
	go func() {
		l.handlers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(l.cfg.DrainTimeout.Duration()):
		l.logger.Warn("drain timeout reached; closing connections still in flight", "active", l.active.Load())
		cancelHandlers()
		<-done
	}
}

// acceptLoop accepts on one socket until ctx ends or the socket is closed.
// It returns nil when the stop was asked for and the error otherwise.
func (l *Listener) acceptLoop(ctx, handlerCtx context.Context, socket net.Listener, handler Handler) error {
	var backoff time.Duration
	for {
		// Wait for a slot before accepting, so excess clients queue in the
		// kernel backlog instead of being accepted and closed.
		if err := l.guards.Cap.Acquire(ctx); err != nil {
			return nil
		}

		conn, err := socket.Accept()
		if err != nil {
			l.guards.Cap.Release()
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return fmt.Errorf("listen: socket %s closed unexpectedly", socket.Addr())
			}
			// Out of descriptors or a transient kernel refusal: spinning would
			// only make it worse, so back off the way net/http does.
			backoff = nextBackoff(backoff)
			l.logger.Warn("accept failed; retrying", "addr", socket.Addr().String(), "error", err, "retryIn", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
			continue
		}
		backoff = 0

		if clientAddr, ok := limits.ClientAddr(conn.RemoteAddr()); ok && !l.guards.PerClient.Allow(clientAddr) {
			l.rejected.Add(1)
			l.logger.Debug("connection refused by rate limit", "client", clientAddr)
			conn.Close()
			l.guards.Cap.Release()
			continue
		}

		l.accepted.Add(1)
		l.active.Add(1)
		l.handlers.Add(1)
		go func() {
			defer l.handlers.Done()
			defer l.guards.Cap.Release()
			defer l.active.Add(-1)
			handler(handlerCtx, conn)
		}()
	}
}

// Accept retry backoff bounds, as used by net/http's Server.
const (
	minAcceptBackoff = 5 * time.Millisecond
	maxAcceptBackoff = time.Second
)

// nextBackoff doubles the previous delay from the minimum up to the cap.
func nextBackoff(previous time.Duration) time.Duration {
	if previous == 0 {
		return minAcceptBackoff
	}
	return min(previous*2, maxAcceptBackoff)
}
