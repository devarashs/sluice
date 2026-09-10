// Command sluice-capacity measures how much memory one idle forwarded
// connection costs on the host it runs on, so an operator can turn a memory
// budget into a connection ceiling.
//
// It stands up a forward in-process to a local accept-and-hold server, opens
// -conns connections through it, waits for them to settle, and reports the
// heap growth per connection. It is a development and sizing tool; it is not
// built into the sluice binary or shipped in releases.
//
// Usage:
//
//	go run ./cmd/sluice-capacity -conns 20000
//
// The per-connection figure is meaningful only on the target OS: the relay's
// splice fast path on Linux holds no user-space buffer, while other platforms
// keep a pooled buffer per active copy. Run it on the box you will deploy to.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/devarashs/sluice/internal/admin"
	"github.com/devarashs/sluice/internal/app"
	"github.com/devarashs/sluice/internal/config"
	"github.com/devarashs/sluice/internal/forward"
	"github.com/devarashs/sluice/internal/metrics"
	"github.com/devarashs/sluice/internal/version"
)

func main() {
	conns := flag.Int("conns", 10000, "number of concurrent idle connections to open")
	settle := flag.Duration("settle", 3*time.Second, "time to let connections settle before measuring")
	flag.Parse()

	if err := run(*conns, *settle); err != nil {
		fmt.Fprintln(os.Stderr, "sluice-capacity:", err)
		os.Exit(1)
	}
}

func run(conns int, settle time.Duration) error {
	fmt.Println(version.String())

	target := startHoldServer()
	forwardAddr, err := startForward(target)
	if err != nil {
		return err
	}

	fmt.Printf("opening %d connections through the forwarder...\n", conns)
	baseline := heapInUse()

	held := make([]net.Conn, 0, conns)
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for i := 0; i < conns; i++ {
		c, err := net.Dial("tcp", forwardAddr)
		if err != nil {
			return fmt.Errorf("after %d connections: %w (raise ulimit -n, or lower -conns)", i, err)
		}
		// One byte each way so the relay is fully established in both
		// directions, then leave the connection idle and open.
		if _, err := c.Write([]byte{'x'}); err != nil {
			return fmt.Errorf("write on connection %d: %w", i, err)
		}
		held = append(held, c)
	}

	fmt.Printf("settling for %v...\n", settle)
	time.Sleep(settle)

	runtime.GC()
	used := heapInUse()
	growth := used - baseline
	perConn := float64(growth) / float64(conns)

	fmt.Println("---")
	fmt.Printf("connections held:      %d\n", conns)
	fmt.Printf("heap in use baseline:  %s\n", config.ByteSize(baseline))
	fmt.Printf("heap in use now:       %s\n", config.ByteSize(used))
	fmt.Printf("heap growth:           %s\n", config.ByteSize(growth))
	fmt.Printf("per connection (heap): ~%.0f bytes\n", perConn)
	fmt.Println("---")
	fmt.Printf("A host with a heap budget of B bytes for the relay can hold roughly B / %.0f connections.\n", perConn)
	fmt.Println("Note: this counts the forwarder's own heap, both ends of every")
	fmt.Println("connection running in this one process. On a real deployment the")
	fmt.Println("two ends are separate hosts, so per-host cost is lower. Kernel")
	fmt.Println("socket memory is separate again; see docs/tuning.md.")
	return nil
}

// startHoldServer accepts connections and holds them open without reading, so
// the far side of every forwarded connection stays established and idle.
func startHoldServer() string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	go func() {
		var held []net.Conn
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			held = append(held, conn) // keep a reference so it is not GC'd/closed
		}
	}()
	return listener.Addr().String()
}

func startForward(target string) (string, error) {
	cfg := &forward.Config{
		Common:     app.Common{Admin: admin.Config{Disabled: true}},
		ListenAddr: "127.0.0.1:0",
		TargetAddr: target,
	}
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	deps := app.Deps{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Registry: metrics.NewRegistry(),
		Ready:    app.NewReadiness(),
	}
	f, err := forward.New(cfg, deps)
	if err != nil {
		return "", err
	}
	go f.Serve(context.Background())
	for deps.Ready.Check() != nil {
		time.Sleep(time.Millisecond)
	}
	return f.Addr().String(), nil
}

func heapInUse() int64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapInuse)
}
