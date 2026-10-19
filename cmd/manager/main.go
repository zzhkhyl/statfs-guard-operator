// Command manager is the cluster-wide half of statfs-guard.
package main

import (
	"flag"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	storagev1alpha1 "github.com/zzhkhyl/statfs-guard-operator/api/v1alpha1"
	"github.com/zzhkhyl/statfs-guard-operator/internal/controller"
	"github.com/zzhkhyl/statfs-guard-operator/internal/kube"
)

var version = "dev"

func main() {
	var (
		metricsAddr = flag.String("metrics-bind-address", ":8080", "address the metrics endpoint binds to")
		probeAddr   = flag.String("health-probe-bind-address", ":8081", "address the health probe endpoint binds to")
		leaderElect = flag.Bool("leader-elect", true, "enable leader election, so only one manager is active")

		agentNamespace = flag.String("agent-namespace", "statfs-guard", "namespace the node agents run in")
		agentSelector  = flag.String("agent-selector", "app.kubernetes.io/name=statfs-guard-agent", "label selector for agent pods")
		agentPort      = flag.Int("agent-port", 9101, "port the agent HTTP API listens on")

		driftMinBytes = flag.Int64("drift-min-bytes", 64<<20, "absolute drift below which two readings are considered equal")
		driftMinRatio = flag.Float64("drift-min-ratio", 0.05, "relative drift below which two readings are considered equal")
		autoHeal      = flag.Bool("auto-heal", true, "issue the revalidating read automatically (read-only, cannot harm data)")

		resyncInterval = flag.Duration("resync-interval", 5*time.Minute, "how often healthy volumes are re-checked")
		verifyInterval = flag.Duration("verify-interval", 60*time.Second, "how soon a volume in an open episode is re-checked")
		agentTimeout   = flag.Duration("agent-timeout", 30*time.Second, "timeout for agent calls")

		storageClasses    = flag.String("storage-classes", "", "comma-separated storage class allowlist; empty means every cephfs.csi.ceph.com volume")
		excludeNamespaces = flag.String("exclude-namespaces", "", "comma-separated namespaces to skip")
		logLevel          = flag.String("log-level", "info", "debug|info|warn|error")
		showVersion       = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		os.Stdout.WriteString(version + "\n")
		return
	}

	logger := newLogger(*logLevel)
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, storagev1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			logger.Error("register scheme", "err", err)
			os.Exit(1)
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *probeAddr,
		LeaderElection:         *leaderElect,
		LeaderElectionID:       "statfs-guard.storage.statfsguard.io",
	})
	if err != nil {
		logger.Error("create manager", "err", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		logger.Error("create clientset", "err", err)
		os.Exit(1)
	}

	r := &controller.Reconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Recorder:          mgr.GetEventRecorderFor("statfs-guard"),
		Log:               logger,
		Agents:            kube.NewAgentClient(clientset, *agentNamespace, *agentSelector, *agentPort, *agentTimeout),
		DriftMinBytes:     *driftMinBytes,
		DriftMinRatio:     *driftMinRatio,
		AutoHeal:          *autoHeal,
		ResyncInterval:    *resyncInterval,
		VerifyInterval:    *verifyInterval,
		StorageClasses:    toSet(*storageClasses),
		ExcludeNamespaces: toSet(*excludeNamespaces),
	}
	if err := r.SetupWithManager(mgr); err != nil {
		logger.Error("set up controller", "err", err)
		os.Exit(1)
	}

	// These are two separate registries. Registering "/readyz" through
	// AddHealthzCheck leaves the readyz handler unmounted, so the kubelet's
	// readiness probe gets a 404 and the Deployment never becomes Ready.
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		logger.Error("add liveness check", "err", err)
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		logger.Error("add readiness check", "err", err)
		os.Exit(1)
	}

	logger.Info("statfs-guard manager starting",
		"version", version,
		"auto_heal", *autoHeal,
		"drift_min_bytes", *driftMinBytes,
		"drift_min_ratio", *driftMinRatio)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error("manager exited with error", "err", err)
		os.Exit(1)
	}
}

func toSet(csv string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(csv, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out[p] = true
		}
	}
	return out
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}
