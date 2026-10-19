package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestRegisterExposesManagerMetrics guards a failure mode that looked healthy
// from every other angle: the manager served controller-runtime's registry
// while every cephfs_guard_* collector was registered in prometheus' default
// registry. The pod was Ready, the reports were correct, and :8080 exported
// nothing the alert rules could match.
func TestRegisterExposesManagerMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// A vec with no label values is not gathered at all, so give it one.
	VolumeTrueBytes.WithLabelValues("pv", "ns", "pvc", "node", "class").Set(42)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	names := make([]string, 0, len(families))
	found := false
	for _, mf := range families {
		names = append(names, mf.GetName())
		if mf.GetName() == "cephfs_guard_volume_true_used_bytes" {
			found = true
		}
	}
	if !found {
		t.Fatalf("registry has no cephfs_guard_volume_true_used_bytes; gathered: %s", strings.Join(names, ", "))
	}

	// Start-up must not depend on Register being called exactly once.
	if err := Register(reg); err != nil {
		t.Fatalf("second Register on the same registry: %v", err)
	}
}
