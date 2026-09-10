package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestThrottleLogsOncePerIntervalAndCountsTheRest(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&out, nil))
	clock := time.Unix(1_700_000_000, 0)
	throttle := NewThrottle(time.Second)
	throttle.now = func() time.Time { return clock }

	throttle.Log(logger, slog.LevelWarn, "target down", "target", "10.0.0.5:80")
	for i := 0; i < 5; i++ {
		clock = clock.Add(100 * time.Millisecond)
		throttle.Log(logger, slog.LevelWarn, "target down", "target", "10.0.0.5:80")
	}
	clock = clock.Add(time.Second)
	throttle.Log(logger, slog.LevelWarn, "target down", "target", "10.0.0.5:80")

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), out.String())
	}
	if strings.Contains(lines[0], "suppressed") {
		t.Errorf("first line should not report suppression: %s", lines[0])
	}
	if !strings.Contains(lines[1], "suppressed=5") {
		t.Errorf("second line should report 5 suppressed: %s", lines[1])
	}
	if !strings.Contains(lines[1], "level=WARN") || !strings.Contains(lines[1], "target=10.0.0.5:80") {
		t.Errorf("attributes lost: %s", lines[1])
	}
}
