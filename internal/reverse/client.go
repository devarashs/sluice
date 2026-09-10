package reverse

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/devarashs/sluice/internal/app"
	"github.com/devarashs/sluice/internal/backoff"
	"github.com/devarashs/sluice/internal/certs"
	"github.com/devarashs/sluice/internal/config"
	"github.com/devarashs/sluice/internal/logging"
	"github.com/devarashs/sluice/internal/metrics"
	"github.com/devarashs/sluice/internal/relay"
	"github.com/devarashs/sluice/internal/serving"
)

// ClientMode is the label the reverse client reports in metrics.
const ClientMode = "reverse-client"

// ClientBinding maps a public address the server should open to the local
// service the client delivers those connections to.
type ClientBinding struct {
	// PublicAddr is what the server listens on for users.
	PublicAddr string `json:"publicAddr"`
	// TargetAddr is the local service this client dials for that public port.
	TargetAddr string `json:"targetAddr"`
}

// ClientConfig is the `sluice reverse client` configuration file. The client
// runs beside the private services: it dials the server, asks it to open the
// public addresses, and delivers the connections the server sends back to the
// matching local target.
type ClientConfig struct {
	app.Common
	serving.Config

	// ServerAddr is the reverse server's tunnel address.
	ServerAddr string `json:"serverAddr"`
	// Token is the shared secret the server checks.
	Token string `json:"token"`
	// PinnedCertFile holds the server's certificate, or several during a
	// rotation. Exactly one of this and InsecureSkipVerify must be set.
	PinnedCertFile string `json:"pinnedCertFile"`
	// InsecureSkipVerify connects without verifying the server. Logged loudly.
	InsecureSkipVerify bool `json:"insecureSkipVerify"`
	// Bindings are the public-to-local mappings this client requests.
	Bindings []ClientBinding `json:"bindings"`

	// ReconnectMinBackoff and ReconnectMaxBackoff bound the wait between
	// reconnect attempts. Defaults 1s and 1m.
	ReconnectMinBackoff config.Duration `json:"reconnectMinBackoff"`
	ReconnectMaxBackoff config.Duration `json:"reconnectMaxBackoff"`
}

// Defaults applied by Validate.
const (
	DefaultReconnectMinBackoff = time.Second
	DefaultReconnectMaxBackoff = time.Minute
	// healthySession is how long a session must last to count as healthy and
	// reset the backoff, so a server that accepts and immediately drops does
	// not defeat the backoff.
	healthySession = 30 * time.Second
)

// Validate applies defaults and checks every field.
func (c *ClientConfig) Validate() error {
	if err := c.Common.Validate(); err != nil {
		return err
	}
	if err := c.Config.Validate(); err != nil {
		return err
	}
	if err := config.DialAddr("serverAddr", c.ServerAddr); err != nil {
		return err
	}
	if err := config.Require("token", c.Token); err != nil {
		return err
	}
	switch {
	case c.PinnedCertFile == "" && !c.InsecureSkipVerify:
		return config.Invalid("pinnedCertFile", "is required; copy the server's certificate file here, or set insecureSkipVerify to connect unverified")
	case c.PinnedCertFile != "" && c.InsecureSkipVerify:
		return config.Invalid("insecureSkipVerify", "has no effect when pinnedCertFile is set; remove one of them")
	}
	if len(c.Bindings) == 0 {
		return config.Invalid("bindings", "at least one binding is required")
	}
	if len(c.Bindings) > MaxBindings {
		return config.Invalid("bindings", "more than %d bindings", MaxBindings)
	}
	seen := make(map[string]struct{}, len(c.Bindings))
	for i, b := range c.Bindings {
		if err := config.ListenAddr(fmt.Sprintf("bindings[%d].publicAddr", i), b.PublicAddr); err != nil {
			return err
		}
		if err := config.DialAddr(fmt.Sprintf("bindings[%d].targetAddr", i), b.TargetAddr); err != nil {
			return err
		}
		if _, dup := seen[b.PublicAddr]; dup {
			return config.Invalid(fmt.Sprintf("bindings[%d].publicAddr", i), "duplicate public address %q", b.PublicAddr)
		}
		seen[b.PublicAddr] = struct{}{}
	}
	if c.ReconnectMinBackoff == 0 {
		c.ReconnectMinBackoff = config.Duration(DefaultReconnectMinBackoff)
	}
	if c.ReconnectMaxBackoff == 0 {
		c.ReconnectMaxBackoff = config.Duration(DefaultReconnectMaxBackoff)
	}
	if c.ReconnectMaxBackoff < c.ReconnectMinBackoff {
		return config.Invalid("reconnectMaxBackoff", "must not be less than reconnectMinBackoff")
	}
	return nil
}

