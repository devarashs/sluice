// Package tlsrelay is the encrypted hop: `sluice tls entry` on one host
// accepts plain connections and carries each one over TLS to
// `sluice tls receiver` on another, which hands it to the target.
//
// Trust runs one way and is a pin. The receiver owns a certificate, which it
// generates on first start if it has none, and every entry holds a copy of
// that exact certificate and accepts nothing else. There is no CA, no
// hostname check, and no expiry to forget about.
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

// Mode labels reported in metrics.
const (
	ReceiverMode = "tls-receiver"
	EntryMode    = "tls-entry"
)

// ReceiverConfig is the `sluice tls receiver` configuration file.
type ReceiverConfig struct {
	app.Common
	serving.Config

	// ListenAddr is where entries connect over TLS.
	ListenAddr string `json:"listenAddr"`
	// TargetAddr is where every decrypted connection is delivered.
	TargetAddr string `json:"targetAddr"`
	// CertFile and KeyFile hold the receiver's certificate. When neither
	// exists a pair is generated. Defaults receiver.crt and receiver.key.
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
	// CertHosts are the subject alternative names written into a generated
	// certificate. Pinning ignores them; they only matter for debugging with
	// tools that check names. Default localhost.
	CertHosts []string `json:"certHosts"`
	// HandshakeTimeout bounds how long an accepted connection may take to
	// complete its TLS handshake before it is dropped. Default 10s.
	HandshakeTimeout config.Duration `json:"handshakeTimeout"`
}

// Defaults applied by Validate.
const (
	DefaultCertFile         = "receiver.crt"
	DefaultKeyFile          = "receiver.key"
	DefaultHandshakeTimeout = 10 * time.Second
)

// Validate applies defaults and checks every field.
func (c *ReceiverConfig) Validate() error {
	if err := c.Common.Validate(); err != nil {
		return err
	}
	if err := c.Config.Validate(); err != nil {
		return err
	}
	if err := config.ListenAddr("listenAddr", c.ListenAddr); err != nil {
		return err
	}
	if err := config.DialAddr("targetAddr", c.TargetAddr); err != nil {
		return err
	}
	if c.CertFile == "" {
		c.CertFile = DefaultCertFile
	}
	if c.KeyFile == "" {
		c.KeyFile = DefaultKeyFile
	}
	if c.CertFile == c.KeyFile {
		return config.Invalid("keyFile", "must differ from certFile")
	}
	if len(c.CertHosts) == 0 {
		c.CertHosts = []string{"localhost"}
	}
	if c.HandshakeTimeout == 0 {
		c.HandshakeTimeout = config.Duration(DefaultHandshakeTimeout)
	}
	return nil
}

// Receiver is a bound `sluice tls receiver` instance.
type Receiver struct {
	cfg         *ReceiverConfig
	deps        app.Deps
	listener    *listen.Listener
	metrics     *metrics.Connections
	tlsConfig   *tls.Config
	dialer      *net.Dialer
	unreachable *logging.Throttle
	badClients  *logging.Throttle
}

// NewReceiver loads or generates the certificate, binds the listen address,
// and registers the metrics. cfg must have passed Validate.
func NewReceiver(cfg *ReceiverConfig, deps app.Deps) (*Receiver, error) {
	cert, generated, err := certs.EnsureServer(cfg.CertFile, cfg.KeyFile, cfg.CertHosts)
	if err != nil {
		return nil, err
	}
	fingerprint := certs.Fingerprint(cert.Leaf)
	if generated {
		deps.Logger.Info("generated a new certificate; copy the certificate file to every entry as its pinnedCertFile",
			"certFile", cfg.CertFile, "keyFile", cfg.KeyFile, "fingerprint", fingerprint)
	} else {
		deps.Logger.Info("loaded certificate", "certFile", cfg.CertFile, "fingerprint", fingerprint)
	}

	listener, err := cfg.Listen(cfg.ListenAddr, deps.Logger)
	if err != nil {
		return nil, err
	}
	return &Receiver{
		cfg:         cfg,
		deps:        deps,
		listener:    listener,
		metrics:     metrics.NewConnections(deps.Registry, ReceiverMode, serving.Sources(listener)),
		tlsConfig:   certs.ServerConfig(cert),
		dialer:      cfg.Dialer(),
		unreachable: logging.NewThrottle(time.Second),
		badClients:  logging.NewThrottle(time.Second),
	}, nil
}

// Addr is the bound listen address.
func (r *Receiver) Addr() net.Addr {
	return r.listener.Addr()
}

// Serve accepts, decrypts, and delivers until ctx ends, then drains.
func (r *Receiver) Serve(ctx context.Context) error {
	r.deps.Logger.Info("receiving",
		"listen", r.Addr().String(),
		"target", r.cfg.TargetAddr,
		"sockets", r.listener.Sockets(),
		"maxConnections", r.cfg.Limits.MaxConnections,
		"idleTimeout", r.cfg.RelayOptions().IdleTimeout,
	)
	r.deps.Ready.Set(nil)
	defer r.deps.Ready.Set(errors.New("shutting down"))
	return r.listener.Serve(ctx, r.handle)
}

// handle completes the TLS handshake under a deadline, dials the target, and
// relays. A client that never finishes its handshake is dropped so it cannot
// hold a slot for the idle timeout.
func (r *Receiver) handle(ctx context.Context, raw net.Conn) {
	conn := tls.Server(raw, r.tlsConfig)
	_ = raw.SetDeadline(time.Now().Add(r.cfg.HandshakeTimeout.Duration()))
	if err := conn.HandshakeContext(ctx); err != nil {
		r.metrics.HandshakeFailed()
		r.badClients.Log(r.deps.Logger, slog.LevelWarn, "tls handshake failed", "client", raw.RemoteAddr().String(), "error", err)
		conn.Close()
		return
	}

	target, err := r.dialer.DialContext(ctx, "tcp", r.cfg.TargetAddr)
	if err != nil {
		r.metrics.DialFailed()
		r.unreachable.Log(r.deps.Logger, slog.LevelWarn, "target unreachable", "target", r.cfg.TargetAddr, "error", err)
		conn.Close()
		return
	}

	result := relay.Pipe(ctx, conn, target, r.cfg.RelayOptions())
	r.metrics.RecordRelay(result)
	if result.Err != nil && metrics.RelayCause(result.Err) == metrics.CauseError {
		r.deps.Logger.Debug("relay ended with error", "client", raw.RemoteAddr().String(), "error", result.Err)
	}
}

// RunReceiver is the app.Main for `sluice tls receiver`.
func RunReceiver(ctx context.Context, cfg *ReceiverConfig, deps app.Deps) error {
	receiver, err := NewReceiver(cfg, deps)
	if err != nil {
		return fmt.Errorf("tls receiver: %w", err)
	}
	return receiver.Serve(ctx)
}
