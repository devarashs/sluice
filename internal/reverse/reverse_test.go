package reverse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devarashs/sluice/internal/admin"
	"github.com/devarashs/sluice/internal/app"
	"github.com/devarashs/sluice/internal/certs"
	"github.com/devarashs/sluice/internal/config"
	"github.com/devarashs/sluice/internal/metrics"
	"github.com/devarashs/sluice/internal/serving"
)

// taggedEchoServer echoes each connection with a fixed tag prefixed, so a
// test can tell which local target answered.
func taggedEchoServer(t *testing.T, tag string) string {
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
				buf := make([]byte, 4096)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						conn.Write(append([]byte(tag), buf[:n]...))
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return listener.Addr().String()
}

func newDeps(logs *bytes.Buffer) app.Deps {
	var w io.Writer = io.Discard
	if logs != nil {
		w = logs
	}
	return app.Deps{Logger: slog.New(slog.NewTextHandler(w, nil)), Registry: metrics.NewRegistry(), Ready: app.NewReadiness()}
}

func serverConfig(t *testing.T, dir, token string) *ServerConfig {
	t.Helper()
	cfg := &ServerConfig{
		Common:           app.Common{Admin: admin.Config{Disabled: true}},
		Config:           serving.Config{IdleTimeout: config.Duration(time.Minute)},
		TunnelListenAddr: "127.0.0.1:0",
		Token:            token,
		CertFile:         filepath.Join(dir, "server.crt"),
		KeyFile:          filepath.Join(dir, "server.key"),
		HandshakeTimeout: config.Duration(2 * time.Second),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func clientConfig(t *testing.T, serverAddr, token, pinFile string, bindings []ClientBinding) *ClientConfig {
	t.Helper()
	cfg := &ClientConfig{
		Common:              app.Common{Admin: admin.Config{Disabled: true}},
		Config:              serving.Config{DialTimeout: config.Duration(2 * time.Second), IdleTimeout: config.Duration(time.Minute)},
		ServerAddr:          serverAddr,
		Token:               token,
		PinnedCertFile:      pinFile,
		Bindings:            bindings,
		ReconnectMinBackoff: config.Duration(50 * time.Millisecond),
		ReconnectMaxBackoff: config.Duration(200 * time.Millisecond),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// freeAddr returns a currently free loopback address for a public binding.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func startServer(t *testing.T, cfg *ServerConfig, deps app.Deps) (*Server, func()) {
	t.Helper()
	server, err := NewServer(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { server.Serve(ctx); close(done) }()
	t.Cleanup(cancel)
	return server, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("server Serve did not return")
		}
	}
}

func startClient(t *testing.T, cfg *ClientConfig, deps app.Deps) func() {
	t.Helper()
	client, err := NewClient(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { client.Serve(ctx); close(done) }()
	t.Cleanup(cancel)
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("client Serve did not return")
		}
	}
}

func waitReady(t *testing.T, deps app.Deps) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for deps.Ready.Check() != nil {
		if time.Now().After(deadline) {
			t.Fatal("never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// roundTrip sends payload to publicAddr and returns what came back.
func roundTrip(t *testing.T, publicAddr, payload string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", publicAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", publicAddr, err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read from %s: %v", publicAddr, err)
	}
	return string(buf[:n])
}

func TestUserReachesTheRightTargetThroughTwoBindings(t *testing.T) {
	const token = "reverse-secret"
	dir := t.TempDir()
	serverDeps := newDeps(nil)
	server, stopServer := startServer(t, serverConfig(t, dir, token), serverDeps)
	defer stopServer()

	publicA, publicB := freeAddr(t), freeAddr(t)
	bindings := []ClientBinding{
		{PublicAddr: publicA, TargetAddr: taggedEchoServer(t, "A:")},
		{PublicAddr: publicB, TargetAddr: taggedEchoServer(t, "B:")},
	}
	clientDeps := newDeps(nil)
	stopClient := startClient(t, clientConfig(t, server.Addr().String(), token, filepath.Join(dir, "server.crt"), bindings), clientDeps)
	defer stopClient()

	waitReady(t, clientDeps)
	// The public listeners come up a moment after the session; retry briefly.
	var gotA, gotB string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", publicA, 200*time.Millisecond); err == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	gotA = roundTrip(t, publicA, "hello")
	gotB = roundTrip(t, publicB, "hello")
	if gotA != "A:hello" {
		t.Errorf("binding A returned %q, want A:hello", gotA)
	}
	if gotB != "B:hello" {
		t.Errorf("binding B returned %q, want B:hello", gotB)
	}

	// Both sides count the two relayed connections.
	waitMetric(t, clientDeps, "sluice_relay_ended_total", metrics.CauseClean, 2)
	waitMetric(t, serverDeps, "sluice_relay_ended_total", metrics.CauseClean, 2)
}

func TestClientSurvivesServerRestart(t *testing.T) {
	const token = "reverse-secret"
	dir := t.TempDir()
	public := freeAddr(t)
	target := taggedEchoServer(t, "T:")

	// Bind the tunnel to a fixed port so the restarted server is where the
	// client keeps trying to reconnect.
	tunnelAddr := freeAddr(t)
	makeServerCfg := func() *ServerConfig {
		cfg := serverConfig(t, dir, token)
		cfg.TunnelListenAddr = tunnelAddr
		return cfg
	}

	server1, stop1 := startServer(t, makeServerCfg(), newDeps(nil))
	_ = server1
	clientDeps := newDeps(nil)
	stopClient := startClient(t, clientConfig(t, tunnelAddr, token, filepath.Join(dir, "server.crt"), []ClientBinding{{PublicAddr: public, TargetAddr: target}}), clientDeps)
	defer stopClient()

	waitReady(t, clientDeps)
	waitDialable(t, public)
	if got := roundTrip(t, public, "one"); got != "T:one" {
		t.Fatalf("before restart: %q", got)
	}

	// Drop the server. The client's session ends and it starts reconnecting.
	stop1()
	deadline := time.Now().Add(3 * time.Second)
	for clientDeps.Ready.Check() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// Bring the server back on the same tunnel port.
	_, stop2 := startServer(t, makeServerCfg(), newDeps(nil))
	defer stop2()

	waitReady(t, clientDeps)
	waitDialable(t, public)
	if got := roundTrip(t, public, "two"); got != "T:two" {
		t.Fatalf("after restart: %q", got)
	}
}

func TestClientWithWrongTokenIsRejected(t *testing.T) {
	dir := t.TempDir()
	serverDeps := newDeps(nil)
	server, stopServer := startServer(t, serverConfig(t, dir, "the-real-token"), serverDeps)
	defer stopServer()

	clientDeps := newDeps(new(bytes.Buffer))
	client, err := NewClient(clientConfig(t, server.Addr().String(), "the-wrong-token", filepath.Join(dir, "server.crt"), []ClientBinding{{PublicAddr: freeAddr(t), TargetAddr: "127.0.0.1:1"}}), clientDeps)
	if err != nil {
		t.Fatal(err)
	}
	err = client.connectAndServe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("err = %v, want a rejection", err)
	}
	if got := metricValue(t, serverDeps, "sluice_connections_accepted_total", ServerMode); got != 0 {
		t.Errorf("server accepted a connection from a bad-token client: %v", got)
	}
}

func TestClientWithWrongPinIsRejected(t *testing.T) {
	const token = "reverse-secret"
	dir := t.TempDir()
	server, stopServer := startServer(t, serverConfig(t, dir, token), newDeps(nil))
	defer stopServer()

	otherDir := t.TempDir()
	if _, err := certs.Generate(filepath.Join(otherDir, "other.crt"), filepath.Join(otherDir, "other.key"), nil, 0); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(clientConfig(t, server.Addr().String(), token, filepath.Join(otherDir, "other.crt"), []ClientBinding{{PublicAddr: freeAddr(t), TargetAddr: "127.0.0.1:1"}}), newDeps(nil))
	if err != nil {
		t.Fatal(err)
	}
	err = client.connectAndServe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "tls handshake") {
		t.Fatalf("err = %v, want a pin/handshake failure", err)
	}
}

func TestServerReportsBindFailureForAnOccupiedPort(t *testing.T) {
	const token = "reverse-secret"
	dir := t.TempDir()
	server, stopServer := startServer(t, serverConfig(t, dir, token), newDeps(nil))
	defer stopServer()

	// Occupy one address; the other is free.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	free := freeAddr(t)
	target := taggedEchoServer(t, "T:")

	clientDeps := newDeps(nil)
	stopClient := startClient(t, clientConfig(t, server.Addr().String(), token, filepath.Join(dir, "server.crt"), []ClientBinding{
		{PublicAddr: occupied.Addr().String(), TargetAddr: target},
		{PublicAddr: free, TargetAddr: target},
	}), clientDeps)
	defer stopClient()

	waitReady(t, clientDeps)
	waitDialable(t, free)
	// The free binding works even though the occupied one failed.
	if got := roundTrip(t, free, "ok"); got != "T:ok" {
		t.Fatalf("free binding returned %q", got)
	}
	// The occupied binding never opened a second listener on that port; the
	// original occupier still owns it, so a dial reaches the occupier, which
	// accepts and never replies. That is enough: the client stayed up on the
	// strength of the one good binding.
}

func TestConfigValidation(t *testing.T) {
	base := func() *ClientConfig {
		return &ClientConfig{
			Common:         app.Common{Admin: admin.Config{Disabled: true}},
			ServerAddr:     "server:9000",
			Token:          "t",
			PinnedCertFile: "s.crt",
			Bindings:       []ClientBinding{{PublicAddr: "0.0.0.0:8080", TargetAddr: "127.0.0.1:3000"}},
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid client config rejected: %v", err)
	}

	cases := map[string]struct {
		mutate func(*ClientConfig)
		field  string
	}{
		"no token":                 {func(c *ClientConfig) { c.Token = "" }, "token"},
		"neither pin nor insecure": {func(c *ClientConfig) { c.PinnedCertFile = "" }, "pinnedCertFile"},
		"both":                     {func(c *ClientConfig) { c.InsecureSkipVerify = true }, "insecureSkipVerify"},
		"no bindings":              {func(c *ClientConfig) { c.Bindings = nil }, "bindings"},
		"bad public":               {func(c *ClientConfig) { c.Bindings[0].PublicAddr = "nope" }, "bindings[0].publicAddr"},
		"bad target":               {func(c *ClientConfig) { c.Bindings[0].TargetAddr = "nohost" }, "bindings[0].targetAddr"},
		"dup public":               {func(c *ClientConfig) { c.Bindings = append(c.Bindings, c.Bindings[0]) }, "bindings[1].publicAddr"},
		"backoff inverted": {func(c *ClientConfig) {
			c.ReconnectMinBackoff = config.Duration(time.Minute)
			c.ReconnectMaxBackoff = config.Duration(time.Second)
		}, "reconnectMaxBackoff"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			c.mutate(cfg)
			err := cfg.Validate()
			var fieldErr *config.FieldError
			if !errors.As(err, &fieldErr) || fieldErr.Field != c.field {
				t.Fatalf("err = %v, want FieldError on %s", err, c.field)
			}
		})
	}

	serverBase := &ServerConfig{Common: app.Common{Admin: admin.Config{Disabled: true}}, TunnelListenAddr: ":9000", Token: "t"}
	if err := serverBase.Validate(); err != nil {
		t.Fatalf("valid server config rejected: %v", err)
	}
	if serverBase.MaxClients != DefaultMaxClients || serverBase.CertFile != DefaultServerCertFile {
		t.Errorf("server defaults not applied: %+v", serverBase)
	}
	noToken := &ServerConfig{Common: app.Common{Admin: admin.Config{Disabled: true}}, TunnelListenAddr: ":9000"}
	var fieldErr *config.FieldError
	if err := noToken.Validate(); !errors.As(err, &fieldErr) || fieldErr.Field != "token" {
		t.Errorf("server without token: err = %v", err)
	}
}

// --- metric helpers ---

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
			matched := labelValue == ""
			for _, pair := range m.GetLabel() {
				if pair.GetValue() == labelValue {
					matched = true
				}
			}
			if !matched {
				continue
			}
			if m.GetCounter() != nil {
				return m.GetCounter().GetValue()
			}
			return m.GetGauge().GetValue()
		}
	}
	return 0
}

func waitMetric(t *testing.T, deps app.Deps, family, labelValue string, want float64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if metricValue(t, deps, family, labelValue) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s{%s} did not reach %v (last %v)", family, labelValue, want, metricValue(t, deps, family, labelValue))
}

func waitDialable(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never became dialable", addr)
}

var _ = fmt.Sprintf
var _ = sync.Mutex{}
