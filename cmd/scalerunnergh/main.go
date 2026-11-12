// scalerunnergh serves GitHub Actions runner scale sets from one host: it
// starts an ephemeral runner per job (a container, or a native process on
// Linux and Windows), within a host-wide slot pool, and removes it afterwards.
//
// Usage:
//
//	scalerunnergh --config /etc/scalerunnergh/config.json
//	scalerunnergh --config config.json --check   # validate and exit
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/emon5122/scalerunnergh/internal/app"
	"github.com/emon5122/scalerunnergh/internal/config"
	"github.com/emon5122/scalerunnergh/internal/metrics"
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
		check       bool
	)
	flag.StringVar(&configFile, "config", "", "Path to the host config JSON file")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit")
	flag.BoolVar(&check, "check", false, "Validate the config and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("scalerunnergh %s (built %s)\n", version, buildTime)
		return nil
	}
	if configFile == "" {
		return fmt.Errorf("--config is required")
	}
	cfg, err := config.LoadFile(configFile)
	if err != nil {
		return err
	}
	if check {
		fmt.Printf("config ok: host %s, %d scale set(s), %d slot(s)\n", cfg.Host.Name, len(cfg.ScaleSets), cfg.Host.MaxRunners)
		return nil
	}

	// SIGINT/SIGTERM (and a Windows service stop) cancel ctx; the app then
	// stops every runner before exiting.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return runMaybeService(ctx, cancel, func(ctx context.Context) error { return serve(ctx, cfg) })
}

func serve(ctx context.Context, cfg *config.Config) error {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	var out io.Writer = os.Stdout
	if cfg.LogFile != "" {
		// A Windows service has no stdout; log_file is its only log.
		f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			return fmt.Errorf("open log_file: %w", err)
		}
		defer f.Close()
		out = f
	}
	logger := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level}))
	logger.Info("starting scalerunnergh", "version", version, "host", cfg.Host.Name,
		"scale_sets", len(cfg.ScaleSets), "max_runners", cfg.Host.MaxRunners, "max_heavy", cfg.Host.MaxHeavy)

	met := metrics.New()
	a := app.New(cfg, version, logger, met)

	if cfg.Metrics.Enabled {
		mux := http.NewServeMux()
		mux.Handle("/metrics", met.Handler())
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "ok")
		})
		mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(a.Status())
		})
		srv := &http.Server{Addr: cfg.Metrics.Address, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			logger.Info("metrics server listening", "address", cfg.Metrics.Address)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("metrics server error", "error", err)
			}
		}()
		defer srv.Close()
	}
	if err := a.Run(ctx); err != nil {
		// Also to the log: a service has no stderr.
		logger.Error("scalerunnergh stopped with an error", "error", err)
		return err
	}
	logger.Info("scalerunnergh stopped")
	return nil
}
