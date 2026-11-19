// Package gc runs the service's own cleanup (the `gc` config block) on its own timer: unused docker images, the
// daemon's build cache and leftover native job dirs, plus a disk-space check. Its outcome is part of /status and
// /health, so whoever watches the service sees a failing cleanup or a filling disk without any outside schedule.
package gc

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/emon5122/scalerunnergh/internal/backend"
	"github.com/emon5122/scalerunnergh/internal/config"
	"github.com/emon5122/scalerunnergh/internal/metrics"
)

// Images is the docker side of a run (backend.Docker).
type Images interface {
	GCImages(ctx context.Context, keepRefs []string, p backend.ImagePolicy, dryRun bool) (backend.ImageGCResult, error)
	PruneBuildCache(ctx context.Context, maxBytes int64) (string, error)
	RootDir(ctx context.Context) string
}

// JobDirs is the native side of a run (backend.Native).
type JobDirs interface {
	SweepJobDirs(maxAge time.Duration, dryRun bool) (backend.SweepResult, error)
	Root() string
}

// Disk is one checked path in Status.
type Disk struct {
	Path        string  `json:"path"`
	FreeBytes   uint64  `json:"free_bytes"`
	TotalBytes  uint64  `json:"total_bytes"`
	FreePercent float64 `json:"free_percent"`
	Pressure    bool    `json:"pressure"`
}

// Status is the gc part of /status.
type Status struct {
	DryRun       bool       `json:"dry_run"`
	Interval     string     `json:"interval"`
	LastRun      *time.Time `json:"last_run,omitempty"`
	LastDuration string     `json:"last_duration,omitempty"`
	// OK is null before the first run, then whether the last run had no errors.
	OK *bool `json:"ok"`
	// Stale: the last run is older than twice the interval plus the run timeout (the loop is stuck).
	Stale  bool     `json:"stale"`
	Errors []string `json:"errors,omitempty"`

	ImagesCount             int      `json:"images_count"`
	ImagesBytes             int64    `json:"images_bytes"`
	ImagesOverCap           bool     `json:"images_over_cap"`
	ImagesPlanned           []string `json:"images_planned,omitempty"` // dry run: what a real run would remove
	ImagesRemovedTotal      int      `json:"images_removed_total"`
	BytesFreedEstimateTotal int64    `json:"bytes_freed_estimate_total"`
	BuildCachePruned        string   `json:"build_cache_pruned,omitempty"` // docker's summary of the last prune

	NativeDirsRemovedTotal int      `json:"native_dirs_removed_total"`
	NativeLeftoverDirs     []string `json:"native_leftover_dirs,omitempty"`

	Disks        []Disk `json:"disks,omitempty"`
	DiskPressure bool   `json:"disk_pressure"`
}

// Degraded lists why the service needs a look (empty = fine).
func (s Status) Degraded() []string {
	var out []string
	if s.Stale {
		out = append(out, "gc has not run for too long")
	}
	if len(s.Errors) > 0 {
		out = append(out, fmt.Sprintf("gc failed (%d errors)", len(s.Errors)))
	}
	if s.ImagesOverCap {
		out = append(out, "images over gc.images_max_bytes")
	}
	if len(s.NativeLeftoverDirs) > 0 {
		out = append(out, fmt.Sprintf("%d native job dirs could not be removed", len(s.NativeLeftoverDirs)))
	}
	for _, d := range s.Disks {
		if d.Pressure {
			out = append(out, fmt.Sprintf("disk %s %.1f%% free", d.Path, d.FreePercent))
		}
	}
	return out
}

// Runner is the gc loop of one host.
type Runner struct {
	cfg      config.GCConfig
	images   Images  // nil without a docker scale set
	dirs     JobDirs // nil without a native scale set
	keepRefs []string
	logger   *slog.Logger
	metrics  *metrics.Collector
	now      func() time.Time
	usage    func(path string) (free, total uint64, err error)

	mu sync.Mutex
	st Status
}

// New creates the runner. images/dirs may be nil (typed nils are not accepted: pass a nil interface).
func New(cfg *config.Config, images Images, dirs JobDirs, logger *slog.Logger, met *metrics.Collector) *Runner {
	keep := append([]string(nil), cfg.GC.KeepImages...)
	for _, ss := range cfg.ScaleSets {
		if ss.Backend == config.BackendDocker {
			keep = append(keep, ss.Image)
		}
	}
	return &Runner{
		cfg: *cfg.GC, images: images, dirs: dirs, keepRefs: keep, logger: logger.With("component", "gc"), metrics: met,
		now: time.Now, usage: diskUsage,
		st: Status{DryRun: cfg.GC.DryRun, Interval: cfg.GC.Interval.Duration.String()},
	}
}

// FirstRunDelay lets crash cleanup and the first runners settle before the first run.
var FirstRunDelay = time.Minute

func (r *Runner) timeout() time.Duration { return min(r.cfg.Interval.Duration, 30*time.Minute) }

// Loop runs gc FirstRunDelay after it starts, then every interval, until ctx ends.
func (r *Runner) Loop(ctx context.Context) {
	wait := FirstRunDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		rctx, cancel := context.WithTimeout(ctx, r.timeout())
		r.RunOnce(rctx)
		cancel()
		wait = r.cfg.Interval.Duration
	}
}

