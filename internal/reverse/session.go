package reverse

import (
	"bytes"
	"io"
	"log/slog"
	"time"

	"github.com/hashicorp/yamux"
)

// logWriter routes yamux's line-oriented internal logging into slog at debug
// level, so it lands in the same place and format as everything else and does
// not go to stderr on its own.
type logWriter struct {
	logger *slog.Logger
}

func (w logWriter) Write(p []byte) (int, error) {
	w.logger.Debug("yamux", "detail", string(bytes.TrimRight(p, "\n")))
	return len(p), nil
}

// yamux tuning shared by both ends of a session. The window is large because
// one session carries every tunnelled connection for a client, so a small
// window would cap aggregate throughput; keepalive detects a peer that has
// gone away without a FIN so a dead session does not linger.
const (
	sessionWindowSize      = 1 << 20 // 1MiB per stream
	sessionKeepAlive       = 30 * time.Second
	sessionWriteTimeout    = 15 * time.Second
	sessionOpenStreamLimit = 15 * time.Second
)

// sessionConfig builds the yamux configuration, sending yamux's own logging
// to the same place as the rest of the process rather than the default
// stderr writer.
func sessionConfig(logOutput io.Writer) *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = sessionKeepAlive
	cfg.ConnectionWriteTimeout = sessionWriteTimeout
	cfg.MaxStreamWindowSize = sessionWindowSize
	cfg.StreamOpenTimeout = sessionOpenStreamLimit
	cfg.LogOutput = logOutput
	cfg.Logger = nil
	return cfg
}
