// scalerunnergh is a fully ephemeral, GPU-aware, auto-scaling GitHub Actions
// runner orchestrator. It polls GitHub for queued workflow jobs, launches
// Docker-based ephemeral runner containers on demand, and cleans them up
// after completion.
//
// Usage:
//
//	scalerunnergh --config config.json
//	scalerunnergh  # uses environment variables
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/emon5122/scalerunnergh/internal/config"
	gh "github.com/emon5122/scalerunnergh/internal/github"
	"github.com/emon5122/scalerunnergh/internal/metrics"
	"github.com/emon5122/scalerunnergh/internal/orchestrator"
	"github.com/emon5122/scalerunnergh/internal/resource"
	"github.com/emon5122/scalerunnergh/internal/runner"
)

var (
	version   = "dev"
	buildTime = "unknown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configFile  string
		showVersion bool
	)
	flag.StringVar(&configFile, "config", "", "Path to config JSON file (optional, can use env vars)")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("scalerunnergh %s (built %s)\n", version, buildTime)
		return nil
	}

	// Load config
	var cfg *config.Config
	var err error
	if configFile != "" {
		cfg, err = config.LoadFile(configFile)
	} else {
		cfg, err = config.LoadFromEnv()
	}
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Setup logger
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	logger.Info("starting scalerunnergh",
		"version", version,
		"repos", len(cfg.Repos),
		"max_runners", cfg.Resources.MaxRunners,
		"max_gpus", cfg.Resources.MaxGPUs,
		"gpu_enabled", cfg.Docker.GPUEnabled,
		"image", cfg.Docker.Image)

	// Initialize components
	ghClient := gh.NewClient(nil)
	dockerClient := runner.NewCLIDockerClient()
	launcher := runner.NewLauncher(dockerClient, logger)
	resMgr := resource.NewManager(
		cfg.Resources.MaxRunners,
		cfg.Resources.MaxGPUs,
		cfg.Resources.MaxCPUs,
		cfg.Resources.MaxMemMB,
	)
	met := metrics.New()

	// Start metrics server if enabled
	if cfg.Metrics.Enabled {
		mux := http.NewServeMux()
		mux.Handle("/metrics", met.Handler())
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "ok")
		})
		mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
			snap := resMgr.Snapshot()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"active_runners":%d,"max_runners":%d,"used_gpus":%d,"max_gpus":%d}`,
				snap.ActiveRunners, snap.MaxRunners, snap.UsedGPUs, snap.MaxGPUs)
		})

		go func() {
			logger.Info("starting metrics server", "address", cfg.Metrics.Address)
			if err := http.ListenAndServe(cfg.Metrics.Address, mux); err != nil {
				logger.Error("metrics server error", "error", err)
			}
		}()
	}

	// Setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	// Create and run orchestrator
	orch := orchestrator.New(cfg, ghClient, launcher, resMgr, met, logger)
	return orch.Run(ctx)
}
