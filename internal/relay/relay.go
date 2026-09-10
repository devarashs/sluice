// Package relay moves bytes between two connections in both directions until
// both sides are done, then reports how many bytes went each way and why it
// stopped.
//
// Capacity is decided by what one relayed connection costs, so the package is
// built around two facts. On Linux, a copy between two *net.TCPConn goes
// through splice(2) and holds no user-space buffer at all; every other pair
// copies through one small pooled buffer per direction.
//
// The idle timeout is judged for the relay as a whole, not per direction, so
// a download with a silent upload side is never cut off. A watchdog keeps an
// activity clock that the buffered path advances on every write. Because a
// spliced copy cannot report progress while it runs, the watchdog does not
// trust the clock when it looks stale: it pokes both copy loops by expiring
// their read deadlines, which makes them return and publish whatever moved,
// and only then decides. A direction that is stuck in a write for a whole
// interval cannot answer the poke, and that is exactly a peer that has
// stopped draining, so it is judged idle too.
package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultBufferSize is the per-direction copy buffer for pairs that cannot
// splice. 16KiB is the largest TLS record, so an encrypted hop moves one
// record per read without a buffer twice that size sitting idle on every
// connection.
const DefaultBufferSize = 16 * 1024

// spliceChunk bounds one kernel-side copy so that byte counts and the
// activity clock advance at least once per chunk on a busy spliced relay,
// rather than only when the copy ends.
const spliceChunk = 1 << 20

// ErrIdle reports that neither direction carried a byte for Options.IdleTimeout.
var ErrIdle = errors.New("relay: idle timeout")

// Options control one relay.
type Options struct {
	// IdleTimeout ends the relay once neither direction has carried a byte
	// for this long. The relay is checked every half interval, so it closes
	// between one and one and a half intervals after going quiet, and a peer
	// that stops draining is caught within two. Zero disables it, and then a
	// peer that goes silent without closing holds the relay open until TCP
	// keepalive or a reset ends it.
	IdleTimeout time.Duration
	// BufferSize is the per-direction copy buffer for pairs that cannot
	// splice. Zero means DefaultBufferSize. Spliced pairs use no buffer.
	BufferSize int
}

// Result reports what a finished relay moved and why it stopped.
type Result struct {
	BytesAToB int64
	BytesBToA int64
	// Err is nil when both sides finished cleanly, ErrIdle when the idle
	// timeout fired, the context's error when the context ended the relay,
	// and otherwise the first I/O error either direction hit.
	Err error
}

// Pipe relays between a and b until both directions have finished, closes
// both connections, and reports what happened. It blocks, so call it from the
// goroutine that owns the connection.
//
// A direction finishes when its source reaches EOF; the relay then half-closes
// the destination so the far peer sees EOF too, while the other direction
// keeps running. Connections that cannot half-close stay open until the other
// direction finishes or the idle timeout fires.
//
// Any deadlines already set on a or b are cleared: the relay uses read
// deadlines as its own signal and never sets write deadlines.
func Pipe(ctx context.Context, a, b net.Conn, opts Options) Result {
	if opts.BufferSize <= 0 {
		opts.BufferSize = DefaultBufferSize
	}
	if opts.IdleTimeout < 0 {
		opts.IdleTimeout = 0
	}

	r := newRelayRun(a, b, opts)
	defer r.closeBoth()
	_ = a.SetDeadline(time.Time{})
	_ = b.SetDeadline(time.Time{})

	// Closing both connections is the only way to unblock reads and writes
	// in flight when the context ends.
	stopWatching := context.AfterFunc(ctx, func() {
		r.cancelled.Store(true)
		r.closeBoth()
	})
	defer stopWatching()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r.copy(&r.bytesAToB, b, a)
	}()
	go func() {
		defer wg.Done()
		r.copy(&r.bytesBToA, a, b)
	}()
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()

	if opts.IdleTimeout > 0 {
		r.watch(finished)
	}
	<-finished

	result := Result{BytesAToB: r.bytesAToB.Load(), BytesBToA: r.bytesBToA.Load(), Err: r.err}
	if result.Err == nil && r.cancelled.Load() {
		result.Err = ctx.Err()
	}
	return result
}

// relayRun is the state shared by the two copy goroutines and the watchdog of
// one Pipe call.
type relayRun struct {
	a, b  net.Conn
	opts  Options
	start time.Time

	bytesAToB atomic.Int64
	bytesBToA atomic.Int64

	// lastActivity is when a byte last moved, as nanoseconds since start so
	// the clock is monotonic.
	lastActivity atomic.Int64
	// running counts copy goroutines that have not exited, so the watchdog
	// knows how many answers a poke can get.
	running atomic.Int32
	// returned receives one token each time a copy call returns, which is how
	// a poke is acknowledged. Buffered for one token per direction.
	returned chan struct{}

	// closed is set once either connection has been closed by this relay, so
	// the errors that closing provokes in the other direction are not
	// mistaken for causes.
	closed    atomic.Bool
	cancelled atomic.Bool
	closeOnce sync.Once

	// err is the first cause of failure. Written once under errOnce; read
	// after both copy goroutines have joined.
	errOnce sync.Once
	err     error
}

