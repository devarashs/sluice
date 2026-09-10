package metrics

import (
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// gather flattens a registry into family name -> sorted label string ->
// value, which is what assertions want.
func gather(t *testing.T, registry prometheus.Gatherer) map[string]map[string]float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]float64{}
	for _, family := range families {
		series := map[string]float64{}
		for _, metric := range family.GetMetric() {
			var labels []string
			for _, pair := range metric.GetLabel() {
				labels = append(labels, pair.GetName()+`="`+pair.GetValue()+`"`)
			}
			sort.Strings(labels)
			var v float64
			switch {
			case metric.GetCounter() != nil:
				v = metric.GetCounter().GetValue()
			case metric.GetGauge() != nil:
				v = metric.GetGauge().GetValue()
			}
			series[strings.Join(labels, ",")] = v
		}
		out[family.GetName()] = series
	}
	return out
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
