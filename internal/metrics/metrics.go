// Package metrics owns the Prometheus registry every mode reports into.
//
// A private registry rather than the client library's default one, so tests
// can build as many as they like and nothing registers behind our back. The
// Go runtime and process collectors are included because operators cannot
// size a host for millions of connections without goroutine counts, heap
// size, GC pause times, and open file descriptors.
package metrics

import (
	"runtime"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/devarashs/sluice/internal/version"
)

// Namespace prefixes every metric sluice defines.
const Namespace = "sluice"

// NewRegistry returns a registry carrying the Go runtime and process
// collectors and a sluice_build_info gauge. Modes register their own
// collectors on it.
func NewRegistry() *prometheus.Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo(),
	)
	return registry
}

// buildInfo is the conventional constant gauge that lets dashboards show
// which version is running where.
func buildInfo() prometheus.Collector {
	gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: Namespace,
		Name:      "build_info",
		Help:      "Build identity of the running binary. Always 1.",
	}, []string{"version", "commit", "goversion"})
	gauge.WithLabelValues(version.Version, version.Commit, runtime.Version()).Set(1)
	return gauge
}