// protocolBindings is what the client asks the server to open, in order, so
// the returned results and the stream ids line up with c.Bindings.
func (c *ClientConfig) protocolBindings() []Binding {
	out := make([]Binding, len(c.Bindings))
	for i, b := range c.Bindings {
		out[i] = Binding{PublicAddr: b.PublicAddr}
	}
	return out
}

// Client is a bound `sluice reverse client` instance.
type Client struct {
	cfg         *ClientConfig
	deps        app.Deps
	tlsConfig   *tls.Config
	dialer      *net.Dialer
	localDialer *net.Dialer
	metrics     *metrics.Connections
	backoff     *backoff.Backoff
	unreachable *logging.Throttle

	accepted atomic.Uint64 // streams accepted from the server over the client's life
	active   atomic.Int64  // tunnelled connections in flight
}

// NewClient loads the pin and prepares the dialers. cfg must have passed
// Validate.
func NewClient(cfg *ClientConfig, deps app.Deps) (*Client, error) {
	tlsConfig, err := clientTLSConfig(cfg, deps.Logger)
	if err != nil {
		return nil, err
	}
	c := &Client{
		cfg:         cfg,
		deps:        deps,
		tlsConfig:   tlsConfig,
		dialer:      cfg.Dialer(),
		localDialer: &net.Dialer{Timeout: cfg.DialTimeout.Duration()},
		backoff:     backoff.New(cfg.ReconnectMinBackoff.Duration(), cfg.ReconnectMaxBackoff.Duration(), 2),
		unreachable: logging.NewThrottle(time.Second),
	}
	c.metrics = metrics.NewConnections(deps.Registry, ClientMode, metrics.ConnectionSources{
		Accepted:       func() uint64 { return c.accepted.Load() },
		Active:         func() int { return int(c.active.Load()) },
		RejectedByRate: func() uint64 { return 0 },
	})
	return c, nil
}

// clientTLSConfig builds the pinned or insecure TLS configuration.
func clientTLSConfig(cfg *ClientConfig, logger *slog.Logger) (*tls.Config, error) {
	if cfg.InsecureSkipVerify {
		logger.Warn("insecureSkipVerify is set: the server is not verified and this tunnel can be intercepted by anyone on the path")
		return certs.ClientConfig(nil, true)
	}
	pins, err := certs.LoadPins(cfg.PinnedCertFile)
	if err != nil {
		return nil, err
	}
	for _, pin := range pins {
		logger.Info("pinned server certificate", "file", cfg.PinnedCertFile, "fingerprint", certs.Fingerprint(pin))
	}
	return certs.ClientConfig(pins, false)
}

