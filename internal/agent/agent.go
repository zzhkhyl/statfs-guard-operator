// Package agent implements the per-node half of statfs-guard.
//
// It runs as a DaemonSet with /var/lib/kubelet/pods bind-mounted read-only.
// On every tick it walks the kubelet volume directories, and for each CephFS
// volume mount it takes two measurements of the same path:
//
//	statfs(2)              -> what kubelet reports to Prometheus
//	ceph.dir.rbytes xattr  -> what the MDS says is actually in the subvolume
//
// Taking the second measurement is not passive observation: the attribute read
// round-trips to the authoritative MDS (CEPH_STAT_RSTAT) and writes the answer
// back into the kernel client's cached inode, which is exactly what makes the
// next statfs(2) — and therefore kubelet_volume_stats_used_bytes — correct.
// The repair is a read. Nothing is written, no quota is touched, no mount is
// bounced, and the volume is never evicted, so there is no data path to break.
//
// The agent exposes its samples over HTTP so the operator can ask one specific
// node for ground truth, and as Prometheus metrics for direct alerting.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/zzhkhyl/statfs-guard-operator/internal/cephfs"
	"github.com/zzhkhyl/statfs-guard-operator/internal/metrics"
)

// Options configures an Agent.
type Options struct {
	// PodsDir is the host path holding kubelet's pod volume directories.
	// Defaults to /var/lib/kubelet/pods (the DaemonSet binds it read-only).
	PodsDir string
	// Interval is how often every visible volume is probed. Each probe is one
	// MDS statx per volume, so do not make this aggressive.
	Interval time.Duration
	// ListenAddr is the address of the agent's HTTP endpoints.
	ListenAddr string
	// ProbeTimeout bounds a single probe.
	ProbeTimeout time.Duration
	// NodeName is the Kubernetes node this agent runs on, stamped on every
	// sample and series. Defaults to detectedNodeName().
	NodeName string
	// Logger receives structured logs.
	Logger *slog.Logger
}

// Sample is one volume's measurement on one node.
type Sample struct {
	PV   string `json:"pv"`
	Node string `json:"node"`
	Path string `json:"path"`

	// Truth: ceph.dir.rbytes family.
	RBytes int64 `json:"rbytes"`
	RFiles int64 `json:"rfiles"`
	Quota  int64 `json:"quotaMaxBytes"`

	// kubeletView: statfs(2) on the exact path kubelet measures.
	StatfsUsed     int64 `json:"statfsUsedBytes"`
	StatfsCapacity int64 `json:"statfsCapacityBytes"`
	StatfsAvail    int64 `json:"statfsAvailableBytes"`

	// Drift is RBytes - StatfsUsed. Positive means statfs under-reports.
	Drift int64 `json:"driftBytes"`

	// DriftEpisodes counts how many distinct times this volume was caught
	// reporting a stale figure on this node.
	DriftEpisodes int64 `json:"driftEpisodes"`

	// WorstDriftBytes is the largest drift seen in the current or most recent episode.
	WorstDriftBytes int64 `json:"worstDriftBytes"`

	// StaleSeconds is how long the kubelet-facing figure has been known wrong
	// in the current episode; zero when in sync.
	StaleSeconds float64 `json:"staleSeconds"`

	// StaleSince is when the current episode started, if one is open.
	StaleSince *time.Time `json:"staleSince,omitempty"`

	// Duration is how long the rstat read took (MDS round trip).
	Duration time.Duration `json:"duration"`

	// ProbedAt is when the sample was taken.
	ProbedAt time.Time `json:"probedAt"`

	// Error is empty on success.
	Error string `json:"error,omitempty"`
}

// Agent is the node-local collector.
type Agent struct {
	opts Options
	log  *slog.Logger

	mu      sync.RWMutex
	samples map[string]Sample // keyed by PV name
	paths   map[string]string // PV -> mount path, from the last discovery pass

	// Drift episode bookkeeping, also keyed by PV name.
	episodes     map[string]int64
	episodeStart map[string]time.Time
	episodeWorst map[string]int64

	// mountsWarned keeps the "discovered nothing" warning to one line per empty
	// period instead of one every interval. Only sweep touches it, and sweep
	// runs on the single Run goroutine.
	mountsWarned bool

	// node is the Kubernetes node name, resolved once at startup. See
	// detectedNodeName for why it must not just be os.Hostname().
	node string
}

// New builds an Agent with sane defaults applied.
func New(opts Options) *Agent {
	if opts.PodsDir == "" {
		opts.PodsDir = "/var/lib/kubelet/pods"
	}
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Minute
	}
	if opts.ListenAddr == "" {
		opts.ListenAddr = ":9101"
	}
	if opts.ProbeTimeout <= 0 {
		opts.ProbeTimeout = 30 * time.Second
	}
	if opts.NodeName == "" {
		opts.NodeName = detectedNodeName()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Agent{
		opts:         opts,
		log:          opts.Logger,
		samples:      map[string]Sample{},
		paths:        map[string]string{},
		episodes:     map[string]int64{},
		episodeStart: map[string]time.Time{},
		episodeWorst: map[string]int64{},
		node:         opts.NodeName,
	}
}

