package serving

import (
	"errors"
	"testing"
	"time"

	"github.com/devarashs/sluice/internal/config"
	"github.com/devarashs/sluice/internal/relay"
)

func TestValidateDefaults(t *testing.T) {
	var cfg Config
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.DialTimeout.Duration() != DefaultDialTimeout || cfg.IdleTimeout.Duration() != DefaultIdleTimeout {
		t.Errorf("timeouts = %v / %v", cfg.DialTimeout, cfg.IdleTimeout)
	}
	if cfg.BufferSize != config.ByteSize(relay.DefaultBufferSize) {
		t.Errorf("bufferSize = %v", cfg.BufferSize)
	}
	if cfg.Listener.Acceptors == 0 || cfg.Listener.DrainTimeout == 0 {
		t.Errorf("listener defaults not applied: %+v", cfg.Listener)
	}
	if got := cfg.RelayOptions(); got.IdleTimeout != DefaultIdleTimeout || got.BufferSize != relay.DefaultBufferSize {
		t.Errorf("relay options = %+v", got)
	}
	cfg.NoIdleTimeout = true
	if cfg.RelayOptions().IdleTimeout != 0 {
		t.Error("noIdleTimeout should disable the relay idle timeout")
	}
	dialer := cfg.Dialer()
	if dialer.Timeout != DefaultDialTimeout || !dialer.KeepAliveConfig.Enable || dialer.KeepAliveConfig.Idle != cfg.Listener.KeepAliveIdle.Duration() {
		t.Errorf("dialer = %+v", dialer)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Config)
		field  string
	}{
		"idle too short":    {func(c *Config) { c.IdleTimeout = config.Duration(100 * time.Millisecond) }, "idleTimeout"},
		"buffer too small":  {func(c *Config) { c.BufferSize = 100 }, "bufferSize"},
		"nested limits":     {func(c *Config) { c.Limits.MaxConnections = -5 }, "limits.maxConnections"},
		"nested per-client": {func(c *Config) { c.Limits.PerClient.ConnectionsPerSecond = -1 }, "limits.perClient.connectionsPerSecond"},
		"nested listener":   {func(c *Config) { c.Listener.Acceptors = -1 }, "listener.acceptors"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var cfg Config
			c.mutate(&cfg)
			err := cfg.Validate()
			var fieldErr *config.FieldError
			if !errors.As(err, &fieldErr) || fieldErr.Field != c.field {
				t.Fatalf("err = %v, want FieldError on %s", err, c.field)
			}
		})
	}
}
