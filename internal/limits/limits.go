// Package limits protects a sluice process from more work than it can carry:
// a cap on connections handled at once, and a per-client rate on new ones.
//
// Both are built so that memory does not grow with load. The concurrency cap
// is a channel semaphore whose zero-size elements cost nothing however high
// the limit is. The per-client limiter keeps its token buckets in a bounded
// LRU, so an attacker cycling through addresses evicts old buckets instead of
// growing a map without end.
package limits

import (
	"math"

	"github.com/devarashs/sluice/internal/config"
)

// Config is the `limits` block shared by every mode's configuration file.
type Config struct {
	// MaxConnections caps the connections handled at once across all of a
	// mode's listeners. Zero means no cap, and the open-file limit decides.
	MaxConnections int `json:"maxConnections"`
	// PerClient rate-limits new connections per client address.
	PerClient PerClientConfig `json:"perClient"`
}

// PerClientConfig tunes the per-client rate limiter. A client is one IPv4
// address or one IPv6 /64, because a host commonly holds a whole /64 and
// could otherwise dodge the limit by rotating within it.
type PerClientConfig struct {
	// ConnectionsPerSecond is the sustained rate of new connections allowed
	// per client. Zero disables the limiter.
	ConnectionsPerSecond float64 `json:"connectionsPerSecond"`
	// Burst is how many connections a client may open at once before the
	// rate applies. Default: the rate rounded up, and at least 1.
	Burst int `json:"burst"`
	// MaxTrackedClients bounds the limiter's memory. Each tracked client
	// costs on the order of 200 bytes. Default 65536.
	MaxTrackedClients int `json:"maxTrackedClients"`
}

// DefaultMaxTrackedClients is the LRU size when none is configured.
const DefaultMaxTrackedClients = 65536

// Validate applies defaults and rejects impossible values. prefix is the JSON
// path of this block, such as "limits", used in errors.
func (c *Config) Validate(prefix string) error {
	if c.MaxConnections < 0 {
		return config.Invalid(prefix+".maxConnections", "must not be negative, got %d", c.MaxConnections)
	}
	return c.PerClient.Validate(prefix + ".perClient")
}

// Validate applies defaults and rejects impossible values.
func (c *PerClientConfig) Validate(prefix string) error {
	if c.ConnectionsPerSecond < 0 || math.IsNaN(c.ConnectionsPerSecond) || math.IsInf(c.ConnectionsPerSecond, 0) {
		return config.Invalid(prefix+".connectionsPerSecond", "must be a non-negative number, got %v", c.ConnectionsPerSecond)
	}
	if c.Burst < 0 {
		return config.Invalid(prefix+".burst", "must not be negative, got %d", c.Burst)
	}
	if c.MaxTrackedClients < 0 {
		return config.Invalid(prefix+".maxTrackedClients", "must not be negative, got %d", c.MaxTrackedClients)
	}
	if c.ConnectionsPerSecond == 0 {
		return nil
	}
	if c.Burst == 0 {
		c.Burst = max(1, int(math.Ceil(c.ConnectionsPerSecond)))
	}
	if c.MaxTrackedClients == 0 {
		c.MaxTrackedClients = DefaultMaxTrackedClients
	}
	return nil
}

// Enabled reports whether the per-client limiter is configured at all.
func (c PerClientConfig) Enabled() bool {
	return c.ConnectionsPerSecond > 0
}
