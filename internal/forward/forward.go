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
	"net"
	"time"

	"github.com/devarashs/sluice/internal/app"
	"github.com/devarashs/sluice/internal/config"
	"github.com/devarashs/sluice/internal/limits"
	"github.com/devarashs/sluice/internal/listen"
	"github.com/devarashs/sluice/internal/metrics"
	"github.com/devarashs/sluice/internal/relay"
)

// Mode is the label this mode reports in metrics.
const Mode = "forward"

// Config is the `sluice forward` configuration file.
type Config struct {
	app.Common

	// ListenAddr is where clients connect.
	ListenAddr string `json:"listenAddr"`
	// TargetAddr is where every accepted connection is forwarded.
	TargetAddr string `json:"targetAddr"`
	// DialTimeout bounds connecting to the target. Default 10s.
	DialTimeout config.Duration `json:"dialTimeout"`
	// IdleTimeout ends a connection when neither direction has moved a byte
	// for this long. Default 10m, minimum 1s.
	IdleTimeout config.Duration `json:"idleTimeout"`
	// NoIdleTimeout disables the idle timeout entirely, for services whose
	// connections legitimately sit silent for hours. TCP keepalive still
	// reaps dead peers.
	NoIdleTimeout bool `json:"noIdleTimeout"`
	// BufferSize is the relay buffer for connections that cannot splice.
	// Default 16KiB.
	BufferSize config.ByteSize `json:"bufferSize"`

	Limits   limits.Config `json:"limits"`
	Listener listen.Config `json:"listener"`
}

// Defaults and bounds applied by Validate.
const (
	DefaultDialTimeout = 10 * time.Second
	DefaultIdleTimeout = 10 * time.Minute
	MinIdleTimeout     = time.Second
)

// Validate applies defaults and checks every field, including the shared
// blocks under their JSON names.
func (c *Config) Validate() error {
	if err := c.Common.Validate(); err != nil {
		return err
	}
	if err := config.ListenAddr("listenAddr", c.ListenAddr); err != nil {
		return err
	}
	if err := config.DialAddr("targetAddr", c.TargetAddr); err != nil {
		return err
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = config.Duration(DefaultDialTimeout)
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = config.Duration(DefaultIdleTimeout)
	}
	if c.IdleTimeout.Duration() < MinIdleTimeout {
		return config.Invalid("idleTimeout", "%s is below the %s minimum; set noIdleTimeout to disable it", c.IdleTimeout, MinIdleTimeout)
	}
	if c.BufferSize == 0 {
		c.BufferSize = config.ByteSize(relay.DefaultBufferSize)
	}
	if c.BufferSize < 4*config.KiB {
		return config.Invalid("bufferSize", "%s is below the 4KiB minimum", c.BufferSize)
	}
	if err := c.Limits.Validate("limits"); err != nil {
		return err
	}
	return c.Listener.Validate("listener")
}

// relayOptions is what the relay gets from this configuration.
func (c *Config) relayOptions() relay.Options {
	opts := relay.Options{IdleTimeout: c.IdleTimeout.Duration(), BufferSize: int(c.BufferSize)}
	if c.NoIdleTimeout {
		opts.IdleTimeout = 0
	}
	return opts
}

// Forwarder is a bound `sluice forward` instance.
type Forwarder struct {
	cfg      *Config
	deps     app.Deps
	listener *listen.Listener
	metrics  *metrics.Connections
	dialer   *net.Dialer
}

// New binds the listen address and registers the metrics. cfg must have
// passed Validate.
func New(cfg *Config, deps app.Deps) (*Forwarder, error) {
	concurrency := limits.NewConcurrency(cfg.Limits.MaxConnections)
	perClient, err := limits.NewPerClient(cfg.Limits.PerClient)
	if err != nil {
		return nil, err
	}
	listener, err := listen.Listen(cfg.ListenAddr, cfg.Listener, listen.Guards{Cap: concurrency, PerClient: perClient}, deps.Logger)
	if err != nil {
		return nil, err
	}

	f := &Forwarder{
		cfg:      cfg,
		deps:     deps,
		listener: listener,
		dialer: &net.Dialer{
			Timeout: cfg.DialTimeout.Duration(),
			// The target side gets the same keepalive policy as the client
			// side, so a dead target is reaped just as promptly.
			KeepAliveConfig: net.KeepAliveConfig{
				Enable:   true,
				Idle:     cfg.Listener.KeepAliveIdle.Duration(),
				Interval: cfg.Listener.KeepAliveInterval.Duration(),
				Count:    cfg.Listener.KeepAliveCount,
			},
		},
	}
	f.metrics = metrics.NewConnections(deps.Registry, Mode, metrics.ConnectionSources{
		Accepted:       func() uint64 { return listener.Stats().Accepted },
		Active:         func() int { return listener.Stats().Active },
		RejectedByRate: func() uint64 { return listener.Stats().RejectedByRate },
	})
	return f, nil
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
		"idleTimeout", f.cfg.relayOptions().IdleTimeout,
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
		f.deps.Logger.Warn("target unreachable", "target", f.cfg.TargetAddr, "client", client.RemoteAddr().String(), "error", err)
		client.Close()
		return
	}

	result := relay.Pipe(ctx, client, target, f.cfg.relayOptions())
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
