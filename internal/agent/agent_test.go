package agent

import (
	"testing"
	"time"
)

func TestParseByteSize(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"0", 0, false},
		{"1024", 1024, false},
		{"1Ki", 1 << 10, false},
		{"64Mi", 64 << 20, false},
		{"1Gi", 1 << 30, false},
		{"2Ti", 2 << 40, false},
		{"5K", 5 * 1000, false},
		{"5M", 5 * 1000 * 1000, false},
		{"5G", 5 * 1000 * 1000 * 1000, false},
		{"5T", 5 * 1000 * 1000 * 1000 * 1000, false},
		{"3k", 3 * 1000, false},
		{"3m", 3 * 1000 * 1000, false},
		{"3g", 3 * 1000 * 1000 * 1000, false},
		{"1.5Gi", int64(1.5 * float64(int64(1)<<30)), false},
		{"", 0, true},
		{"Mi", 0, true},
		{"abc", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseByteSize(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseByteSize(%q) = %d, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseByteSize(%q): unexpected error %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseByteSize(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestQuotaOrZero pins the conversion that keeps "no quota" (a negative value)
// from being exported as a negative gauge.
func TestQuotaOrZero(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want int64
	}{
		{-1, 0},
		{-4096, 0},
		{0, 0},
		{100, 100},
		{1 << 40, 1 << 40},
	} {
		if got := quotaOrZero(tc.in); got != tc.want {
			t.Fatalf("quotaOrZero(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestAbs64(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want int64
	}{
		{0, 0},
		{5, 5},
		{-5, 5},
		{1 << 40, 1 << 40},
		{-(1 << 40), 1 << 40},
	} {
		if got := abs64(tc.in); got != tc.want {
			t.Fatalf("abs64(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestDetectedNodeName pins the fallback order that keeps the agent's `node`
// label equal to the manager's: inside a pod with hostNetwork false the kernel
// hostname is the pod name, so NODE_NAME from the downward API must win.
func TestDetectedNodeName(t *testing.T) {
	t.Setenv("NODE_NAME", "node-1")
	if got := detectedNodeName(); got != "node-1" {
		t.Fatalf("detectedNodeName() = %q, want node-1", got)
	}

	t.Setenv("NODE_NAME", "   ")
	got := detectedNodeName()
	if got == "" || got == "   " {
		t.Fatalf("blank NODE_NAME should fall back to the hostname, got %q", got)
	}

	t.Setenv("NODE_NAME", "")
	if got := detectedNodeName(); got == "" {
		t.Fatal("empty NODE_NAME should fall back to the hostname")
	}
}

func TestNewResolvesNodeName(t *testing.T) {
	t.Setenv("NODE_NAME", "node-7")
	a := New(Options{})
	if a.opts.NodeName != "node-7" {
		t.Fatalf("Options.NodeName = %q, want node-7", a.opts.NodeName)
	}
	if a.node != "node-7" {
		t.Fatalf("agent node = %q, want node-7", a.node)
	}

	b := New(Options{NodeName: "explicit"})
	if b.node != "explicit" {
		t.Fatalf("explicit NodeName was overridden: %q", b.node)
	}
}

// TestNewDefaults documents the values the DaemonSet relies on when it passes
// no flags at all.
func TestNewDefaults(t *testing.T) {
	a := New(Options{})
	if a.opts.PodsDir != "/var/lib/kubelet/pods" {
		t.Errorf("PodsDir = %q, want /var/lib/kubelet/pods", a.opts.PodsDir)
	}
	if a.opts.Interval != 5*time.Minute {
		t.Errorf("Interval = %v, want 5m", a.opts.Interval)
	}
	if a.opts.ProbeTimeout != 30*time.Second {
		t.Errorf("ProbeTimeout = %v, want 30s", a.opts.ProbeTimeout)
	}
	if a.opts.ListenAddr != ":9101" {
		t.Errorf("ListenAddr = %q, want :9101", a.opts.ListenAddr)
	}
	if a.log == nil {
		t.Error("Logger was not defaulted")
	}
	if a.Samples() == nil {
		t.Error("Samples() must return an empty slice, not nil")
	}
}
