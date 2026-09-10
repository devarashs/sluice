// Package serving holds the tuning every connection-handling mode shares:
// how many connections, how fast, how long they may sit idle, and how the
// listener behaves. Modes embed Config so these keys sit at the top level of
// their file and mean the same thing in every mode.
package serving

import (
	"log/slog"
	"net"
	"time"

	"github.com/devarashs/sluice/internal/config"
	"github.com/devarashs/sluice/internal/limits"
	"github.com/devarashs/sluice/internal/listen"
	"github.com/devarashs/sluice/internal/metrics"
	"github.com/devarashs/sluice/internal/relay"
)

// Config is embedded by every mode that accepts and relays connections.
type Config struct {
	// DialTimeout bounds connecting to the next hop, including any TLS
	// handshake. Default 10s.
	DialTimeout config.Duration `json:"dialTimeout"`
	// IdleTimeout ends a connection when neither direction has moved a byte
	// for this long. Default 10m, minimum 1s.
	IdleTimeout config.Duration `json:"idleTimeout"`
	// NoIdleTimeout disables the idle timeout entirely, for services whose
	// connections legitimately sit silent for hours. TCP keepalive still
	// reaps dead peers.
	NoIdleTimeout bool `json:"noIdleTimeout"`
	// BufferSize is the relay buffer for connections that cannot splice.
	// Default 16KiB, minimum 4KiB.
	BufferSize config.ByteSize `json:"bufferSize"`

	Limits   limits.Config `json:"limits"`
	Listener listen.Config `json:"listener"`
}

// Defaults and bounds applied by Validate.
const (
	DefaultDialTimeout = 10 * time.Second
	DefaultIdleTimeout = 10 * time.Minute
	MinIdleTimeout     = time.Second
	MinBufferSize      = 4 * config.KiB
)

// Validate applies defaults and checks every field, including the shared
// blocks under their JSON names.
func (c *Config) Validate() error {
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
	if c.BufferSize < MinBufferSize {
		return config.Invalid("bufferSize", "%s is below the %s minimum", c.BufferSize, MinBufferSize)
	}
	if err := c.Limits.Validate("limits"); err != nil {
		return err
	}
	return c.Listener.Validate("listener")
}

// RelayOptions is what the relay gets from this configuration.
func (c *Config) RelayOptions() relay.Options {
	opts := relay.Options{IdleTimeout: c.IdleTimeout.Duration(), BufferSize: int(c.BufferSize)}
	if c.NoIdleTimeout {
		opts.IdleTimeout = 0
	}
	return opts
}

// Dialer connects to the next hop with the dial timeout and the same
// keepalive policy the listener applies to clients, so a dead next hop is
// reaped as promptly as a dead client.
func (c *Config) Dialer() *net.Dialer {
	return &net.Dialer{
		Timeout: c.DialTimeout.Duration(),
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     c.Listener.KeepAliveIdle.Duration(),
			Interval: c.Listener.KeepAliveInterval.Duration(),
			Count:    c.Listener.KeepAliveCount,
		},
	}
}

// Listen binds addr with the configured listener settings and the
// configured limits as guards.
func (c *Config) Listen(addr string, logger *slog.Logger) (*listen.Listener, error) {
	perClient, err := limits.NewPerClient(c.Limits.PerClient)
	if err != nil {
		return nil, err
	}
	guards := listen.Guards{Cap: limits.NewConcurrency(c.Limits.MaxConnections), PerClient: perClient}
	return listen.Listen(addr, c.Listener, guards, logger)
}

// Sources exposes a listener's counters to the connection metrics.
func Sources(l *listen.Listener) metrics.ConnectionSources {
	return metrics.ConnectionSources{
		Accepted:       func() uint64 { return l.Stats().Accepted },
		Active:         func() int { return l.Stats().Active },
		RejectedByRate: func() uint64 { return l.Stats().RejectedByRate },
	}
}
