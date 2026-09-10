// Package logging builds the process-wide slog.Logger from configuration.
//
// Output is text by default and JSON on request: a systemd journal reads text
// well and a log shipper wants JSON. Every package logs per-connection events
// at Debug only. At the connection counts sluice is built for, per-connection
// logging at Info would be the bottleneck, so Info is reserved for events
// that happen once per process or once per peer.
package logging

import (
	"io"
	"log/slog"

	"github.com/devarashs/sluice/internal/config"
)

// Config is the `log` block shared by every mode's configuration file.
type Config struct {
	// Level is "debug", "info", "warn", or "error". Default "info".
	Level string `json:"level"`
	// Format is "text" or "json". Default "text".
	Format string `json:"format"`
}

// Defaults applied by Validate when a field is omitted.
const (
	DefaultLevel  = "info"
	DefaultFormat = "text"
)

var levels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// Validate applies defaults and rejects unknown level or format names.
// prefix is the JSON path of this block, such as "log", used in errors.
func (c *Config) Validate(prefix string) error {
	if c.Level == "" {
		c.Level = DefaultLevel
	}
	if _, known := levels[c.Level]; !known {
		return config.Invalid(prefix+".level", "%q is not one of debug, info, warn, error", c.Level)
	}
	if c.Format == "" {
		c.Format = DefaultFormat
	}
	if c.Format != "text" && c.Format != "json" {
		return config.Invalid(prefix+".format", "%q is not one of text, json", c.Format)
	}
	return nil
}

// Threshold returns the configured level as slog understands it. Call
// Validate first; an unknown name falls back to Info rather than panicking.
func (c Config) Threshold() slog.Level {
	if level, known := levels[c.Level]; known {
		return level
	}
	return slog.LevelInfo
}

// New builds a logger writing to w in the configured format at the configured
// level. cfg must have passed Validate.
func New(cfg Config, w io.Writer) *slog.Logger {
	options := &slog.HandlerOptions{Level: cfg.Threshold()}
	if cfg.Format == "json" {
		return slog.New(slog.NewJSONHandler(w, options))
	}
	return slog.New(slog.NewTextHandler(w, options))
}
