package tlsrelay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devarashs/sluice/internal/admin"
	"github.com/devarashs/sluice/internal/app"
	"github.com/devarashs/sluice/internal/certs"
	"github.com/devarashs/sluice/internal/config"
	"github.com/devarashs/sluice/internal/metrics"
	"github.com/devarashs/sluice/internal/serving"
)

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

func newDeps(logs *bytes.Buffer) app.Deps {
	var handler slog.Handler
	if logs != nil {
		handler = slog.NewTextHandler(logs, nil)
	} else {
		handler = slog.NewTextHandler(io.Discard, nil)
	}
	return app.Deps{Logger: slog.New(handler), Registry: metrics.NewRegistry(), Ready: app.NewReadiness()}
}

type server interface {
	Addr() net.Addr
	Serve(context.Context) error
}

func run(t *testing.T, s server) (addr string, stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx) }()
	t.Cleanup(cancel)
	return s.Addr().String(), func() error {
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
			matches := false
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
	return 0
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !condition() {
		t.Fatalf("timed out waiting for %s", what)
	}
}

func receiverConfig(t *testing.T, dir, target string) *ReceiverConfig {
	t.Helper()
	cfg := &ReceiverConfig{
		Common:           app.Common{Admin: admin.Config{Disabled: true}},
		Config:           serving.Config{DialTimeout: config.Duration(2 * time.Second)},
		ListenAddr:       "127.0.0.1:0",
		TargetAddr:       target,
		CertFile:         filepath.Join(dir, "receiver.crt"),
		KeyFile:          filepath.Join(dir, "receiver.key"),
		HandshakeTimeout: config.Duration(500 * time.Millisecond),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func entryConfig(t *testing.T, receiverAddr, pinFile string, insecure bool) *EntryConfig {
	t.Helper()
	cfg := &EntryConfig{
		Common:             app.Common{Admin: admin.Config{Disabled: true}},
		Config:             serving.Config{DialTimeout: config.Duration(2 * time.Second)},
		ListenAddr:         "127.0.0.1:0",
		ReceiverAddr:       receiverAddr,
		PinnedCertFile:     pinFile,
		InsecureSkipVerify: insecure,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// chain brings up echo <- receiver <- entry with the entry pinned to the
// receiver's generated certificate, and returns the entry address.
func chain(t *testing.T) (entryAddr string, entryDeps, receiverDeps app.Deps, receiverLogs *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	receiverLogs = new(bytes.Buffer)
	receiverDeps = newDeps(receiverLogs)
	receiver, err := NewReceiver(receiverConfig(t, dir, echoServer(t)), receiverDeps)
	if err != nil {
		t.Fatal(err)
	}
	receiverAddr, _ := run(t, receiver)

	entryDeps = newDeps(nil)
	entry, err := NewEntry(entryConfig(t, receiverAddr, filepath.Join(dir, "receiver.crt"), false), entryDeps)
	if err != nil {
		t.Fatal(err)
	}
	entryAddr, _ = run(t, entry)
	return entryAddr, entryDeps, receiverDeps, receiverLogs
}

func TestEncryptedHopRelaysBothWays(t *testing.T) {
	entryAddr, entryDeps, receiverDeps, receiverLogs := chain(t)

	conn, err := net.Dial("tcp", entryAddr)
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("through the sluice ", 5000)
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatal(err)
	}
	if string(reply) != payload {
		t.Fatal("echo through the encrypted hop did not match")
	}
	conn.Close()

	waitFor(t, "entry relay to finish", func() bool {
		return metricValue(t, entryDeps, "sluice_relay_ended_total", metrics.CauseClean) == 1
	})
	waitFor(t, "receiver relay to finish", func() bool {
		return metricValue(t, receiverDeps, "sluice_relay_ended_total", metrics.CauseClean) == 1
	})
	if got := metricValue(t, entryDeps, "sluice_relay_bytes_total", metrics.DirectionUpstream); got != float64(len(payload)) {
		t.Errorf("entry upstream bytes = %v, want %d", got, len(payload))
	}
	if got := metricValue(t, receiverDeps, "sluice_relay_bytes_total", metrics.DirectionDownstream); got != float64(len(payload)) {
		t.Errorf("receiver downstream bytes = %v, want %d", got, len(payload))
	}
	if !strings.Contains(receiverLogs.String(), "generated a new certificate") || !strings.Contains(receiverLogs.String(), "sha256:") {
		t.Errorf("receiver did not log the generated certificate and its fingerprint:\n%s", receiverLogs.String())
	}
}

func TestEntryWithWrongPinIsRefusedAndBothSidesCount(t *testing.T) {
	dir := t.TempDir()
	receiverDeps := newDeps(nil)
	receiver, err := NewReceiver(receiverConfig(t, dir, echoServer(t)), receiverDeps)
	if err != nil {
		t.Fatal(err)
	}
	receiverAddr, _ := run(t, receiver)

	otherDir := t.TempDir()
	if _, err := certs.Generate(filepath.Join(otherDir, "other.crt"), filepath.Join(otherDir, "other.key"), nil, 0); err != nil {
		t.Fatal(err)
	}
	entryDeps := newDeps(nil)
	entry, err := NewEntry(entryConfig(t, receiverAddr, filepath.Join(otherDir, "other.crt"), false), entryDeps)
	if err != nil {
		t.Fatal(err)
	}
	entryAddr, _ := run(t, entry)

	conn, err := net.Dial("tcp", entryAddr)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("client should be closed when the entry refuses the receiver's certificate")
	}
	waitFor(t, "entry dial failure", func() bool {
		return metricValue(t, entryDeps, "sluice_dial_failures_total", EntryMode) == 1
	})
	waitFor(t, "receiver handshake failure", func() bool {
		return metricValue(t, receiverDeps, "sluice_tls_handshake_failures_total", ReceiverMode) == 1
	})
}

func TestInsecureEntryConnectsAndWarns(t *testing.T) {
	dir := t.TempDir()
	receiver, err := NewReceiver(receiverConfig(t, dir, echoServer(t)), newDeps(nil))
	if err != nil {
		t.Fatal(err)
	}
	receiverAddr, _ := run(t, receiver)

	var logs bytes.Buffer
	entry, err := NewEntry(entryConfig(t, receiverAddr, "", true), newDeps(&logs))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "insecureSkipVerify is set") {
		t.Fatalf("insecure entry did not warn:\n%s", logs.String())
	}
	entryAddr, _ := run(t, entry)
	conn, err := net.Dial("tcp", entryAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil || string(reply) != "hi" {
		t.Fatalf("echo via insecure entry: %q, %v", reply, err)
	}
}

func TestReceiverDropsClientsThatNeverHandshake(t *testing.T) {
	dir := t.TempDir()
	deps := newDeps(nil)
	receiver, err := NewReceiver(receiverConfig(t, dir, echoServer(t)), deps)
	if err != nil {
		t.Fatal(err)
	}
	receiverAddr, _ := run(t, receiver)

	silent, err := net.Dial("tcp", receiverAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	started := time.Now()
	silent.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := silent.Read(make([]byte, 1)); err == nil {
		t.Fatal("silent client was not dropped")
	}
	if elapsed := time.Since(started); elapsed < 400*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("silent client dropped after %v, want about the 500ms handshake timeout", elapsed)
	}

	garbage, err := net.Dial("tcp", receiverAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer garbage.Close()
	garbage.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	garbage.SetReadDeadline(time.Now().Add(5 * time.Second))
	garbage.Read(make([]byte, 1))

	waitFor(t, "two handshake failures", func() bool {
		return metricValue(t, deps, "sluice_tls_handshake_failures_total", ReceiverMode) == 2
	})
	if metricValue(t, deps, "sluice_relay_ended_total", metrics.CauseClean) != 0 {
		t.Fatal("no relay should have run")
	}
}

func TestReceiverRefusesHalfACertificatePair(t *testing.T) {
	dir := t.TempDir()
	cfg := receiverConfig(t, dir, "127.0.0.1:1")
	if _, err := certs.Generate(cfg.CertFile, cfg.KeyFile, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg.KeyFile); err != nil {
		t.Fatal(err)
	}
	if _, err := NewReceiver(cfg, newDeps(nil)); err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("err = %v, want a complaint about the missing key", err)
	}
}

func TestConfigValidation(t *testing.T) {
	receiver := &ReceiverConfig{Common: app.Common{Admin: admin.Config{Disabled: true}}, ListenAddr: ":0", TargetAddr: "h:1"}
	if err := receiver.Validate(); err != nil {
		t.Fatal(err)
	}
	if receiver.CertFile != DefaultCertFile || receiver.KeyFile != DefaultKeyFile || receiver.HandshakeTimeout.Duration() != DefaultHandshakeTimeout || receiver.CertHosts[0] != "localhost" {
		t.Errorf("receiver defaults = %+v", receiver)
	}
	same := &ReceiverConfig{Common: app.Common{Admin: admin.Config{Disabled: true}}, ListenAddr: ":0", TargetAddr: "h:1", CertFile: "x.pem", KeyFile: "x.pem"}
	var fieldErr *config.FieldError
	if err := same.Validate(); !errors.As(err, &fieldErr) || fieldErr.Field != "keyFile" {
		t.Errorf("same cert and key file: err = %v", err)
	}

	cases := map[string]struct {
		cfg   EntryConfig
		field string
	}{
		"neither pin nor insecure": {EntryConfig{ListenAddr: ":0", ReceiverAddr: "h:1"}, "pinnedCertFile"},
		"both pin and insecure":    {EntryConfig{ListenAddr: ":0", ReceiverAddr: "h:1", PinnedCertFile: "r.crt", InsecureSkipVerify: true}, "insecureSkipVerify"},
		"bad receiver":             {EntryConfig{ListenAddr: ":0", ReceiverAddr: ":1", PinnedCertFile: "r.crt"}, "receiverAddr"},
		"idle too short":           {EntryConfig{Config: serving.Config{IdleTimeout: config.Duration(time.Millisecond)}, ListenAddr: ":0", ReceiverAddr: "h:1", PinnedCertFile: "r.crt"}, "idleTimeout"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := c.cfg
			cfg.Common = app.Common{Admin: admin.Config{Disabled: true}}
			err := cfg.Validate()
			var fieldErr *config.FieldError
			if !errors.As(err, &fieldErr) || fieldErr.Field != c.field {
				t.Fatalf("err = %v, want FieldError on %s", err, c.field)
			}
		})
	}
}