// Serve connects, serves streams, and reconnects with backoff until ctx ends.
func (c *Client) Serve(ctx context.Context) error {
	c.deps.Logger.Info("reverse client", "server", c.cfg.ServerAddr, "bindings", len(c.cfg.Bindings))
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := c.connectAndServe(ctx); err != nil && ctx.Err() == nil {
			c.deps.Ready.Set(errors.New("disconnected"))
			delay := c.backoff.Duration()
			c.unreachable.Log(c.deps.Logger, slog.LevelWarn, "session ended; reconnecting", "error", err, "retryIn", delay.Round(time.Millisecond))
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// connectAndServe runs one session: dial, handshake, hello, then serve
// streams until the session ends. It returns the reason the session ended.
func (c *Client) connectAndServe(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, c.cfg.DialTimeout.Duration())
	defer cancel()
	raw, err := c.dialer.DialContext(dialCtx, "tcp", c.cfg.ServerAddr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	conn := tls.Client(raw, c.tlsConfig)
	_ = raw.SetDeadline(time.Now().Add(c.cfg.DialTimeout.Duration()))
	if err := conn.HandshakeContext(dialCtx); err != nil {
		conn.Close()
		return fmt.Errorf("tls handshake: %w", err)
	}

	if err := WriteClientHello(conn, c.cfg.Token, c.cfg.protocolBindings()); err != nil {
		conn.Close()
		return fmt.Errorf("send hello: %w", err)
	}
	hello, err := ReadServerHello(conn, len(c.cfg.Bindings))
	if err != nil {
		conn.Close()
		return fmt.Errorf("read server hello: %w", err)
	}
	_ = raw.SetDeadline(time.Time{})
	if hello.Status != StatusAccepted {
		conn.Close()
		return fmt.Errorf("server rejected the client: %s", hello.Status)
	}
	if bound := c.reportBindings(hello.Results); bound == 0 {
		conn.Close()
		return errors.New("server bound none of the requested addresses")
	}

	session, err := yamux.Client(conn, sessionConfig(logWriter{c.deps.Logger}))
	if err != nil {
		conn.Close()
		return fmt.Errorf("start session: %w", err)
	}
	defer session.Close()

	c.deps.Ready.Set(nil)
	started := time.Now()
	err = c.serveStreams(ctx, session)
	if time.Since(started) >= healthySession {
		c.backoff.Reset()
	}
	return err
}

// reportBindings logs the outcome of each requested binding and returns how
// many were bound.
func (c *Client) reportBindings(results []BindResult) int {
	bound := 0
	for i, r := range results {
		b := c.cfg.Bindings[i]
		if r.OK {
			bound++
			c.deps.Logger.Info("binding open", "public", b.PublicAddr, "target", b.TargetAddr)
		} else {
			c.deps.Logger.Warn("binding refused by server", "public", b.PublicAddr, "reason", r.Message)
		}
	}
	return bound
}

// serveStreams accepts streams until the session ends or ctx is cancelled.
func (c *Client) serveStreams(ctx context.Context, session *yamux.Session) error {
	go func() {
		<-ctx.Done()
		session.Close()
	}()
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept stream: %w", err)
		}
		go c.handleStream(ctx, stream)
	}
}

// handleStream reads the binding id, dials that binding's local target, and
// relays. The stream is closed by the relay.
func (c *Client) handleStream(ctx context.Context, stream *yamux.Stream) {
	id, err := ReadStreamHeader(stream, len(c.cfg.Bindings))
	if err != nil {
		c.deps.Logger.Warn("bad stream header", "error", err)
		stream.Close()
		return
	}
	target := c.cfg.Bindings[id].TargetAddr

	local, err := c.localDialer.DialContext(ctx, "tcp", target)
	if err != nil {
		c.metrics.DialFailed()
		c.unreachable.Log(c.deps.Logger, slog.LevelWarn, "local target unreachable", "target", target, "error", err)
		stream.Close()
		return
	}

	c.accepted.Add(1)
	c.active.Add(1)
	defer c.active.Add(-1)
	result := relay.Pipe(ctx, stream, local, c.cfg.RelayOptions())
	c.metrics.RecordRelay(result)
}

// RunClient is the app.Main for `sluice reverse client`.
func RunClient(ctx context.Context, cfg *ClientConfig, deps app.Deps) error {
	client, err := NewClient(cfg, deps)
	if err != nil {
		return fmt.Errorf("reverse client: %w", err)
	}
	return client.Serve(ctx)
}
