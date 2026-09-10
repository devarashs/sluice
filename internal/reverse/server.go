package reverse

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/devarashs/sluice/internal/app"
	"github.com/devarashs/sluice/internal/certs"
	"github.com/devarashs/sluice/internal/config"
	"github.com/devarashs/sluice/internal/limits"
	"github.com/devarashs/sluice/internal/listen"
	"github.com/devarashs/sluice/internal/logging"
	"github.com/devarashs/sluice/internal/metrics"
	"github.com/devarashs/sluice/internal/relay"
	"github.com/devarashs/sluice/internal/serving"
)

// ServerMode is the label the reverse server reports in metrics.
const ServerMode = "reverse-server"

// ServerConfig is the `sluice reverse server` configuration file. The server
// runs on the public host: clients dial its tunnel address over TLS, and it
// opens each client's requested public ports and carries user connections to
// that client over the multiplexed session.
type ServerConfig struct {
	app.Common
	serving.Config

	// TunnelListenAddr is where clients connect over TLS.
	TunnelListenAddr string `json:"tunnelListenAddr"`
	// Token is the shared secret every client must present.
	Token string `json:"token"`
	// CertFile and KeyFile hold the server's certificate, generated when
	// neither exists. Defaults reverse-server.crt and reverse-server.key.
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
	// CertHosts are the SANs written into a generated certificate. Pinning
	// ignores them. Default localhost.
	CertHosts []string `json:"certHosts"`
	// HandshakeTimeout bounds TLS plus the client hello. Default 10s.
	HandshakeTimeout config.Duration `json:"handshakeTimeout"`
	// MaxClients caps simultaneous client sessions. Default 128.
	MaxClients int `json:"maxClients"`
}

// Defaults applied by Validate.
const (
	DefaultServerCertFile   = "reverse-server.crt"
	DefaultServerKeyFile    = "reverse-server.key"
	DefaultMaxClients       = 128
	DefaultHandshakeTimeout = 10 * time.Second
)

// Validate applies defaults and checks every field.
func (c *ServerConfig) Validate() error {
	if err := c.Common.Validate(); err != nil {
		return err
	}
	if err := c.Config.Validate(); err != nil {
		return err
	}
	if err := config.ListenAddr("tunnelListenAddr", c.TunnelListenAddr); err != nil {
		return err
	}
	if err := config.Require("token", c.Token); err != nil {
		return err
	}
	if c.CertFile == "" {
		c.CertFile = DefaultServerCertFile
	}
	if c.KeyFile == "" {
		c.KeyFile = DefaultServerKeyFile
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
	if c.MaxClients < 0 {
		return config.Invalid("maxClients", "must not be negative, got %d", c.MaxClients)
	}
	if c.MaxClients == 0 {
		c.MaxClients = DefaultMaxClients
	}
	return nil
}

// Server is a bound `sluice reverse server` instance.
type Server struct {
	cfg       *ServerConfig
	deps      app.Deps
	tlsConfig *tls.Config
	tunnel    net.Listener
	clientCap *limits.Concurrency
	metrics   *metrics.Connections
	badClient *logging.Throttle

	clients atomic.Int64 // connected client sessions

	// live is the set of public listeners across all clients, summed on a
	// metrics scrape. retired* carry the counters of listeners already torn
	// down so the totals stay monotonic.
	mu              sync.Mutex
	live            map[*listen.Listener]struct{}
	retiredAccepted uint64
	retiredRejected uint64
}

// NewServer loads or generates the certificate and binds the tunnel address.
// cfg must have passed Validate.
func NewServer(cfg *ServerConfig, deps app.Deps) (*Server, error) {
	cert, generated, err := certs.EnsureServer(cfg.CertFile, cfg.KeyFile, cfg.CertHosts)
	if err != nil {
		return nil, err
	}
	fingerprint := certs.Fingerprint(cert.Leaf)
	if generated {
		deps.Logger.Info("generated a new certificate; copy the certificate file to every reverse client as its pinnedCertFile",
			"certFile", cfg.CertFile, "fingerprint", fingerprint)
	} else {
		deps.Logger.Info("loaded certificate", "certFile", cfg.CertFile, "fingerprint", fingerprint)
	}

	// A plain TCP listener: handleClient wraps each connection in tls.Server
	// itself so it can drive the handshake under a deadline. Using tls.Listen
	// here would wrap it a second time.
	tunnel, err := net.Listen("tcp", cfg.TunnelListenAddr)
	if err != nil {
		return nil, fmt.Errorf("reverse server: listen %s: %w", cfg.TunnelListenAddr, err)
	}

	s := &Server{
		cfg:       cfg,
		deps:      deps,
		tlsConfig: certs.ServerConfig(cert),
		tunnel:    tunnel,
		clientCap: limits.NewConcurrency(cfg.MaxClients),
		badClient: logging.NewThrottle(time.Second),
		live:      make(map[*listen.Listener]struct{}),
	}
	s.metrics = metrics.NewConnections(deps.Registry, ServerMode, metrics.ConnectionSources{
		Accepted:       s.totalAccepted,
		Active:         s.totalActive,
		RejectedByRate: s.totalRejected,
	})
	return s, nil
}

// Addr is the bound tunnel address.
func (s *Server) Addr() net.Addr {
	return s.tunnel.Addr()
}

// Serve accepts client sessions until ctx ends, then stops and waits for the
// sessions in flight to finish.
func (s *Server) Serve(ctx context.Context) error {
	s.deps.Logger.Info("reverse server", "tunnel", s.Addr().String(), "maxClients", s.cfg.MaxClients)
	s.deps.Ready.Set(nil)
	defer s.deps.Ready.Set(errors.New("shutting down"))

	var sessions sync.WaitGroup
	go func() {
		<-ctx.Done()
		s.tunnel.Close()
	}()

	for {
		raw, err := s.tunnel.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			s.deps.Logger.Warn("tunnel accept failed", "error", err)
			continue
		}
		if !s.clientCap.TryAcquire() {
			s.deps.Logger.Warn("client rejected: at maxClients", "maxClients", s.cfg.MaxClients, "client", raw.RemoteAddr().String())
			raw.Close()
			continue
		}
		sessions.Add(1)
		go func() {
			defer sessions.Done()
			defer s.clientCap.Release()
			s.handleClient(ctx, raw)
		}()
	}
	sessions.Wait()
	return nil
}