func newRelayRun(a, b net.Conn, opts Options) *relayRun {
	r := &relayRun{a: a, b: b, opts: opts, start: time.Now(), returned: make(chan struct{}, 2)}
	r.running.Store(2)
	return r
}

func (r *relayRun) markActivity() {
	r.lastActivity.Store(int64(time.Since(r.start)))
}

func (r *relayRun) sinceActivity() time.Duration {
	return time.Since(r.start) - time.Duration(r.lastActivity.Load())
}

// noteReturn acknowledges a poke. Dropping the token when the buffer is full
// is fine: the watchdog drains it before every poke.
func (r *relayRun) noteReturn() {
	select {
	case r.returned <- struct{}{}:
	default:
	}
}

func (r *relayRun) fail(err error) {
	r.errOnce.Do(func() { r.err = err })
	r.closeBoth()
}

func (r *relayRun) closeBoth() {
	r.closeOnce.Do(func() {
		r.closed.Store(true)
		r.a.Close()
		r.b.Close()
	})
}

// copy moves bytes from src to dst until src reaches EOF or an error occurs,
// adding what it moved to counter and answering watchdog pokes on the way.
func (r *relayRun) copy(counter *atomic.Int64, dst, src net.Conn) {
	defer r.running.Add(-1)

	zeroCopy := canSplice(dst, src)
	var buffer []byte
	var sink io.Writer
	if !zeroCopy {
		buffer = getBuffer(r.opts.BufferSize)
		defer putBuffer(buffer)
		sink = countingWriter{dst: dst, counter: counter, run: r}
	}

	for {
		var err error
		if zeroCopy {
			copied, copyErr := io.CopyN(dst, src, spliceChunk)
			if copied > 0 {
				counter.Add(copied)
				r.markActivity()
			}
			if errors.Is(copyErr, io.EOF) {
				copyErr = nil
			}
			err = copyErr
			if err == nil && copied == spliceChunk {
				// A full chunk with more to come.
				r.noteReturn()
				continue
			}
		} else {
			_, err = io.CopyBuffer(sink, readerOnly{src}, buffer)
		}
		r.noteReturn()

		switch {
		case err == nil:
			// src reached EOF. Tell dst's peer nothing more is coming, but
			// leave the other direction running.
			closeWrite(dst)
			return
		case r.closed.Load():
			// This relay closed the connections: the other direction failed,
			// the idle timeout fired, or the context ended. The cause is
			// already recorded.
			return
		case isTimeout(err):
			// A watchdog poke: the counts it wanted are published above.
			if setErr := src.SetReadDeadline(time.Time{}); setErr != nil && !r.closed.Load() {
				r.fail(setErr)
				return
			}
		default:
			r.fail(err)
			return
		}
	}
}

// watch enforces the idle timeout until finished is closed.
func (r *relayRun) watch(finished <-chan struct{}) {
	idle := r.opts.IdleTimeout
	ticker := time.NewTicker(max(idle/2, time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case <-finished:
			return
		case <-ticker.C:
		}
		if r.sinceActivity() < idle {
			continue
		}
		if r.confirmIdle(finished) {
			r.fail(ErrIdle)
			return
		}
	}
}

// confirmIdle pokes both directions so that bytes moving inside a copy call
// are counted, waits up to one interval for them to answer, and reports
// whether the relay is still idle. A direction that cannot answer within the
// interval is stuck in a write to a peer that has stopped draining, which
// counts as idle.
func (r *relayRun) confirmIdle(finished <-chan struct{}) bool {
	for len(r.returned) > 0 {
		<-r.returned
	}
	expected := min(int(r.running.Load()), 2)

	now := time.Now()
	_ = r.a.SetReadDeadline(now)
	_ = r.b.SetReadDeadline(now)

	grace := time.NewTimer(r.opts.IdleTimeout)
	defer grace.Stop()
	for answered := 0; answered < expected; {
		select {
		case <-r.returned:
			answered++
		case <-finished:
			return false
		case <-grace.C:
			answered = expected
		}
	}
	return r.sinceActivity() >= r.opts.IdleTimeout
}

// canSplice reports whether copying dst from src can go through the kernel
// without a user-space buffer, which is only the case for two TCP connections
// on Linux.
func canSplice(dst, src net.Conn) bool {
	if !spliceSupported {
		return false
	}
	_, dstTCP := dst.(*net.TCPConn)
	_, srcTCP := src.(*net.TCPConn)
	return dstTCP && srcTCP
}

// closeWrite half-closes conn when it can, so the peer reads EOF while the
// other direction keeps flowing. A failure here is not worth acting on: the
// connection is closed fully when the relay ends.
func closeWrite(conn net.Conn) {
	if halfCloser, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = halfCloser.CloseWrite()
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// countingWriter publishes every write as it happens, which is what lets the
// buffered path keep the activity clock exact.
type countingWriter struct {
	dst     io.Writer
	counter *atomic.Int64
	run     *relayRun
}

func (w countingWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if n > 0 {
		w.counter.Add(int64(n))
		w.run.markActivity()
	}
	return n, err
}

// readerOnly hides WriteTo, so io.CopyBuffer copies through the pooled buffer
// instead of letting a *net.TCPConn allocate its own 32KiB for a peer it
// cannot splice with. countingWriter hides ReadFrom the same way.
type readerOnly struct{ io.Reader }
