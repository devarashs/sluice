package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/devarashs/sluice/internal/config"
)

func TestValidateAppliesDefaults(t *testing.T) {
	var cfg Config
	if err := cfg.Validate("log"); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Level != "info" || cfg.Format != "text" {
		t.Fatalf("defaults = %+v, want info/text", cfg)
	}
	if cfg.Threshold() != slog.LevelInfo {
		t.Fatalf("Threshold() = %v, want info", cfg.Threshold())
	}
}

func TestValidateRejectsUnknownNames(t *testing.T) {
	for name, cfg := range map[string]Config{
		"level":  {Level: "loud"},
		"format": {Format: "yaml"},
	} {
		t.Run(name, func(t *testing.T) {
			err := cfg.Validate("log")
			var fieldErr *config.FieldError
			if !errors.As(err, &fieldErr) {
				t.Fatalf("err = %v, want a FieldError", err)
			}
			if fieldErr.Field != "log."+name {
				t.Fatalf("field = %q, want log.%s", fieldErr.Field, name)
			}
		})
	}
}

func TestNewFiltersBelowLevelAndHonoursFormat(t *testing.T) {
	var out bytes.Buffer
	cfg := Config{Level: "info", Format: "json"}
	if err := cfg.Validate("log"); err != nil {
		t.Fatal(err)
	}
	logger := New(cfg, &out)
	logger.Debug("hidden", "k", 1)
	logger.Info("shown", "peer", "10.0.0.1")

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1 (debug suppressed):\n%s", len(lines), out.String())
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("line is not JSON: %v\n%s", err, lines[0])
	}
	if record["msg"] != "shown" || record["peer"] != "10.0.0.1" || record["level"] != "INFO" {
		t.Fatalf("record = %v", record)
	}
}

func TestNewTextFormatIsKeyValue(t *testing.T) {
	var out bytes.Buffer
	cfg := Config{Level: "debug"}
	if err := cfg.Validate("log"); err != nil {
		t.Fatal(err)
	}
	New(cfg, &out).Debug("visible", "count", 3)
	if !strings.Contains(out.String(), "level=DEBUG") || !strings.Contains(out.String(), "count=3") {
		t.Fatalf("text output = %q", out.String())
	}
}
