package tlsrelay

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/devarashs/sluice/internal/app"
	"github.com/devarashs/sluice/internal/certs"
	"github.com/devarashs/sluice/internal/config"
	"github.com/devarashs/sluice/internal/listen"
	"github.com/devarashs/sluice/internal/logging"
	"github.com/devarashs/sluice/internal/metrics"
	"github.com/devarashs/sluice/internal/relay"
	"github.com/devarashs/sluice/internal/serving"
)

// EntryConfig is the `sluice tls entry` configuration file.
type EntryConfig struct {
	app.Common
	serving.Config

	// ListenAddr is where clients connect in the clear.
	ListenAddr string `json:"listenAddr"`
	// ReceiverAddr is the receiver's TLS listen address.
	ReceiverAddr string `json:"receiverAddr"`
	// PinnedCertFile holds the receiver's certificate, or several during a
	// rotation. The receiver must present exactly one of them.
	PinnedCertFile string `json:"pinnedCertFile"`
	// InsecureSkipVerify connects without checking who answers. It is the
	// only alternative to a pin, it is logged loudly, and it leaves the hop
	// open to anyone who can intercept traffic to the receiver.
	InsecureSkipVerify bool `json:"insecureSkipVerify"`
}

// Validate applies defaults and checks every field. Exactly one of a pin and
// the insecure flag must be given.
func (c *EntryConfig) Validate() error {
	if err := c.Common.Validate(); err != nil {
		return err
	}
	if err := c.Config.Validate(); err != nil {
		return err
	}
	if err := config.ListenAddr("listenAddr", c.ListenAddr); err != nil {
		return err
	}
	if err := config.DialAddr("receiverAddr", c.ReceiverAddr); err != nil {
		return err
	}
	switch {
	case c.PinnedCertFile == "" && !c.InsecureSkipVerify:
		return config.Invalid("pinnedCertFile", "is required; copy the receiver's certificate file here, or set insecureSkipVerify to connect unverified")
	case c.PinnedCertFile != "" && c.InsecureSkipVerify:
		return config.Invalid("insecureSkipVerify", "has no effect when pinnedCertFile is set; remove one of them")
	}
	return nil
}

// Entry is a bound `sluice tls entry` instance.
type Entry struct {
	cfg         *EntryConfig
	deps        app.Deps
	listener    *listen.Listener
	metrics     *metrics.Connections
	dialer      *tls.Dialer
	unreachable *logging.Throttle
}

// NewEntry loads the pin, binds the listen address, and registers the
// metrics. cfg must have passed Validate.
func NewEntry(cfg *EntryConfig, deps app.Deps) (*Entry, error) {
	var tlsConfig *tls.Config
	if cfg.InsecureSkipVerify {
		deps.Logger.Warn("insecureSkipVerify is set: the receiver is not verified and this hop can be intercepted by anyone on the path")
		unverified, err := certs.ClientConfig(nil, true)
		if err != nil {
			return nil, err
		}
		tlsConfig = unverified
	} else {
		pins, err := certs.LoadPins(cfg.PinnedCertFile)
		if err != nil {
			return nil, err
		}
		pinned, err := certs.ClientConfig(pins, false)
		if err != nil {
			return nil, err
		}
		tlsConfig = pinned
		for _, pin := range pins {
			deps.Logger.Info("pinned receiver certificate", "file", cfg.PinnedCertFile, "fingerprint", certs.Fingerprint(pin))
		}
	}

	listener, err := cfg.Listen(cfg.ListenAddr, deps.Logger)
	if err != nil {
		return nil, err
	}
	return &Entry{
		cfg:         cfg,
		deps:        deps,
		listener:    listener,
		metrics:     metrics.NewConnections(deps.Registry, EntryMode, serving.Sources(listener)),
		dialer:      &tls.Dialer{NetDialer: cfg.Dialer(), Config: tlsConfig},
		unreachable: logging.NewThrottle(time.Second),
	}, nil
}

// Addr is the bound listen address.
func (e *Entry) Addr() net.Addr {
	return e.listener.Addr()
}

// Serve accepts, encrypts, and forwards until ctx ends, then drains.
func (e *Entry) Serve(ctx context.Context) error {
	e.deps.Logger.Info("entering",
		"listen", e.Addr().String(),
		"receiver", e.cfg.ReceiverAddr,
		"sockets", e.listener.Sockets(),
		"maxConnections", e.cfg.Limits.MaxConnections,
		"idleTimeout", e.cfg.RelayOptions().IdleTimeout,
	)
	e.deps.Ready.Set(nil)
	defer e.deps.Ready.Set(errors.New("shutting down"))
	return e.listener.Serve(ctx, e.handle)
}

// handle dials the receiver over TLS, which includes the handshake and the
// pin check within the dial timeout, and relays.
func (e *Entry) handle(ctx context.Context, client net.Conn) {
	upstream, err := e.dialer.DialContext(ctx, "tcp", e.cfg.ReceiverAddr)
	if err != nil {
		e.metrics.DialFailed()
		e.unreachable.Log(e.deps.Logger, slog.LevelWarn, "receiver unreachable or rejected", "receiver", e.cfg.ReceiverAddr, "error", err)
		client.Close()
		return
	}

	result := relay.Pipe(ctx, client, upstream, e.cfg.RelayOptions())
	e.metrics.RecordRelay(result)
	if result.Err != nil && metrics.RelayCause(result.Err) == metrics.CauseError {
		e.deps.Logger.Debug("relay ended with error", "client", client.RemoteAddr().String(), "error", result.Err)
	}
}

// RunEntry is the app.Main for `sluice tls entry`.
func RunEntry(ctx context.Context, cfg *EntryConfig, deps app.Deps) error {
	entry, err := NewEntry(cfg, deps)
	if err != nil {
		return fmt.Errorf("tls entry: %w", err)
	}
	return entry.Serve(ctx)
}
