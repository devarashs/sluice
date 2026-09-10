package admin

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devarashs/sluice/internal/config"
	"github.com/devarashs/sluice/internal/metrics"
)

func TestValidate(t *testing.T) {
	var cfg Config
	if err := cfg.Validate("admin"); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.ListenAddr != DefaultListenAddr {
		t.Fatalf("default listenAddr = %q", cfg.ListenAddr)
	}

	bad := Config{ListenAddr: "nope"}
	err := bad.Validate("admin")
	var fieldErr *config.FieldError
	if !errors.As(err, &fieldErr) || fieldErr.Field != "admin.listenAddr" {
		t.Fatalf("err = %v, want a FieldError on admin.listenAddr", err)
	}

	disabled := Config{Disabled: true, ListenAddr: "nope"}
	if err := disabled.Validate("admin"); err != nil {
		t.Fatalf("disabled config should not be validated further, got %v", err)
	}
}

// startServer runs an admin server on a free loopback port and returns its
// base URL and a switch that controls readiness.
func startServer(t *testing.T) (baseURL string, ready *atomic.Bool, stop func()) {
	t.Helper()
	ready = new(atomic.Bool)
	readiness := func() error {
		if ready.Load() {
			return nil
		}
		return errors.New("tunnel not connected")
	}
	server := New(Config{ListenAddr: "127.0.0.1:0"}, readiness, metrics.NewRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	listener, err := server.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	stop = func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v, want nil after cancel", err)
			}
		case <-time.After(shutdownGrace + 2*time.Second):
			t.Error("Serve did not return after cancel")
		}
	}
	return "http://" + listener.Addr().String(), ready, stop
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body)
}

func TestEndpoints(t *testing.T) {
	baseURL, ready, stop := startServer(t)
	defer stop()

	if status, body := get(t, baseURL+"/healthz"); status != 200 || body != "ok\n" {
		t.Errorf("/healthz = %d %q", status, body)
	}

	if status, body := get(t, baseURL+"/readyz"); status != 503 || !strings.Contains(body, "tunnel not connected") {
		t.Errorf("/readyz before ready = %d %q, want 503 naming the reason", status, body)
	}
	ready.Store(true)
	if status, body := get(t, baseURL+"/readyz"); status != 200 || body != "ready\n" {
		t.Errorf("/readyz after ready = %d %q", status, body)
	}

	if status, body := get(t, baseURL+"/metrics"); status != 200 || !strings.Contains(body, "sluice_build_info") || !strings.Contains(body, "go_goroutines") {
		t.Errorf("/metrics = %d, body missing expected families", status)
	}

	if status, body := get(t, baseURL+"/debug/pprof/"); status != 200 || !strings.Contains(body, "goroutine") {
		t.Errorf("/debug/pprof/ = %d", status)
	}
	if status, _ := get(t, baseURL+"/debug/pprof/heap"); status != 200 {
		t.Errorf("/debug/pprof/heap = %d", status)
	}

	if status, body := get(t, baseURL+"/"); status != 200 || !strings.Contains(body, "/metrics") {
		t.Errorf("/ = %d %q", status, body)
	}
	if status, _ := get(t, baseURL+"/nope"); status != 404 {
		t.Errorf("/nope = %d, want 404", status)
	}

	response, err := http.Post(baseURL+"/healthz", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz = %d, want 405", response.StatusCode)
	}
}

func TestServeStopsOnCancelAndRefusesAfterwards(t *testing.T) {
	baseURL, _, stop := startServer(t)
	stop()
	client := &http.Client{Timeout: time.Second}
	if _, err := client.Get(baseURL + "/healthz"); err == nil {
		t.Fatal("server still answering after shutdown")
	}
}

func TestListenReportsBindFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	server := New(Config{ListenAddr: occupied.Addr().String()}, func() error { return nil }, metrics.NewRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := server.Listen(); err == nil || !strings.Contains(err.Error(), "admin: listen") {
		t.Fatalf("err = %v, want a listen failure naming the address", err)
	}
}

func TestExposesBeyondHost(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:9800": false,
		"[::1]:9800":     false,
		"localhost:9800": false,
		":9800":          true,
		"0.0.0.0:9800":   true,
		"[::]:9800":      true,
		"10.0.0.5:9800":  true,
		"host.example:1": true,
		"garbage":        true,
	} {
		if got := exposesBeyondHost(addr); got != want {
			t.Errorf("%q = %v, want %v", addr, got, want)
		}
	}
}
