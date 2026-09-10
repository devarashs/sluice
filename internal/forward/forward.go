// Package forward is the `sluice forward` mode: every connection accepted on
// the listen address is dialled through to the target address and relayed.
//
// It is the simplest mode and the one that goes fastest, because on Linux a
// forwarded connection is two kernel sockets joined by splice with no
// user-space buffer at all. Everything else, the caps, the rate limit, the
// idle timeout, the metrics, comes from the shared packages.
package forward

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/devarashs/sluice/internal/app"
	"github.com/devarashs/sluice/internal/config"
	"github.com/devarashs/sluice/internal/listen"
	"github.com/devarashs/sluice/internal/logging"
	"github.com/devarashs/sluice/internal/metrics"
	"github.com/devarashs/sluice/internal/relay"
	"github.com/devarashs/sluice/internal/serving"
)

// Mode is the label this mode reports in metrics.
const Mode = "forward"

// Config is the `sluice forward` configuration file.
type Config struct {
	app.Common
	serving.Config

	// ListenAddr is where clients connect.
	ListenAddr string `json:"listenAddr"`
	// TargetAddr is where every accepted connection is forwarded.
	TargetAddr string `json:"targetAddr"`
}

// Validate applies defaults and checks every field, including the shared
// blocks under their JSON names.
func (c *Config) Validate() error {
	if err := c.Common.Validate(); err != nil {
		return err
	}
	if err := c.Config.Validate(); err != nil {
		return err
	}
	if err := config.ListenAddr("listenAddr", c.ListenAddr); err != nil {
		return err
	}
	return config.DialAddr("targetAddr", c.TargetAddr)
}

// Forwarder is a bound `sluice forward` instance.
type Forwarder struct {
	cfg         *Config
	deps        app.Deps
	listener    *listen.Listener
	metrics     *metrics.Connections
	dialer      *net.Dialer
	unreachable *logging.Throttle
}

// New binds the listen address and registers the metrics. cfg must have
// passed Validate.
func New(cfg *Config, deps app.Deps) (*Forwarder, error) {
	listener, err := cfg.Listen(cfg.ListenAddr, deps.Logger)
	if err != nil {
		return nil, err
	}
	return &Forwarder{
		cfg:         cfg,
		deps:        deps,
		listener:    listener,
		metrics:     metrics.NewConnections(deps.Registry, Mode, serving.Sources(listener)),
		dialer:      cfg.Dialer(),
		unreachable: logging.NewThrottle(time.Second),
	}, nil
}

// Addr is the bound listen address.
func (f *Forwarder) Addr() net.Addr {
	return f.listener.Addr()
}

// Serve accepts and forwards until ctx ends, then drains and returns.
func (f *Forwarder) Serve(ctx context.Context) error {
	f.deps.Logger.Info("forwarding",
		"listen", f.Addr().String(),
		"target", f.cfg.TargetAddr,
		"sockets", f.listener.Sockets(),
		"acceptors", f.cfg.Listener.Acceptors,
		"maxConnections", f.cfg.Limits.MaxConnections,
		"idleTimeout", f.cfg.RelayOptions().IdleTimeout,
	)
	f.deps.Ready.Set(nil)
	defer f.deps.Ready.Set(errors.New("shutting down"))
	return f.listener.Serve(ctx, f.handle)
}

// handle forwards one accepted connection. The relay closes both ends.
func (f *Forwarder) handle(ctx context.Context, client net.Conn) {
	target, err := f.dialer.DialContext(ctx, "tcp", f.cfg.TargetAddr)
	if err != nil {
		f.metrics.DialFailed()
		f.unreachable.Log(f.deps.Logger, slog.LevelWarn, "target unreachable", "target", f.cfg.TargetAddr, "error", err)
		client.Close()
		return
	}

	result := relay.Pipe(ctx, client, target, f.cfg.RelayOptions())
	f.metrics.RecordRelay(result)
	if result.Err != nil && metrics.RelayCause(result.Err) == metrics.CauseError {
		f.deps.Logger.Debug("relay ended with error", "client", client.RemoteAddr().String(), "error", result.Err)
	}
}

// Run is the app.Main for this mode.
func Run(ctx context.Context, cfg *Config, deps app.Deps) error {
	forwarder, err := New(cfg, deps)
	if err != nil {
		return fmt.Errorf("forward: %w", err)
	}
	return forwarder.Serve(ctx)
}
