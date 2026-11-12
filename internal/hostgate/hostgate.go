// Package hostgate protects the host: a load/memory gate that stops new
// runners while the box is busy (production hosts), and a kill-switch file
// watcher that pauses the pool and can stop running jobs.
package hostgate

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// LoadGate checks /proc/loadavg and /proc/meminfo. Zero thresholds are off.
// On hosts without /proc (Windows) it always allows.
type LoadGate struct {
	MaxLoadPerCore    float64
	MinMemAvailableMB int
	ProcRoot          string // default "/proc"; tests point it elsewhere
	Cores             int    // default runtime.NumCPU()
}

// Check implements pool.Gate.
func (g LoadGate) Check() (bool, string) {
	if g.MaxLoadPerCore <= 0 && g.MinMemAvailableMB <= 0 {
		return true, ""
	}
	root := g.ProcRoot
	if root == "" {
		root = "/proc"
	}
	cores := g.Cores
	if cores <= 0 {
		cores = runtime.NumCPU()
	}
	if g.MaxLoadPerCore > 0 {
		load, err := readLoad1(filepath.Join(root, "loadavg"))
		switch {
		case errors.Is(err, os.ErrNotExist):
			// no /proc on this OS: gate is not available, allow.
		case err != nil:
			return false, "load gate: " + err.Error() // fail closed
		case load/float64(cores) > g.MaxLoadPerCore:
			return false, fmt.Sprintf("load1 %.2f over %.2f per core", load, g.MaxLoadPerCore)
		}
	}
	if g.MinMemAvailableMB > 0 {
		mb, err := readMemAvailableMB(filepath.Join(root, "meminfo"))
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return false, "memory gate: " + err.Error()
		case mb < g.MinMemAvailableMB:
			return false, fmt.Sprintf("MemAvailable %d MB under %d MB", mb, g.MinMemAvailableMB)
		}
	}
	return true, ""
}

func readLoad1(path string) (float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, fmt.Errorf("empty %s", path)
	}
	return strconv.ParseFloat(f[0], 64)
}

func readMemAvailableMB(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kb, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0, err
		}
		return kb / 1024, nil
	}
	return 0, fmt.Errorf("MemAvailable not found in %s", path)
}

// KillSwitch polls a file. While it exists, OnChange(true) has been called.
type KillSwitch struct {
	Path     string
	Interval time.Duration // default 2s
	OnChange func(active bool)
	Logger   *slog.Logger
}

// Active reports whether the switch file exists now.
func (k KillSwitch) Active() bool {
	if k.Path == "" {
		return false
	}
	_, err := os.Stat(k.Path)
	return err == nil
}

// Run polls until ctx ends. It calls OnChange once at start with the current
// state and then on every change.
func (k KillSwitch) Run(ctx context.Context) {
	if k.Path == "" {
		return
	}
	iv := k.Interval
	if iv <= 0 {
		iv = 2 * time.Second
	}
	logger := k.Logger
	if logger == nil {
		logger = slog.Default()
	}
	last := k.Active()
	if last {
		logger.Warn("kill switch present at start: taking no jobs", "file", k.Path)
	}
	k.OnChange(last)
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := k.Active()
			if now == last {
				continue
			}
			last = now
			if now {
				logger.Warn("kill switch ON: taking no new jobs", "file", k.Path)
			} else {
				logger.Info("kill switch OFF: resuming", "file", k.Path)
			}
			k.OnChange(now)
		}
	}
}