// RunOnce does one gc run and records its outcome.
func (r *Runner) RunOnce(ctx context.Context) {
	start := r.now()
	var errs []string
	r.mu.Lock()
	next := r.st // carries the totals
	r.mu.Unlock()
	next.Errors, next.ImagesPlanned, next.NativeLeftoverDirs, next.BuildCachePruned = nil, nil, nil, ""

	if r.images != nil && r.imageRules() {
		res, err := r.images.GCImages(ctx, r.keepRefs, backend.ImagePolicy{
			MaxAge: r.cfg.ImageMaxAge.Duration, MinAge: r.cfg.ImageMinAge.Duration, MaxBytes: int64(r.cfg.ImagesMaxBytes),
		}, r.cfg.DryRun)
		if err != nil {
			errs = append(errs, err.Error())
		} else {
			errs = append(errs, res.RemoveError...)
			// What is on the daemon now (estimate): a dry run removes nothing.
			left := res.TotalBytes - res.FreedBytes
			next.ImagesCount, next.ImagesBytes, next.ImagesOverCap = res.Count-res.Removed, left, res.OverCap
			if r.cfg.DryRun {
				next.ImagesPlanned = res.Planned
			}
			next.ImagesRemovedTotal += res.Removed
			next.BytesFreedEstimateTotal += res.FreedBytes
			r.metrics.Set(metrics.GCImagesBytes, "", float64(left))
			r.metrics.Add(metrics.GCImagesRemoved, "", float64(res.Removed))
			r.metrics.Add(metrics.GCBytesFreed, "", float64(res.FreedBytes))
			r.logger.Info("gc: images", "on_daemon", res.Count, "planned", len(res.Planned), "removed", res.Removed,
				"skipped", res.Skipped, "freed_estimate", config.ByteSize(res.FreedBytes).String(),
				"left", config.ByteSize(left).String(), "over_cap", res.OverCap, "dry_run", r.cfg.DryRun)
		}
	}
	if r.images != nil && r.cfg.BuildCacheMaxBytes > 0 && !r.cfg.DryRun {
		sum, err := r.images.PruneBuildCache(ctx, int64(r.cfg.BuildCacheMaxBytes))
		if err != nil {
			errs = append(errs, err.Error())
		} else {
			next.BuildCachePruned = sum
			r.logger.Info("gc: build cache pruned", "max", r.cfg.BuildCacheMaxBytes.String(), "docker", sum)
		}
	}
	if r.dirs != nil {
		res, err := r.dirs.SweepJobDirs(r.cfg.NativeMaxJobDirAge.Duration, r.cfg.DryRun)
		if err != nil {
			errs = append(errs, err.Error())
		}
		if !r.cfg.DryRun {
			next.NativeDirsRemovedTotal += len(res.Removed)
		}
		next.NativeLeftoverDirs = res.Leftovers
		if len(res.Removed) > 0 {
			r.logger.Info("gc: leftover native job dirs", "removed", len(res.Removed), "dry_run", r.cfg.DryRun)
		}
	}

	next.Disks, next.DiskPressure = nil, false
	for _, p := range r.diskPaths(ctx) {
		free, total, err := r.usage(p.path)
		if err != nil || total == 0 {
			if p.configured {
				errs = append(errs, fmt.Sprintf("disk %s: %v", p.path, err))
			}
			continue
		}
		pct := float64(free) * 100 / float64(total)
		d := Disk{Path: p.path, FreeBytes: free, TotalBytes: total, FreePercent: float64(int(pct*10)) / 10, Pressure: pct < r.cfg.MinFreeDiskPercent}
		next.Disks = append(next.Disks, d)
		if d.Pressure {
			next.DiskPressure = true
			r.logger.Warn("gc: disk under pressure", "path", d.Path, "free_percent", d.FreePercent, "free", config.ByteSize(free).String())
		}
	}
	pressure := 0.0
	if next.DiskPressure {
		pressure = 1
	}
	r.metrics.Set(metrics.DiskPressure, "", pressure)

	for _, e := range errs {
		r.logger.Warn("gc: error", "error", e)
	}
	r.metrics.Add(metrics.GCRuns, "", 1)
	if len(errs) > 0 {
		r.metrics.Add(metrics.GCErrors, "", 1)
	}
	end := r.now()
	ok := len(errs) == 0
	next.LastRun, next.LastDuration, next.OK, next.Errors = &end, end.Sub(start).Round(time.Millisecond).String(), &ok, errs
	r.mu.Lock()
	r.st = next
	r.mu.Unlock()
}

func (r *Runner) imageRules() bool {
	return r.cfg.ImageMaxAge.Duration > 0 || r.cfg.ImagesMaxBytes > 0
}

type diskPath struct {
	path       string
	configured bool // an unreadable configured path is an error; an unreadable default is skipped
}

func (r *Runner) diskPaths(ctx context.Context) []diskPath {
	if len(r.cfg.DiskPaths) > 0 {
		out := make([]diskPath, 0, len(r.cfg.DiskPaths))
		for _, p := range r.cfg.DiskPaths {
			out = append(out, diskPath{p, true})
		}
		return out
	}
	var out []diskPath
	if r.dirs != nil {
		out = append(out, diskPath{r.dirs.Root(), false})
	}
	if r.images != nil {
		if p := r.images.RootDir(ctx); p != "" {
			out = append(out, diskPath{p, false})
		}
	}
	return out
}

// Status returns a copy of the last run's outcome.
func (r *Runner) Status() Status {
	r.mu.Lock()
	st := r.st
	r.mu.Unlock()
	st.Errors = append([]string(nil), st.Errors...)
	st.ImagesPlanned = append([]string(nil), st.ImagesPlanned...)
	st.NativeLeftoverDirs = append([]string(nil), st.NativeLeftoverDirs...)
	st.Disks = append([]Disk(nil), st.Disks...)
	if st.LastRun != nil {
		t := *st.LastRun
		st.LastRun = &t
		st.Stale = r.now().Sub(t) > 2*r.cfg.Interval.Duration+r.timeout()
	}
	return st
}
