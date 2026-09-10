package forward

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/devarashs/sluice/internal/admin"
	"github.com/devarashs/sluice/internal/app"
	"github.com/devarashs/sluice/internal/config"
	"github.com/devarashs/sluice/internal/metrics"
)

// echoServer accepts on loopback and echoes each connection until EOF.
func echoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

func validConfig(t *testing.T, target string) *Config {
	t.Helper()
	cfg := &Config{
		Common:      app.Common{Admin: admin.Config{Disabled: true}},
		ListenAddr:  "127.0.0.1:0",
		TargetAddr:  target,
		DialTimeout: config.Duration(2 * time.Second),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func testDeps() app.Deps {
	return app.Deps{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Registry: metrics.NewRegistry(),
		Ready:    app.NewReadiness(),
	}
}

// start runs a forwarder and returns its address and a stop function.
func start(t *testing.T, cfg *Config, deps app.Deps) (addr string, stop func() error) {
	t.Helper()
	forwarder, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- forwarder.Serve(ctx) }()
	t.Cleanup(cancel)
	return forwarder.Addr().String(), func() error {
		cancel()
		select {
		case err := <-served:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("Serve did not return")
			return nil
		}
	}
}

func metricValue(t *testing.T, deps app.Deps, family, labelValue string) float64 {
	t.Helper()
	families, err := deps.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != family {
			continue
		}
		for _, m := range f.GetMetric() {
			matches := labelValue == ""
			for _, pair := range m.GetLabel() {
				if pair.GetValue() == labelValue {
					matches = true
				}
			}
			if !matches {
				continue
			}
			if m.GetCounter() != nil {
				return m.GetCounter().GetValue()
			}
			return m.GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %s{%s} not found", family, labelValue)
	return 0
}

func TestForwardsAndReportsBytes(t *testing.T) {
	deps := testDeps()
	cfg := validConfig(t, echoServer(t))
	addr, stop := start(t, cfg, deps)

	// Readiness flips on the serving goroutine once accepting begins.
	readyBy := time.Now().Add(2 * time.Second)
	for deps.Ready.Check() != nil && time.Now().Before(readyBy) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := deps.Ready.Check(); err != nil {
		t.Fatalf("forwarder never became ready: %v", err)
	}

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("sluice ", 1000)
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatal(err)
	}
	if string(reply) != payload {
		t.Fatal("echo through the forwarder did not match")
	}
	conn.Close()

	// The relay finishes a moment after the client closes.
	deadline := time.Now().Add(2 * time.Second)
	for metricValue(t, deps, "sluice_relay_ended_total", metrics.CauseClean) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := metricValue(t, deps, "sluice_relay_bytes_total", metrics.DirectionUpstream); got != float64(len(payload)) {
		t.Errorf("upstream bytes = %v, want %d", got, len(payload))
	}
	if got := metricValue(t, deps, "sluice_relay_bytes_total", metrics.DirectionDownstream); got != float64(len(payload)) {
		t.Errorf("downstream bytes = %v, want %d", got, len(payload))
	}
	if got := metricValue(t, deps, "sluice_connections_accepted_total", Mode); got != 1 {
		t.Errorf("accepted = %v, want 1", got)
	}

	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if deps.Ready.Check() == nil {
		t.Fatal("forwarder should not be ready after stopping")
	}
}

func TestUnreachableTargetClosesClientAndCounts(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := closed.Addr().String()
	closed.Close()

	deps := testDeps()
	addr, stop := start(t, validConfig(t, unreachable), deps)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("client should be closed when the target is unreachable")
	}
	if got := metricValue(t, deps, "sluice_dial_failures_total", Mode); got != 1 {
		t.Errorf("dial failures = %v, want 1", got)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrencyCapIsApplied(t *testing.T) {
	deps := testDeps()
	cfg := validConfig(t, echoServer(t))
	cfg.Limits.MaxConnections = 1
	addr, stop := start(t, cfg, deps)

	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(first, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}

	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	second.Write([]byte("b"))
	second.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatal("second connection was served while the only slot was held")
	}

	first.Close()
	second.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := second.Read(make([]byte, 1)); err != nil {
		t.Fatalf("second connection not served after the slot freed: %v", err)
	}
	second.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateDefaultsAndErrors(t *testing.T) {
	cfg := &Config{Common: app.Common{Admin: admin.Config{Disabled: true}}, ListenAddr: ":0", TargetAddr: "10.0.0.1:80"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.DialTimeout.Duration() != DefaultDialTimeout || cfg.IdleTimeout.Duration() != DefaultIdleTimeout {
		t.Errorf("timeouts = %v / %v", cfg.DialTimeout, cfg.IdleTimeout)
	}
	if cfg.BufferSize != 16*config.KiB {
		t.Errorf("bufferSize = %v", cfg.BufferSize)
	}
	if cfg.Listener.Acceptors == 0 || cfg.Log.Level != "info" {
		t.Errorf("nested defaults not applied: %+v", cfg)
	}
	if cfg.relayOptions().IdleTimeout != DefaultIdleTimeout {
		t.Errorf("relay idle = %v", cfg.relayOptions().IdleTimeout)
	}

	cfg.NoIdleTimeout = true
	if cfg.relayOptions().IdleTimeout != 0 {
		t.Error("noIdleTimeout should disable the relay idle timeout")
	}

	cases := map[string]struct {
		mutate func(*Config)
		field  string
	}{
		"missing listen":    {func(c *Config) { c.ListenAddr = "" }, "listenAddr"},
		"bad target":        {func(c *Config) { c.TargetAddr = "nohost" }, "targetAddr"},
		"idle too short":    {func(c *Config) { c.IdleTimeout = config.Duration(100 * time.Millisecond) }, "idleTimeout"},
		"buffer too small":  {func(c *Config) { c.BufferSize = 100 }, "bufferSize"},
		"nested limits":     {func(c *Config) { c.Limits.MaxConnections = -5 }, "limits.maxConnections"},
		"nested admin":      {func(c *Config) { c.Admin.Disabled = false; c.Admin.ListenAddr = "bad" }, "admin.listenAddr"},
		"nested log":        {func(c *Config) { c.Log.Level = "loud" }, "log.level"},
		"nested listener":   {func(c *Config) { c.Listener.Acceptors = -1 }, "listener.acceptors"},
		"nested per-client": {func(c *Config) { c.Limits.PerClient.ConnectionsPerSecond = -1 }, "limits.perClient.connectionsPerSecond"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := &Config{Common: app.Common{Admin: admin.Config{Disabled: true}}, ListenAddr: ":0", TargetAddr: "10.0.0.1:80"}
			c.mutate(cfg)
			err := cfg.Validate()
			var fieldErr *config.FieldError
			if !errors.As(err, &fieldErr) || fieldErr.Field != c.field {
				t.Fatalf("err = %v, want FieldError on %s", err, c.field)
			}
		})
	}
}

func TestConfigLoadsFromJSONWithSharedBlocks(t *testing.T) {
	raw := []byte(`{
		"listenAddr": "0.0.0.0:8080",
		"targetAddr": "10.0.0.5:80",
		"idleTimeout": "5m",
		"limits": {"maxConnections": 50000, "perClient": {"connectionsPerSecond": 20}},
		"listener": {"acceptors": 2, "drainTimeout": "5s"},
		"admin": {"listenAddr": "127.0.0.1:9800"},
		"log": {"level": "debug", "format": "json"},
		"process": {"memoryLimit": "512MiB"}
	}`)
	cfg, err := config.Decode(raw, (*Config).Validate)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.PerClient.Burst != 20 || cfg.Listener.DrainTimeout.Duration() != 5*time.Second || cfg.Process.MemoryLimit != 512*config.MiB {
		t.Fatalf("decoded = %+v", cfg)
	}
	if _, err := config.Decode([]byte(`{"listenAddr": ":1", "targetAddr": "h:1", "unknownBlock": {}}`), (*Config).Validate); err == nil {
		t.Fatal("unknown top-level block must be refused")
	}
}