// handleClient runs one client session start to finish: TLS, authenticate,
// bind the requested public addresses, then multiplex user connections over
// yamux until the session ends.
func (s *Server) handleClient(ctx context.Context, raw net.Conn) {
	clientAddr := raw.RemoteAddr().String()
	conn := tls.Server(raw, s.tlsConfig)

	// One deadline covers the TLS handshake and the client hello together, so
	// a peer that connects and stalls cannot hold the slot.
	_ = raw.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout.Duration()))
	if err := conn.HandshakeContext(ctx); err != nil {
		s.badClient.Log(s.deps.Logger, slog.LevelWarn, "client tls handshake failed", "client", clientAddr, "error", err)
		conn.Close()
		return
	}

	hello, err := ReadClientHello(conn, s.cfg.Token)
	if err != nil {
		s.badClient.Log(s.deps.Logger, slog.LevelWarn, "client hello rejected", "client", clientAddr, "error", err)
		_ = WriteServerHello(conn, RejectHello(err))
		conn.Close()
		return
	}
	_ = raw.SetDeadline(time.Time{})

	listeners, results := s.bindAll(hello.Bindings, clientAddr)
	if err := WriteServerHello(conn, ServerHello{Status: StatusAccepted, Results: results}); err != nil {
		s.deps.Logger.Warn("failed to send server hello", "client", clientAddr, "error", err)
		s.closeListeners(listeners)
		conn.Close()
		return
	}
	bound := countBound(results)
	if bound == 0 {
		s.deps.Logger.Warn("no requested address could be bound; dropping client", "client", clientAddr)
		conn.Close()
		return
	}

	session, err := yamux.Server(conn, sessionConfig(logWriter{s.deps.Logger}))
	if err != nil {
		s.deps.Logger.Warn("failed to start session", "client", clientAddr, "error", err)
		s.closeListeners(listeners)
		conn.Close()
		return
	}
	s.clients.Add(1)
	s.deps.Logger.Info("client connected", "client", clientAddr, "bound", bound, "requested", len(hello.Bindings))

	s.runSession(ctx, session, listeners, clientAddr)

	s.clients.Add(-1)
	s.closeListeners(listeners)
	session.Close()
	conn.Close()
	s.deps.Logger.Info("client disconnected", "client", clientAddr)
}

