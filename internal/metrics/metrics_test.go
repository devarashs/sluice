package metrics

import (
	"testing"

	"github.com/devarashs/sluice/internal/version"
)

func TestNewRegistryGathersBuildInfoAndRuntimeCollectors(t *testing.T) {
	families, err := NewRegistry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	byName := map[string]bool{}
	for _, family := range families {
		byName[family.GetName()] = true
		if family.GetName() == "sluice_build_info" {
			labels := map[string]string{}
			for _, pair := range family.GetMetric()[0].GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			if labels["version"] != version.Version || labels["commit"] != version.Commit {
				t.Errorf("build_info labels = %v, want version %q commit %q", labels, version.Version, version.Commit)
			}
			if family.GetMetric()[0].GetGauge().GetValue() != 1 {
				t.Errorf("build_info value = %v, want 1", family.GetMetric()[0].GetGauge().GetValue())
			}
		}
	}
	for _, want := range []string{"sluice_build_info", "go_goroutines", "go_memstats_heap_alloc_bytes"} {
		if !byName[want] {
			t.Errorf("registry is missing %s", want)
		}
	}
}

func TestEachRegistryIsIndependent(t *testing.T) {
	// Two registries must not collide, which is what a shared default registry
	// would do on the second MustRegister.
	NewRegistry()
	NewRegistry()
}
