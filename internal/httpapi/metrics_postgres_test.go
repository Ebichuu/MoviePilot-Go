package httpapi

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestRegisterPostgresPoolMetricsHandlesUnavailableDatabase(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	registerPostgresPoolMetrics(registry, nil)

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	want := map[string]bool{
		"moviepilot_postgres_pool_acquired_connections":     false,
		"moviepilot_postgres_pool_idle_connections":         false,
		"moviepilot_postgres_pool_constructing_connections": false,
		"moviepilot_postgres_pool_total_connections":        false,
		"moviepilot_postgres_pool_max_connections":          false,
	}
	for _, family := range families {
		name := family.GetName()
		if _, ok := want[name]; !ok {
			continue
		}
		want[name] = true
		metrics := family.GetMetric()
		if len(metrics) != 1 || metrics[0].GetGauge().GetValue() != 0 {
			t.Fatalf("%s = %#v, want one zero gauge", name, metrics)
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("metric %s was not registered", name)
		}
	}
}
