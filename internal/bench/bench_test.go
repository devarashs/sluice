// Package bench holds the throughput and setup-rate benchmarks for the relay
// and the forward mode. They are Go benchmarks, so a plain `go test` skips
// them and only `go test -bench` runs them; that keeps the merge gate fast
// while giving operators a repeatable way to measure on their own hardware.
//
// The numbers depend heavily on the host: the splice fast path only exists on
// Linux, and loopback throughput on a laptop bears little resemblance to a
// tuned server. Treat these as a method to run where it matters, not as
// published figures. See docs/capacity.md.
package bench

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/devarashs/sluice/internal/admin"
	"github.com/devarashs/sluice/internal/app"
	"github.com/devarashs/sluice/internal/config"
	"github.com/devarashs/sluice/internal/forward"
	"github.com/devarashs/sluice/internal/metrics"
	"github.com/devarashs/sluice/internal/relay"
	"github.com/devarashs/sluice/internal/serving"
)

// discardServer accepts and drains every connection, so a throughput test is
// not bounded by an echo turnaround.
func discardServer(b *testing.B) string {
	b.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				io.Copy(io.Discard, conn)
				conn.Close()
			}()
		}
	}()
	return listener.Addr().String()
}

// BenchmarkRelayThroughput measures how fast relay.Pipe moves bytes between
// two real TCP connections, which is the splice fast path on Linux and the
// pooled-buffer path elsewhere.
func BenchmarkRelayThroughput(b *testing.B) {
	target := discardServer(b)

	// left <-> a  ==Pipe==  b <-> right(discard). The benchmark writes into
	// left; the bytes cross the relay to the discard server.
	left, a := tcpConnPair(b)
	bConn, err := net.Dial("tcp", target)
	if err != nil {
		skipOrFail(b, err)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go relay.Pipe(ctx, a, bConn, relay.Options{})

	const chunk = 128 * 1024
	buf := make([]byte, chunk)
	b.SetBytes(chunk)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := left.Write(buf); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	left.Close()
}

// tcpConnPair returns both ends of one loopback TCP connection.
func tcpConnPair(b *testing.B) (net.Conn, net.Conn) {
	b.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, _ := listener.Accept()
		accepted <- conn
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		skipOrFail(b, err)
	}
	server := <-accepted
	b.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

// startForward runs a forward to target on a free port and returns its
// address, for the setup-rate benchmark.
func startForward(b *testing.B, target string) string {
	b.Helper()
	cfg := &forward.Config{
		Common:     app.Common{Admin: admin.Config{Disabled: true}},
		Config:     serving.Config{DialTimeout: config.Duration(2 * time.Second)},
		ListenAddr: "127.0.0.1:0",
		TargetAddr: target,
	}
	if err := cfg.Validate(); err != nil {
		b.Fatal(err)
	}
	deps := app.Deps{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Registry: metrics.NewRegistry(),
		Ready:    app.NewReadiness(),
	}
	f, err := forward.New(cfg, deps)
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go f.Serve(ctx)
	b.Cleanup(cancel)
	for deps.Ready.Check() != nil {
		time.Sleep(time.Millisecond)
	}
	return f.Addr().String()
}

// BenchmarkForwardConnectionSetup measures full connection lifecycles per
// second through the forward: dial, one small round trip, close.
func BenchmarkForwardConnectionSetup(b *testing.B) {
	addr := startForward(b, discardServer(b))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			skipOrFail(b, err)
			return
		}
		if _, err := conn.Write([]byte("x")); err != nil {
			conn.Close()
			skipOrFail(b, err)
			return
		}
		conn.Close()
	}
}

// skipOrFail turns a loopback resource ceiling — exhausted ephemeral ports or
// open files under rapid connection churn — into a skip with guidance, since
// that is the host's limit rather than a defect. Anything else is a real
// failure.
func skipOrFail(tb testing.TB, err error) {
	msg := err.Error()
	for _, marker := range []string{
		"Only one usage of each socket address", // Windows: ephemeral ports exhausted
		"cannot assign requested address",       // Linux: ephemeral ports exhausted
		"too many open files",                   // descriptor limit
	} {
		if strings.Contains(msg, marker) {
			tb.Skipf("host resource ceiling reached (%v); tune ephemeral ports and ulimit per docs/tuning.md to benchmark setup rate", err)
		}
	}
	tb.Fatal(err)
}

// BenchmarkForwardConnectionSetupParallel is the same across many goroutines,
// which is closer to how connections actually arrive.
func BenchmarkForwardConnectionSetupParallel(b *testing.B) {
	addr := startForward(b, discardServer(b))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				skipOrFail(b, err)
				return
			}
			conn.Write([]byte("x"))
			conn.Close()
		}
	})
}
