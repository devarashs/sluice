package metrics

import (
	"context"
	"errors"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/devarashs/sluice/internal/relay"
)

// ConnectionSources are the live counters a mode's listener already keeps,
// read on scrape so the hot path pays for nothing.
type ConnectionSources struct {
	Accepted       func() uint64
	Active         func() int
	RejectedByRate func() uint64
}

// Connections is the metric set every connection-handling mode reports.
// Metric names are shared across modes with a constant `mode` label, so one
// dashboard covers every kind of sluice process.
type Connections struct {
	dialFailures      prometheus.Counter
	handshakeFailures prometheus.Counter
	bytes             *prometheus.CounterVec
	relayEnded        *prometheus.CounterVec
}

// Direction labels for relay_bytes_total. Upstream is client to target.
const (
	DirectionUpstream   = "upstream"
	DirectionDownstream = "downstream"
)

// Cause labels for relay_ended_total.
const (
	CauseClean    = "clean"
	CauseIdle     = "idle"
	CauseShutdown = "shutdown"
	CauseError    = "error"
)

// NewConnections registers the connection metrics for mode on registry and
// returns the handle a mode uses to record relays.
func NewConnections(registry prometheus.Registerer, mode string, sources ConnectionSources) *Connections {
	modeLabel := prometheus.Labels{"mode": mode}
	registry.MustRegister(
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Namespace: Namespace, Name: "connections_accepted_total", ConstLabels: modeLabel,
			Help: "Connections accepted and handed to a handler.",
		}, func() float64 { return float64(sources.Accepted()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: Namespace, Name: "connections_active", ConstLabels: modeLabel,
			Help: "Connections being handled right now.",
		}, func() float64 { return float64(sources.Active()) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Namespace: Namespace, Name: "connections_rejected_total",
			ConstLabels: prometheus.Labels{"mode": mode, "reason": "rate_limit"},
			Help:        "Connections closed at accept because the client exceeded its rate.",
		}, func() float64 { return float64(sources.RejectedByRate()) }),
	)

	c := &Connections{
		dialFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Name: "dial_failures_total", ConstLabels: modeLabel,
			Help: "Accepted connections dropped because the next hop could not be reached or refused the handshake.",
		}),
		handshakeFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Name: "tls_handshake_failures_total", ConstLabels: modeLabel,
			Help: "Accepted connections dropped because the client's TLS handshake failed or timed out.",
		}),
		bytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Name: "relay_bytes_total", ConstLabels: modeLabel,
			Help: "Bytes relayed, by direction; upstream is client to target.",
		}, []string{"direction"}),
		relayEnded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Name: "relay_ended_total", ConstLabels: modeLabel,
			Help: "Relays finished, by cause.",
		}, []string{"cause"}),
	}
	registry.MustRegister(c.dialFailures, c.handshakeFailures, c.bytes, c.relayEnded)

	// Create every label combination up front so dashboards see zeros rather
	// than absent series before the first event.
	for _, direction := range []string{DirectionUpstream, DirectionDownstream} {
		c.bytes.WithLabelValues(direction)
	}
	for _, cause := range []string{CauseClean, CauseIdle, CauseShutdown, CauseError} {
		c.relayEnded.WithLabelValues(cause)
	}
	return c
}

// DialFailed records an accepted connection that never reached its next hop.
func (c *Connections) DialFailed() {
	c.dialFailures.Inc()
}

// HandshakeFailed records an accepted connection whose TLS handshake failed.
func (c *Connections) HandshakeFailed() {
	c.handshakeFailures.Inc()
}

// RecordRelay records a finished relay whose A side was the client and B
// side the target.
func (c *Connections) RecordRelay(result relay.Result) {
	c.bytes.WithLabelValues(DirectionUpstream).Add(float64(result.BytesAToB))
	c.bytes.WithLabelValues(DirectionDownstream).Add(float64(result.BytesBToA))
	c.relayEnded.WithLabelValues(RelayCause(result.Err)).Inc()
}

// RelayCause maps a relay's ending error onto the cause label.
func RelayCause(err error) string {
	switch {
	case err == nil:
		return CauseClean
	case errors.Is(err, relay.ErrIdle):
		return CauseIdle
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return CauseShutdown
	default:
		return CauseError
	}
}
