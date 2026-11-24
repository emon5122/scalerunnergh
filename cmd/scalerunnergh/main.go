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
	"github.com/emon5122/scalerunnergh/internal/startwatch"
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
		stateDir    string
	)
	flag.StringVar(&configFile, "config", "", "Path to the host config JSON file")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit")
	flag.BoolVar(&check, "check", false, "Validate the config and exit")
	flag.StringVar(&stateDir, "state-dir", os.Getenv("SCALERUNNERGH_STATE_DIR"),
		"Directory for the restart record (default host.state_dir); set it to also count starts that fail before the config loads")
	flag.Parse()

	if showVersion {
		fmt.Printf("scalerunnergh %s (built %s)\n", version, buildTime)
		return nil
	}
	if configFile == "" {
		return fmt.Errorf("--config is required")
	}
	// The start is recorded before anything that can fail, so a crash loop is noticed whatever its cause.
	var watch *startwatch.Watch
	var recordErr error
	if stateDir != "" && !check {
		watch = startwatch.New(stateDir)
		recordErr = watch.Record()
	}
	cfg, err := config.LoadFile(configFile)
	if err != nil {
		if watch != nil {
			// No config, so no logger and no alert command from it: stderr, defaults and the env command.
			logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
			checkRestarts(context.Background(), logger, watch, recordErr, config.DefaultRestartAlert(), "")
			_ = watch.Exit(err)
		}
		return err
	}
	if check {
		fmt.Printf("config ok: host %s, %d scale set(s), %d slot(s)\n", cfg.Host.Name, len(cfg.ScaleSets), cfg.Host.MaxRunners)
		return nil
	}
	if watch == nil && cfg.Host.StateDir != "" {
		watch = startwatch.New(cfg.Host.StateDir)
		recordErr = watch.Record()
	}

	// SIGINT/SIGTERM (and a Windows service stop) cancel ctx; the app then
	// stops every runner before exiting.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err = runMaybeService(ctx, cancel, func(ctx context.Context) error { return serve(ctx, cfg, watch, recordErr) })
	if watch != nil {
		// A clean exit clears the record; an error is kept for the next start's report.
		_ = watch.Exit(err)
	}
	return err
}

// alertCommandEnv names a program run on a restart spike when the config sets no host.restart_alert.command (or
// cannot be loaded).
const alertCommandEnv = "SCALERUNNERGH_ALERT_COMMAND"

// checkRestarts reports a restart spike: an error in the log and, at most once per window, the alert command.
func checkRestarts(ctx context.Context, logger *slog.Logger, w *startwatch.Watch, recordErr error, ra config.RestartAlertConfig, host string) startwatch.Spike {
	if recordErr != nil {
		logger.Warn("restart record not written; restart spikes may go unnoticed", "file", w.Path(), "error", recordErr)
	}
	sp, err := w.Evaluate(ra.Window.Duration, ra.MaxStarts)
	if err != nil {
		logger.Warn("restart record not readable", "file", w.Path(), "error", err)
		return sp
	}
	if !sp.Spike {
		return sp
	}
	logger.Error("restart spike: the service keeps failing and being restarted", "starts", sp.Starts,
		"window", sp.Window, "max_starts", ra.MaxStarts, "last_error", sp.LastError)
	if !sp.AlertDue {
		return sp
	}
	command := ra.Command
	if len(command) == 0 {
		if p := os.Getenv(alertCommandEnv); p != "" {
			command = []string{p}
		}
	}
	if len(command) == 0 {
		return sp
	}
	out, err := startwatch.Alert(ctx, command, host, sp)
	if err != nil {
		logger.Error("restart spike alert command failed", "command", command[0], "error", err, "output", out)
		return sp // not marked: the next start tries again
	}
	logger.Info("restart spike alert sent", "command", command[0])
	if err := w.MarkAlerted(); err != nil {
		logger.Warn("restart record not updated after the alert", "error", err)
	}
	return sp
}

func serve(ctx context.Context, cfg *config.Config, watch *startwatch.Watch, recordErr error) error {
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
			if watch != nil {
				checkRestarts(ctx, slog.New(slog.NewTextHandler(os.Stderr, nil)), watch, recordErr, *cfg.Host.RestartAlert, cfg.Host.Name)
			}
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
	met.Set(metrics.StartTime, "", float64(time.Now().Unix()))
	if watch != nil {
		sp := checkRestarts(ctx, logger, watch, recordErr, *cfg.Host.RestartAlert, cfg.Host.Name)
		met.Set(metrics.RecentStarts, "", float64(sp.Starts))
		if sp.Spike {
			met.Set(metrics.RestartSpike, "", 1)
			a.SetRestartSpike(fmt.Sprintf("restart spike: %d unclean starts within %s", sp.Starts, sp.Window), sp.Window)
		} else {
			met.Set(metrics.RestartSpike, "", 0)
		}
	}

	if cfg.Metrics.Enabled {
		mux := http.NewServeMux()
		mux.Handle("/metrics", met.Handler())
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, a.Health())
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
