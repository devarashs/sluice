package metrics

import (
	"context"
	"errors"
	"testing"

	"github.com/devarashs/sluice/internal/relay"
)

// value finds one sample by family name and label set on a registry.
func value(t *testing.T, families map[string]map[string]float64, family string, labels string) float64 {
	t.Helper()
	series, ok := families[family]
	if !ok {
		t.Fatalf("family %s not gathered; have %v", family, keys(families))
	}
	v, ok := series[labels]
	if !ok {
		t.Fatalf("family %s has no series %q; have %v", family, labels, keys(series))
	}
	return v
}

func TestConnectionsRegistersEveryFamilyWithModeLabel(t *testing.T) {
	registry := NewRegistry()
	accepted, active, rejected := uint64(7), 3, uint64(2)
	conns := NewConnections(registry, "forward", ConnectionSources{
		Accepted:       func() uint64 { return accepted },
		Active:         func() int { return active },
		RejectedByRate: func() uint64 { return rejected },
	})
	conns.DialFailed()
	conns.RecordRelay(relay.Result{BytesAToB: 100, BytesBToA: 250})
	conns.RecordRelay(relay.Result{BytesAToB: 1, Err: relay.ErrIdle})

	families := gather(t, registry)
	if got := value(t, families, "sluice_connections_accepted_total", `mode="forward"`); got != 7 {
		t.Errorf("accepted = %v", got)
	}
	if got := value(t, families, "sluice_connections_active", `mode="forward"`); got != 3 {
		t.Errorf("active = %v", got)
	}
	if got := value(t, families, "sluice_connections_rejected_total", `mode="forward",reason="rate_limit"`); got != 2 {
		t.Errorf("rejected = %v", got)
	}
	if got := value(t, families, "sluice_dial_failures_total", `mode="forward"`); got != 1 {
		t.Errorf("dial failures = %v", got)
	}
	if got := value(t, families, "sluice_relay_bytes_total", `direction="upstream",mode="forward"`); got != 101 {
		t.Errorf("upstream bytes = %v", got)
	}
	if got := value(t, families, "sluice_relay_bytes_total", `direction="downstream",mode="forward"`); got != 250 {
		t.Errorf("downstream bytes = %v", got)
	}
	if got := value(t, families, "sluice_relay_ended_total", `cause="clean",mode="forward"`); got != 1 {
		t.Errorf("clean relays = %v", got)
	}
	if got := value(t, families, "sluice_relay_ended_total", `cause="idle",mode="forward"`); got != 1 {
		t.Errorf("idle relays = %v", got)
	}
	// Pre-created series show as zero rather than being absent.
	if got := value(t, families, "sluice_relay_ended_total", `cause="error",mode="forward"`); got != 0 {
		t.Errorf("error relays = %v, want 0", got)
	}
}

func TestRelayCause(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"nil":      {nil, CauseClean},
		"idle":     {relay.ErrIdle, CauseIdle},
		"canceled": {context.Canceled, CauseShutdown},
		"deadline": {context.DeadlineExceeded, CauseShutdown},
		"other":    {errors.New("connection reset"), CauseError},
	}
	for name, c := range cases {
		if got := RelayCause(c.err); got != c.want {
			t.Errorf("%s: cause = %q, want %q", name, got, c.want)
		}
	}
}
