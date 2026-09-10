package listen

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devarashs/sluice/internal/config"
	"github.com/devarashs/sluice/internal/limits"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func validated(t *testing.T, cfg Config) Config {
	t.Helper()
	if err := cfg.Validate("listener"); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestValidateDefaultsAndBounds(t *testing.T) {
	cfg := validated(t, Config{})
	if cfg.Acceptors < 1 || cfg.Acceptors > MaxAcceptors {
		t.Errorf("acceptors = %d", cfg.Acceptors)
	}
	if cfg.KeepAliveIdle.Duration() != DefaultKeepAliveIdle || cfg.KeepAliveInterval.Duration() != DefaultKeepAliveInterval || cfg.KeepAliveCount != DefaultKeepAliveCount {
		t.Errorf("keepalive defaults = %+v", cfg)
	}
	if cfg.DrainTimeout.Duration() != DefaultDrainTimeout {
		t.Errorf("drainTimeout = %v", cfg.DrainTimeout)
	}

	for name, bad := range map[string]Config{
		"negative acceptors": {Acceptors: -1},
		"too many acceptors": {Acceptors: MaxAcceptors + 1},
		"negative count":     {KeepAliveCount: -1},
	} {
		t.Run(name, func(t *testing.T) {
			var fieldErr *config.FieldError
			if err := bad.Validate("listener"); !errors.As(err, &fieldErr) {
				t.Fatalf("err = %v, want a FieldError", err)
			}
		})
	}
}

// echoHandler copies input back to the client until EOF.
func echoHandler(_ context.Context, conn net.Conn) {
	defer conn.Close()
	io.Copy(conn, conn)
}

// serve starts a listener with the given guards and handler and returns its
// address plus a stop function that ends Serve and returns its error.
func serve(t *testing.T, cfg Config, guards Guards, handler Handler) (addr string, listener *Listener, stop func() error) {
	t.Helper()
	if guards.Cap == nil {
		guards.Cap = limits.NewConcurrency(0)
	}
	listener, err := Listen("127.0.0.1:0", cfg, guards, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- listener.Serve(ctx, handler) }()
	stop = func() error {
		cancel()
		select {
		case err := <-served:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("Serve did not return after cancel")
			return nil
		}
	}
	t.Cleanup(func() { cancel() })
	return listener.Addr().String(), listener, stop
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestAcceptsAndHandsConnectionsToHandler(t *testing.T) {
	addr, listener, stop := serve(t, validated(t, Config{Acceptors: 3}), Guards{}, echoHandler)

	if runtime.GOOS == "linux" {
		if listener.Sockets() != 3 {
			t.Errorf("sockets = %d on linux, want 3 with SO_REUSEPORT", listener.Sockets())
		}
	} else if listener.Sockets() != 1 {
		t.Errorf("sockets = %d off linux, want 1", listener.Sockets())
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn := dial(t, addr)
			message := []byte{byte(i), 'x', 'y'}
			if _, err := conn.Write(message); err != nil {
				t.Error(err)
				return
			}
			reply := make([]byte, len(message))
			if _, err := io.ReadFull(conn, reply); err != nil {
				t.Error(err)
				return
			}
			if string(reply) != string(message) {
				t.Errorf("echo mismatch: %q vs %q", reply, message)
			}
			conn.Close()
		}(i)
	}
	wg.Wait()

	if err := stop(); err != nil {
		t.Fatalf("Serve returned %v", err)
	}
	if stats := listener.Stats(); stats.Accepted != 50 || stats.RejectedByRate != 0 {
		t.Fatalf("stats = %+v, want 50 accepted", stats)
	}
}

func TestConcurrencyCapHoldsExcessInBacklog(t *testing.T) {
	var started atomic.Int32
	release := make(chan struct{})
	holder := func(ctx context.Context, conn net.Conn) {
		defer conn.Close()
		started.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	cap := limits.NewConcurrency(2)
	addr, listener, stop := serve(t, validated(t, Config{Acceptors: 1, DrainTimeout: config.Duration(time.Second)}), Guards{Cap: cap}, holder)

	first, second, third := dial(t, addr), dial(t, addr), dial(t, addr)
	_, _, _ = first, second, third
	time.Sleep(100 * time.Millisecond)
	if got := started.Load(); got != 2 {
		t.Fatalf("handlers started = %d, want 2 at a cap of 2", got)
	}
	if listener.Stats().Active != 2 {
		t.Fatalf("active = %d, want 2", listener.Stats().Active)
	}

	// Freeing one slot lets the queued third connection through.
	release <- struct{}{}
	deadline := time.Now().Add(2 * time.Second)
	for started.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if started.Load() != 3 {
		t.Fatalf("third connection was not handled after a slot freed (started %d)", started.Load())
	}
	close(release)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestRateLimitedClientIsClosedWithoutAHandler(t *testing.T) {
	var handled atomic.Int32
	counting := func(_ context.Context, conn net.Conn) {
		handled.Add(1)
		echoHandler(context.Background(), conn)
	}
	perClientCfg := limits.PerClientConfig{ConnectionsPerSecond: 0.001, Burst: 2, MaxTrackedClients: 10}
	if err := perClientCfg.Validate("limits.perClient"); err != nil {
		t.Fatal(err)
	}
	perClient, err := limits.NewPerClient(perClientCfg)
	if err != nil {
		t.Fatal(err)
	}
	addr, listener, stop := serve(t, validated(t, Config{Acceptors: 1}), Guards{PerClient: perClient}, counting)

	first, second := dial(t, addr), dial(t, addr)
	third := dial(t, addr)
	// The third connection from this client is over the burst: it is closed
	// by the listener before any handler runs.
	third.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := third.Read(make([]byte, 1)); err == nil {
		t.Fatal("rate-limited connection was not closed")
	}
	for _, conn := range []net.Conn{first, second} {
		if _, err := conn.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		reply := make([]byte, 4)
		if _, err := io.ReadFull(conn, reply); err != nil {
			t.Fatalf("allowed connection failed: %v", err)
		}
	}
	first.Close()
	second.Close()

	if err := stop(); err != nil {
		t.Fatal(err)
	}
	stats := listener.Stats()
	if stats.Accepted != 2 || stats.RejectedByRate != 1 || handled.Load() != 2 {
		t.Fatalf("stats = %+v, handled = %d; want 2 accepted, 1 rejected, 2 handled", stats, handled.Load())
	}
}

func TestShutdownStopsAcceptingAndDrainsThenCancels(t *testing.T) {
	var sawCancel atomic.Bool
	blocking := func(ctx context.Context, conn net.Conn) {
		defer conn.Close()
		<-ctx.Done()
		sawCancel.Store(true)
	}
	addr, _, stop := serve(t, validated(t, Config{Acceptors: 1, DrainTimeout: config.Duration(200 * time.Millisecond)}), Guards{}, blocking)
	inFlight := dial(t, addr)
	time.Sleep(50 * time.Millisecond)

	started := time.Now()
	if err := stop(); err != nil {
		t.Fatalf("Serve returned %v", err)
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond {
		t.Fatalf("Serve returned after %v, before the drain timeout", elapsed)
	}
	if !sawCancel.Load() {
		t.Fatal("handler context was not cancelled at the end of the drain")
	}
	if _, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		t.Fatal("listener still accepting after shutdown")
	}
	inFlight.Close()
}

func TestShutdownReturnsAsSoonAsHandlersFinish(t *testing.T) {
	addr, _, stop := serve(t, validated(t, Config{Acceptors: 1, DrainTimeout: config.Duration(10 * time.Second)}), Guards{}, echoHandler)
	conn := dial(t, addr)
	conn.Close()
	time.Sleep(20 * time.Millisecond)
	started := time.Now()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Serve took %v to return with no handlers in flight", elapsed)
	}
}

func TestListenRequiresValidatedConfigAndACapAndAFreePort(t *testing.T) {
	if l, err := Listen("127.0.0.1:0", Config{}, Guards{Cap: limits.NewConcurrency(0)}, quietLogger()); err == nil {
		l.Close()
		t.Error("unvalidated config should be refused")
	}
	if l, err := Listen("127.0.0.1:0", validated(t, Config{}), Guards{}, quietLogger()); err == nil {
		l.Close()
		t.Error("nil cap should be refused")
	}
	// A port held by a socket without SO_REUSEPORT cannot be joined on any
	// platform, so this fails on Linux as well as elsewhere.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	if l, err := Listen(occupied.Addr().String(), validated(t, Config{}), Guards{Cap: limits.NewConcurrency(0)}, quietLogger()); err == nil {
		l.Close()
		t.Error("binding an occupied port should fail")
	}
}

func TestAllSocketsShareThePortChosenForZero(t *testing.T) {
	listener, err := Listen("127.0.0.1:0", validated(t, Config{Acceptors: 4}), Guards{Cap: limits.NewConcurrency(0)}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	for _, socket := range listener.sockets {
		if socket.Addr().(*net.TCPAddr).Port != port {
			t.Fatalf("socket bound to %v, want port %d", socket.Addr(), port)
		}
	}
}

func TestNextBackoffDoublesToTheCap(t *testing.T) {
	var d time.Duration
	seen := []time.Duration{}
	for i := 0; i < 12; i++ {
		d = nextBackoff(d)
		seen = append(seen, d)
	}
	if seen[0] != minAcceptBackoff || seen[1] != 2*minAcceptBackoff || seen[len(seen)-1] != maxAcceptBackoff {
		t.Fatalf("backoff sequence %v", seen)
	}
}
