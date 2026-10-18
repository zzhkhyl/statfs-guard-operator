// Command agent is the node-local half of statfs-guard. Run it as a
// DaemonSet with /var/lib/kubelet/pods mounted read-only.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zzhkhyl/statfs-guard-operator/internal/agent"
)

func main() {
	var (
		podsDir     = flag.String("pods-dir", "/var/lib/kubelet/pods", "host path holding kubelet pod volume dirs")
		interval    = flag.Duration("probe-interval", 5*time.Minute, "how often each visible volume is probed and revalidated")
		probeTO     = flag.Duration("probe-timeout", 30*time.Second, "per-volume probe timeout")
		listen      = flag.String("listen", ":9101", "address of the agent HTTP API")
		logLevel    = flag.String("log-level", "info", "debug|info|warn|error")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		os.Stdout.WriteString(version + "\n")
		return
	}

	logger := newLogger(*logLevel)
	a := agent.New(agent.Options{
		PodsDir:      *podsDir,
		Interval:     *interval,
		ProbeTimeout: *probeTO,
		ListenAddr:   *listen,
		Logger:       logger,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("statfs-guard agent starting",
		"pods_dir", *podsDir, "probe_interval", interval.String(), "listen", *listen)

	go a.Run(ctx)
	if err := a.Serve(ctx); err != nil {
		logger.Error("agent exited with error", "err", err)
		os.Exit(1)
	}
	logger.Info("statfs-guard agent stopped")
}

// version is overridden at build time with -ldflags.
var version = "dev"

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