// Discover lists the CephFS volume mounts kubelet currently serves on this
// node, keyed by PersistentVolume name. The PV name is part of the path
// layout kubelet itself creates, so no API access is required.
func (a *Agent) Discover() (map[string]string, error) {
	pattern := filepath.Join(a.opts.PodsDir, "*", "volumes", "kubernetes.io~csi", "*", "mount")
	found, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("glob %s: %w", pattern, err)
	}

	out := make(map[string]string, len(found))
	for _, path := range found {
		pv := filepath.Base(filepath.Dir(path))

		onCeph, err := cephfs.IsCephFS(path)
		if err != nil {
			a.log.Debug("skip path: statfs failed", "path", path, "err", err)
			continue
		}
		if !onCeph {
			continue
		}
		if prev, dup := out[pv]; dup && prev != path {
			// A PV can be staged under several pods on the same node; the
			// measurements are equivalent, so keep the first and move on.
			continue
		}
		out[pv] = path
	}
	return out, nil
}

// Probe measures one volume. Read it as: observe, and by observing, heal.
func (a *Agent) Probe(ctx context.Context, pv, path string) Sample {
	start := time.Now()
	s := Sample{PV: pv, Path: path, Node: a.node, ProbedAt: start}

	type result struct {
		stat    cephfs.RStat
		usage   cephfs.Usage
		usErr   error
		elapsed time.Duration
	}
	// Ordering is load-bearing: statfs(2) must be sampled BEFORE the rstat
	// read, because the rstat read revalidates the kernel client's cached
	// inode. Sampling statfs first is what lets drift be detected at all;
	// sampling it after would always return the already-healed value.
	ch := make(chan result, 1)
	go func() {
		usage, usErr := cephfs.Statfs(path)
		stat := cephfs.GetRStat(path)
		ch <- result{stat: stat, usage: usage, usErr: usErr, elapsed: time.Since(start)}
	}()

	var r result
	select {
	case r = <-ch:
	case <-ctx.Done():
		s.Error = ctx.Err().Error()
		s.Duration = time.Since(start)
		a.observe(&s, false)
		return s
	case <-time.After(a.opts.ProbeTimeout):
		s.Error = "probe timed out"
		s.Duration = time.Since(start)
		a.observe(&s, false)
		return s
	}

	s.Duration = r.elapsed
	if !r.stat.Available() {
		s.Error = r.stat.Err
		a.observe(&s, false)
		return s
	}
	s.RBytes = r.stat.RBytes
	s.RFiles = r.stat.RFiles
	s.Quota = r.stat.QuotaMaxBytes
	if r.usErr != nil {
		// Ground truth is still useful even if statfs failed.
		s.Error = fmt.Sprintf("statfs: %v", r.usErr)
	}
	s.StatfsUsed = r.usage.Used
	s.StatfsCapacity = r.usage.Capacity
	s.StatfsAvail = r.usage.Available
	s.Drift = s.RBytes - s.StatfsUsed

	a.observe(&s, s.Error == "")
	return s
}

