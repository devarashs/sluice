package process

import (
	"errors"
	"io"
	"log/slog"
	"math"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/devarashs/sluice/internal/config"
)

func TestValidate(t *testing.T) {
	valid := []Config{
		{},
		{MemoryLimit: 512 * config.MiB},
		{GCPercent: 50},
		{MemoryLimit: 2 * config.GiB, GCPercent: -1},
	}
	for _, cfg := range valid {
		if err := cfg.Validate("process"); err != nil {
			t.Errorf("%+v: unexpected error %v", cfg, err)
		}
	}

	invalid := map[string]struct {
		cfg   Config
		field string
	}{
		"negative memory":        {Config{MemoryLimit: -1}, "process.memoryLimit"},
		"tiny memory":            {Config{MemoryLimit: 1024}, "process.memoryLimit"},
		"gc below -1":            {Config{GCPercent: -2}, "process.gcPercent"},
		"gc off without a limit": {Config{GCPercent: -1}, "process.gcPercent"},
	}
	for name, c := range invalid {
		t.Run(name, func(t *testing.T) {
			err := c.cfg.Validate("process")
			var fieldErr *config.FieldError
			if !errors.As(err, &fieldErr) {
				t.Fatalf("err = %v, want a FieldError", err)
			}
			if fieldErr.Field != c.field {
				t.Fatalf("field = %q, want %q", fieldErr.Field, c.field)
			}
		})
	}
}

// restoreRuntime puts the collector settings back however the test left them.
func restoreRuntime(t *testing.T) {
	t.Helper()
	memoryLimit := debug.SetMemoryLimit(-1)
	gcPercent := readGCPercent()
	t.Cleanup(func() {
		debug.SetMemoryLimit(memoryLimit)
		debug.SetGCPercent(gcPercent)
	})
}

func TestApplySetsMemoryLimitAndGCPercent(t *testing.T) {
	restoreRuntime(t)
	report := Apply(Config{MemoryLimit: 256 * config.MiB, GCPercent: 25}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if report.MemoryLimit != int64(256*config.MiB) {
		t.Errorf("MemoryLimit = %d, want %d", report.MemoryLimit, 256*config.MiB)
	}
	if got := debug.SetMemoryLimit(-1); got != int64(256*config.MiB) {
		t.Errorf("runtime memory limit = %d, want %d", got, 256*config.MiB)
	}
	if report.GCPercent != 25 {
		t.Errorf("GCPercent = %d, want 25", report.GCPercent)
	}
}

func TestApplyWithZeroConfigLeavesRuntimeAlone(t *testing.T) {
	restoreRuntime(t)
	before := Report{MemoryLimit: debug.SetMemoryLimit(-1), GCPercent: readGCPercent()}
	report := Apply(Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if report.MemoryLimit != before.MemoryLimit || report.GCPercent != before.GCPercent {
		t.Errorf("Apply changed settings: before %+v, after %+v", before, report)
	}
}

func TestReadFileLimitMatchesPlatform(t *testing.T) {
	limit := readFileLimit()
	if runtime.GOOS == "windows" {
		if limit.Supported {
			t.Fatalf("windows reported a file limit: %+v", limit)
		}
		return
	}
	if !limit.Supported || limit.Soft == 0 || limit.Hard < limit.Soft {
		t.Fatalf("unix file limit looks wrong: %+v", limit)
	}
}

func TestDescribeMemoryLimit(t *testing.T) {
	if got := describeMemoryLimit(math.MaxInt64); got != "none" {
		t.Errorf("no limit = %q, want none", got)
	}
	if got := describeMemoryLimit(int64(2 * config.GiB)); got != "2GiB" {
		t.Errorf("2GiB = %q", got)
	}
}
