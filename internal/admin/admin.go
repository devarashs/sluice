// Package admin serves the operator endpoints every mode exposes on its own
// listener: liveness, readiness, Prometheus metrics, and pprof.
//
// The listener binds to loopback by default. pprof lets anyone who can reach
// it make the process spend CPU on profiles, and metrics reveal traffic
// volumes, so exposing the admin listener beyond the host is something the
// operator chooses explicitly and is warned about.
package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/devarashs/sluice/internal/config"
)

// Config is the `admin` block shared by every mode's configuration file.
type Config struct {
	// Disabled turns the admin listener off entirely.
	Disabled bool `json:"disabled"`
	// ListenAddr is where the admin server listens. Default 127.0.0.1:9800.
	ListenAddr string `json:"listenAddr"`
}

// DefaultListenAddr keeps the admin endpoints on the host unless configured
// otherwise.
const DefaultListenAddr = "127.0.0.1:9800"

// shutdownGrace bounds how long an in-flight request, such as a 30-second
// CPU profile, can delay process exit.
const shutdownGrace = 5 * time.Second

// Validate applies the default address and checks it. prefix is the JSON
// path of this block, such as "admin", used in errors.
func (c *Config) Validate(prefix string) error {
	if c.Disabled {
		return nil
	}
	if c.ListenAddr == "" {
		c.ListenAddr = DefaultListenAddr
	}
	return config.ListenAddr(prefix+".listenAddr", c.ListenAddr)
}

// ReadinessFunc reports whether the mode can take traffic. Nil means ready;
// the error text is returned to whoever asked /readyz.
type ReadinessFunc func() error

// Server is the admin HTTP server. Build it with New; run it with Serve.
type Server struct {
	cfg        Config
	logger     *slog.Logger
	httpServer *http.Server
}

// New builds the admin server. ready answers /readyz, gatherer answers
// /metrics, and pprof is always mounted under /debug/pprof/.
func New(cfg Config, ready ReadinessFunc, gatherer prometheus.Gatherer, logger *slog.Logger) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleIndex)
	mux.HandleFunc("GET /healthz", handleHealth)
	mux.HandleFunc("GET /readyz", readinessHandler(ready))
	mux.Handle("GET /metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
		Timeout:  10 * time.Second,
	}))
	// pprof.Index serves the named profiles under this prefix (heap,
	// goroutine, block, mutex, allocs, threadcreate) as well as the index.
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	// pprof.Symbol accepts GET and POST. Registering it method by method keeps
	// it from conflicting with the GET-only prefix pattern above.
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("POST /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)

	return &Server{
		cfg:    cfg,
		logger: logger,
		httpServer: &http.Server{
			Handler: mux,
			// A CPU profile legitimately takes 30 seconds to write, so there is
			// no write timeout; slow clients are bounded by the header timeout
			// and by the listener being on loopback.
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       60 * time.Second,
			ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		},
	}
}

// Listen binds the configured address and returns the listener, so callers
// that bind port 0 can learn the port. It warns when the address is reachable
// from beyond this host.
func (s *Server) Listen() (net.Listener, error) {
	listener, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("admin: listen %s: %w", s.cfg.ListenAddr, err)
	}
	if exposesBeyondHost(s.cfg.ListenAddr) {
		s.logger.Warn("admin listener is reachable beyond this host; pprof and metrics are exposed to the network", "addr", listener.Addr().String())
	}
	return listener, nil
}

// Serve answers requests on listener until ctx is cancelled, then shuts down
// with a bounded grace period. It returns nil on a clean stop.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	s.logger.Info("admin listener", "addr", listener.Addr().String())

	served := make(chan error, 1)
	go func() { served <- s.httpServer.Serve(listener) }()

	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("admin: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
			// Shutdown gave in-flight requests their chance; close the rest.
			s.httpServer.Close()
		}
		<-served
		return nil
	}
}

// ListenAndServe is Listen followed by Serve.
func (s *Server) ListenAndServe(ctx context.Context) error {
	listener, err := s.Listen()
	if err != nil {
		return err
	}
	return s.Serve(ctx, listener)
}

func handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, "sluice admin\n\n/healthz       liveness\n/readyz        readiness\n/metrics       prometheus\n/debug/pprof/  profiles\n")
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, "ok\n")
}

func readinessHandler(ready ReadinessFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err := ready(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "not ready: %s\n", err)
			return
		}
		fmt.Fprint(w, "ready\n")
	}
}

// exposesBeyondHost reports whether a listen address can receive connections
// from other machines: an unspecified or empty host, or a non-loopback IP or
// name.
func exposesBeyondHost(listenAddr string) bool {
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil || host == "" {
		return true
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	return !ip.IsLoopback()
}