// observe folds one measurement into the agent's state and exports it.
//
// Every successful probe is a revalidation of the node's kernel client cache:
// the rstat read forced an MDS round trip, so the statfs(2) that kubelet will
// take next is correct. That is why a nonzero Drift here is always transient —
// it is the record of a lie that has just been caught, not a standing fault.
func (a *Agent) observe(s *Sample, ok bool) {
	labels := []string{s.PV, s.Node}
	now := time.Now()

	a.mu.Lock()
	if ok {
		if s.Drift != 0 {
			if _, started := a.episodeStart[s.PV]; !started {
				a.episodeStart[s.PV] = now
				a.episodes[s.PV]++
			}
			if d := abs64(s.Drift); d > a.episodeWorst[s.PV] {
				a.episodeWorst[s.PV] = d
			}
		} else if started, active := a.episodeStart[s.PV]; active {
			// kubelet's figure caught up with reality: episode over.
			metrics.NodeEpisodeSeconds.WithLabelValues(labels...).Observe(now.Sub(started).Seconds())
			delete(a.episodeStart, s.PV)
		}
	}
	if started, active := a.episodeStart[s.PV]; active {
		s.StaleSeconds = time.Since(started).Seconds()
		s.StaleSince = &started
	}
	s.DriftEpisodes = a.episodes[s.PV]
	s.WorstDriftBytes = a.episodeWorst[s.PV]
	a.samples[s.PV] = *s
	a.mu.Unlock()

	if ok {
		metrics.NodeTruthBytes.WithLabelValues(labels...).Set(float64(s.RBytes))
		metrics.NodeTruthFiles.WithLabelValues(labels...).Set(float64(s.RFiles))
		metrics.NodeQuotaBytes.WithLabelValues(labels...).Set(float64(quotaOrZero(s.Quota)))
		metrics.NodeStatfsUsedBytes.WithLabelValues(labels...).Set(float64(s.StatfsUsed))
		metrics.NodeStatfsCapacityBytes.WithLabelValues(labels...).Set(float64(s.StatfsCapacity))
		metrics.NodeDriftBytes.WithLabelValues(labels...).Set(float64(s.Drift))
		metrics.NodeProbeSuccess.WithLabelValues(labels...).Set(1)
		metrics.NodeRevalidateTotal.WithLabelValues(s.PV, s.Node, "ok").Inc()
	} else {
		metrics.NodeProbeSuccess.WithLabelValues(labels...).Set(0)
		metrics.NodeRevalidateTotal.WithLabelValues(s.PV, s.Node, "error").Inc()
	}
	metrics.NodeDriftEpisodesTotal.WithLabelValues(labels...).Set(float64(s.DriftEpisodes))
	metrics.NodeDriftBytesLast.WithLabelValues(labels...).Set(float64(s.WorstDriftBytes))
	metrics.NodeReportedStaleSeconds.WithLabelValues(labels...).Set(s.StaleSeconds)
	metrics.NodeProbeDuration.WithLabelValues(s.Node).Observe(s.Duration.Seconds())
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func quotaOrZero(q int64) int64 {
	if q < 0 {
		return 0
	}
	return q
}

// Run probes every discovered volume once per interval until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) {
	ticker := time.NewTicker(a.opts.Interval)
	defer ticker.Stop()

	a.sweep(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.sweep(ctx)
		}
	}
}

func (a *Agent) sweep(ctx context.Context) {
	paths, err := a.Discover()
	if err != nil {
		a.log.Error("discovery failed", "err", err)
		return
	}

	a.mu.Lock()
	a.paths = paths
	a.mu.Unlock()
	metrics.NodeMounts.Set(float64(len(paths)))

	// Discovering nothing is the silent failure mode described in DEVELOPING
	// 3.7: a wrong CephFS magic, or a hostPath pointing at the wrong kubelet
	// root, makes every dashboard green while nothing is actually measured.
	// There is no way to know from here whether this node *should* serve CephFS
	// volumes (the agent has no API access on purpose), so state the fact once
	// per empty period and let an operator decide.
	if len(paths) == 0 {
		if !a.mountsWarned {
			a.log.Warn("no CephFS volume mounts discovered; if this node serves CephFS volumes, check --pods-dir and whether the mounts carry the CephFS magic",
				"pods_dir", a.opts.PodsDir)
			a.mountsWarned = true
		}
	} else {
		if a.mountsWarned {
			a.log.Info("CephFS volume mounts are visible again", "mounts", len(paths))
		}
		a.mountsWarned = false
	}

	seen := make(map[string]struct{}, len(paths))
	for pv, path := range paths {
		seen[pv] = struct{}{}
		s := a.Probe(ctx, pv, path)
		if s.Error != "" {
			a.log.Warn("probe failed", "pv", pv, "path", path, "err", s.Error)
			continue
		}
		if s.Drift != 0 {
			a.log.Info("statfs drift observed; this read also revalidates the client cache",
				"pv", pv, "node", s.Node, "true", s.RBytes, "statfs", s.StatfsUsed, "drift", s.Drift)
		}
	}

	// Drop series for volumes that are gone from this node.
	a.mu.Lock()
	stale := make([]string, 0, len(a.samples))
	for pv := range a.samples {
		if _, ok := seen[pv]; !ok {
			stale = append(stale, pv)
		}
	}
	for _, pv := range stale {
		delete(a.samples, pv)
		delete(a.episodeStart, pv)
		delete(a.episodeWorst, pv)
	}
	a.mu.Unlock()

	// NodeProbeDuration is keyed by node only and is deliberately not dropped
	// here: that would discard the history of every other volume still living
	// on this node.
	for _, pv := range stale {
		node := a.node
		labels := []string{pv, node}
		metrics.NodeTruthBytes.DeleteLabelValues(labels...)
		metrics.NodeTruthFiles.DeleteLabelValues(labels...)
		metrics.NodeQuotaBytes.DeleteLabelValues(labels...)
		metrics.NodeStatfsUsedBytes.DeleteLabelValues(labels...)
		metrics.NodeStatfsCapacityBytes.DeleteLabelValues(labels...)
		metrics.NodeDriftBytes.DeleteLabelValues(labels...)
		metrics.NodeProbeSuccess.DeleteLabelValues(labels...)
		metrics.NodeDriftBytesLast.DeleteLabelValues(labels...)
		metrics.NodeReportedStaleSeconds.DeleteLabelValues(labels...)
		metrics.NodeDriftEpisodesTotal.DeleteLabelValues(labels...)
		metrics.NodeEpisodeSeconds.DeleteLabelValues(labels...)
		// Counters carry an extra result label, so the shared label set above
		// cannot remove them; both values have to be dropped explicitly.
		metrics.NodeRevalidateTotal.DeleteLabelValues(pv, node, "ok")
		metrics.NodeRevalidateTotal.DeleteLabelValues(pv, node, "error")
	}
}