// boundListener pairs a bound public listener with the id the client gave the
// binding, so a stream can be tagged with it.
type boundListener struct {
	id       uint16
	listener *listen.Listener
}

// bindAll tries to open every requested public address and returns the
// listeners that succeeded alongside a result for every request in order.
func (s *Server) bindAll(bindings []Binding, clientAddr string) ([]boundListener, []BindResult) {
	results := make([]BindResult, len(bindings))
	var bound []boundListener
	for i, b := range bindings {
		l, err := s.cfg.Listen(b.PublicAddr, s.deps.Logger)
		if err != nil {
			results[i] = BindResult{OK: false, Message: bindFailureMessage(err)}
			s.deps.Logger.Warn("could not bind requested address", "client", clientAddr, "address", b.PublicAddr, "error", err)
			continue
		}
		results[i] = BindResult{OK: true}
		bound = append(bound, boundListener{id: uint16(i), listener: l})
	}
	return bound, results
}

// runSession serves every bound public listener, opening a stream to the
// client for each user connection, until the session closes or ctx ends.
func (s *Server) runSession(ctx context.Context, session *yamux.Session, listeners []boundListener, clientAddr string) {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// End the session's serving when yamux reports the peer has gone.
	go func() {
		select {
		case <-session.CloseChan():
			cancel()
		case <-sessionCtx.Done():
		}
	}()

	s.trackListeners(listeners, true)
	defer s.trackListeners(listeners, false)

	var loops sync.WaitGroup
	for _, bl := range listeners {
		loops.Add(1)
		go func(bl boundListener) {
			defer loops.Done()
			handler := s.streamHandler(session, bl.id, clientAddr)
			// A public listener's Serve returns when sessionCtx ends.
			_ = bl.listener.Serve(sessionCtx, handler)
		}(bl)
	}
	loops.Wait()
}

// streamHandler returns the per-user-connection handler for one binding: open
// a stream, tag it with the binding id, and relay.
func (s *Server) streamHandler(session *yamux.Session, bindingID uint16, clientAddr string) listen.Handler {
	return func(ctx context.Context, user net.Conn) {
		stream, err := session.OpenStream()
		if err != nil {
			// The session is gone; count it like an unreachable next hop and
			// let the session teardown proceed.
			s.metrics.DialFailed()
			user.Close()
			return
		}
		if err := WriteStreamHeader(stream, bindingID); err != nil {
			s.metrics.DialFailed()
			stream.Close()
			user.Close()
			return
		}
		result := relay.Pipe(ctx, user, stream, s.cfg.RelayOptions())
		s.metrics.RecordRelay(result)
	}
}

// trackListeners adds or removes listeners from the live set used for
// metrics, folding a removed listener's counters into the retired totals.
func (s *Server) trackListeners(listeners []boundListener, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, bl := range listeners {
		if add {
			s.live[bl.listener] = struct{}{}
			continue
		}
		if _, ok := s.live[bl.listener]; ok {
			stats := bl.listener.Stats()
			s.retiredAccepted += stats.Accepted
			s.retiredRejected += stats.RejectedByRate
			delete(s.live, bl.listener)
		}
	}
}

func (s *Server) totalAccepted() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := s.retiredAccepted
	for l := range s.live {
		total += l.Stats().Accepted
	}
	return total
}

func (s *Server) totalRejected() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := s.retiredRejected
	for l := range s.live {
		total += l.Stats().RejectedByRate
	}
	return total
}

func (s *Server) totalActive() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for l := range s.live {
		total += l.Stats().Active
	}
	return total
}

func (s *Server) closeListeners(listeners []boundListener) {
	for _, bl := range listeners {
		bl.listener.Close()
	}
}

// countBound counts the successful results.
func countBound(results []BindResult) int {
	n := 0
	for _, r := range results {
		if r.OK {
			n++
		}
	}
	return n
}

// bindFailureMessage keeps the reason short for the wire.
func bindFailureMessage(err error) string {
	msg := err.Error()
	if len(msg) > maxMessageLen {
		return msg[:maxMessageLen]
	}
	return msg
}

// RunServer is the app.Main for `sluice reverse server`.
func RunServer(ctx context.Context, cfg *ServerConfig, deps app.Deps) error {
	server, err := NewServer(cfg, deps)
	if err != nil {
		return fmt.Errorf("reverse server: %w", err)
	}
	return server.Serve(ctx)
}
