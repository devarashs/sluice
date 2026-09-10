// Package process applies the Go runtime settings that decide how many
// connections one sluice process can hold, and reports the operating-system
// limits that cap it.
package process

import (
	"log/slog"
	"math"
	"runtime/debug"

	"github.com/devarashs/sluice/internal/config"
)

// Config is the `process` block shared by every mode's configuration file.
type Config struct {
	// MemoryLimit is the soft heap limit handed to the Go runtime. As the heap
	// nears it the collector works harder instead of letting the kernel kill
	// the process. Zero leaves the runtime default, which is no limit.
	MemoryLimit config.ByteSize `json:"memoryLimit"`
	// GCPercent is the GOGC value. Zero keeps the Go default of 100. -1 turns
	// off proportional collection so the collector runs only as the memory
	// limit is approached, which is the intended pairing at scale: fewer
	// collections per connection, bounded by the limit.
	GCPercent int `json:"gcPercent"`
}

// MinimumMemoryLimit guards against a limit that was meant as megabytes but
// written as bytes. Below this the collector would run almost continuously.
const MinimumMemoryLimit = 16 * config.MiB

// LowFileLimit is the open-file soft limit below which Apply warns. One
// relayed connection costs two descriptors, so 65536 is roughly 32k
// connections, far under what this process is built for.
const LowFileLimit = 65536

// Validate rejects settings that cannot be applied safely. prefix is the JSON
// path of this block, such as "process", used in errors.
func (c *Config) Validate(prefix string) error {
	if c.MemoryLimit < 0 {
		return config.Invalid(prefix+".memoryLimit", "must not be negative")
	}
	if c.MemoryLimit > 0 && c.MemoryLimit < MinimumMemoryLimit {
		return config.Invalid(prefix+".memoryLimit", "%s is below the %s minimum; a limit that small keeps the collector running constantly", c.MemoryLimit, MinimumMemoryLimit)
	}
	if c.GCPercent < -1 {
		return config.Invalid(prefix+".gcPercent", "must be -1 or at least 0, got %d", c.GCPercent)
	}
	if c.GCPercent == -1 && c.MemoryLimit == 0 {
		return config.Invalid(prefix+".gcPercent", "-1 turns the collector off, which is only safe with memoryLimit set")
	}
	return nil
}

// FileLimit is the process's RLIMIT_NOFILE as reported by the kernel.
type FileLimit struct {
	// Supported is false where the platform has no such limit to read.
	Supported bool
	Soft      uint64
	Hard      uint64
}

// Report says what Apply found and set, for the startup log and for tests.
type Report struct {
	FileLimit FileLimit
	// MemoryLimit is the limit in force after Apply. math.MaxInt64 means none.
	MemoryLimit int64
	// GCPercent is the value in force after Apply.
	GCPercent int
}

// Apply sets the runtime limits from cfg, reads the open-file limit, and logs
// both so the startup log states the capacity this process actually has.
// cfg must have passed Validate.
//
// The open-file soft limit is read, not raised: since Go 1.19 the runtime
// raises it to the hard limit on its own at startup. What remains is telling
// the operator when the hard limit itself is too low for the job, which only
// systemd's LimitNOFILE or the shell's ulimit can fix.
func Apply(cfg Config, logger *slog.Logger) Report {
	var report Report

	report.FileLimit = readFileLimit()
	switch {
	case !report.FileLimit.Supported:
		logger.Debug("open-file limit is not adjustable on this platform")
	case report.FileLimit.Soft < LowFileLimit:
		logger.Warn("open-file limit is low; each relayed connection needs two descriptors. Raise it with systemd LimitNOFILE or ulimit -n",
			"soft", report.FileLimit.Soft, "hard", report.FileLimit.Hard)
	default:
		logger.Info("open-file limit", "soft", report.FileLimit.Soft, "hard", report.FileLimit.Hard)
	}

	if cfg.MemoryLimit > 0 {
		debug.SetMemoryLimit(cfg.MemoryLimit.Bytes())
	}
	// A negative argument reads the current limit without changing it.
	report.MemoryLimit = debug.SetMemoryLimit(-1)

	if cfg.GCPercent != 0 {
		debug.SetGCPercent(cfg.GCPercent)
	}
	report.GCPercent = readGCPercent()

	logger.Info("go runtime", "memoryLimit", describeMemoryLimit(report.MemoryLimit), "gcPercent", report.GCPercent)
	return report
}

// readGCPercent returns the GOGC value in force. The runtime offers no
// read-only query, only SetGCPercent which returns the previous value, so
// this sets a value and immediately restores it. Apply runs once at startup
// before any traffic, so the momentary change has no effect.
func readGCPercent() int {
	current := debug.SetGCPercent(100)
	debug.SetGCPercent(current)
	return current
}

// describeMemoryLimit renders the limit for the startup log, naming the
// no-limit sentinel rather than printing a number nobody recognises.
func describeMemoryLimit(limit int64) string {
	if limit == math.MaxInt64 {
		return "none"
	}
	return config.ByteSize(limit).String()
}