// Samples returns a snapshot of the last sweep.
func (a *Agent) Samples() []Sample {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]Sample, 0, len(a.samples))
	for _, s := range a.samples {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PV < out[j].PV })
	return out
}

// Sample returns the cached sample for one PV, if any.
func (a *Agent) Sample(pv string) (Sample, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	s, ok := a.samples[pv]
	return s, ok
}

// Handler serves the agent's HTTP API.
//
// Endpoints:
//
//	GET /healthz                 liveness
//	GET /metrics                 Prometheus exposition
//	GET /v1/probe                every known sample (cached)
//	GET /v1/probe?pv=<pv>        re-probe one volume *now*: this is the repair
//	GET /v1/volumes              the PV -> mount path map this node serves
//
// The pv parameter is validated against the discovered set, so the endpoint
// cannot be turned into an arbitrary-file-read primitive.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/v1/volumes", func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		defer a.mu.RUnlock()
		writeJSON(w, http.StatusOK, a.paths)
	})
	mux.HandleFunc("/v1/probe", func(w http.ResponseWriter, r *http.Request) {
		pv := r.URL.Query().Get("pv")
		if pv == "" {
			writeJSON(w, http.StatusOK, a.Samples())
			return
		}

		a.mu.RLock()
		path, known := a.paths[pv]
		a.mu.RUnlock()
		if !known {
			// Re-discover once: the volume may have been mounted since the
			// last sweep, or the caller may be racing a reschedule.
			paths, err := a.Discover()
			if err == nil {
				a.mu.Lock()
				a.paths = paths
				a.mu.Unlock()
				path, known = paths[pv]
			}
		}
		if !known {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown pv on this node", "pv": pv})
			return
		}

		s := a.Probe(r.Context(), pv, path)
		status := http.StatusOK
		if s.Error != "" {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, s)
	})
	return mux
}

// Serve runs the HTTP API until ctx is cancelled.
func (a *Agent) Serve(ctx context.Context) error {
	srv := &http.Server{
		Addr:              a.opts.ListenAddr,
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// /v1/probe runs a probe inline, so the write deadline has to outlast
		// --probe-timeout; a fixed value would start truncating responses as
		// soon as somebody raised that flag.
		WriteTimeout: a.opts.ProbeTimeout + 30*time.Second,
		IdleTimeout:  120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		a.log.Info("agent API listening", "addr", a.opts.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// detectedNodeName returns the Kubernetes node this agent runs on.
//
// os.Hostname() alone is not enough. With hostNetwork set to false — the
// DaemonSet's setting — the kernel hostname inside the pod is the *pod* name,
// so every series would be labelled node="statfs-guard-agent-vsn2b" while the
// manager labels the same volume with the real node name from spec.nodeName.
// The two never join, and the node label changes on every DaemonSet rollout.
//
// The DaemonSet therefore injects NODE_NAME from spec.nodeName through the
// downward API. Falling back to the hostname keeps a bare `agent` run directly
// on a node (README §四) producing sensible labels.
func detectedNodeName() string {
	if n := strings.TrimSpace(os.Getenv("NODE_NAME")); n != "" {
		return n
	}
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

// ParseByteSize accepts plain bytes or a Kubernetes-style suffix such as
// 64Mi, 1Gi, 5%.
func ParseByteSize(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("empty size")
	}
	multipliers := []struct {
		suffix string
		mult   int64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
		{"K", 1000}, {"M", 1000 * 1000}, {"G", 1000 * 1000 * 1000}, {"T", 1000 * 1000 * 1000 * 1000},
		{"k", 1000}, {"m", 1000 * 1000}, {"g", 1000 * 1000 * 1000},
	}
	for _, m := range multipliers {
		if len(s) > len(m.suffix) && s[len(s)-len(m.suffix):] == m.suffix {
			v, err := strconv.ParseFloat(s[:len(s)-len(m.suffix)], 64)
			if err != nil {
				return 0, err
			}
			return int64(v * float64(m.mult)), nil
		}
	}
	return strconv.ParseInt(s, 10, 64)
}
