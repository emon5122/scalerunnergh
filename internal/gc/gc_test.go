package gc

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emon5122/scalerunnergh/internal/backend"
	"github.com/emon5122/scalerunnergh/internal/config"
	"github.com/emon5122/scalerunnergh/internal/metrics"
)

type fakeImages struct {
	res      backend.ImageGCResult
	err      error
	pruneErr error
	keep     []string
	pol      backend.ImagePolicy
	dry      bool
	pruned   int64
	root     string
}

func (f *fakeImages) GCImages(_ context.Context, keep []string, p backend.ImagePolicy, dry bool) (backend.ImageGCResult, error) {
	f.keep, f.pol, f.dry = keep, p, dry
	return f.res, f.err
}

func (f *fakeImages) PruneBuildCache(_ context.Context, max int64) (string, error) {
	f.pruned = max
	return "Total:\t1GB", f.pruneErr
}

func (f *fakeImages) RootDir(context.Context) string { return f.root }

type fakeDirs struct{ res backend.SweepResult }

func (f *fakeDirs) SweepJobDirs(time.Duration, bool) (backend.SweepResult, error) { return f.res, nil }
func (f *fakeDirs) Root() string                                                  { return "/srv/srg" }

// testConfig builds a config with a docker scale set, because the image and
// build-cache settings need one. config.Validate refuses the docker backend on
// Windows, so these tests skip there; TestDiskUsageReal still covers the
// Windows disk path (disk_windows.go).
func testConfig(t *testing.T, gc string) *config.Config {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the docker backend is Linux only; config.Validate refuses it on Windows")
	}
	cfg, err := config.Parse([]byte(`{
  "host": {"name": "h", "max_runners": 2},
  "auth": {"pat_file": "/run/secrets/pat"},
  "gc": ` + gc + `,
  "scale_sets": [{"name": "d", "url": "https://github.com/o", "runner_group": "g", "backend": "docker", "image": "ghcr.io/o/runner:2"}]
}`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestRunOnceReportsImagesAndDisk(t *testing.T) {
	cfg := testConfig(t, `{"image_max_age": "168h", "images_max_bytes": "30GB", "build_cache_max_bytes": "10GB", "keep_images": ["base:1"], "min_free_disk_percent": 15}`)
	img := &fakeImages{res: backend.ImageGCResult{Count: 17, TotalBytes: 24_700_000_000, Removed: 15, FreedBytes: 24_000_000_000}, root: "/var/lib/docker"}
	met := metrics.New()
	r := New(cfg, img, nil, slog.New(slog.DiscardHandler), met)
	r.usage = func(p string) (uint64, uint64, error) { return 10, 100, nil } // 10% free < 15%

	if st := r.Status(); st.OK != nil || st.LastRun != nil || len(st.Degraded()) != 0 {
		t.Fatalf("before the first run: %+v", st)
	}
	r.RunOnce(context.Background())
	st := r.Status()
	if st.OK == nil || !*st.OK || st.ImagesCount != 2 || st.ImagesBytes != 700_000_000 || st.ImagesRemovedTotal != 15 ||
		st.BytesFreedEstimateTotal != 24_000_000_000 || st.BuildCachePruned != "Total:\t1GB" {
		t.Fatalf("status: %+v", st)
	}
	if !slices.Equal(img.keep, []string{"base:1", "ghcr.io/o/runner:2"}) || img.pol.MaxBytes != 30_000_000_000 ||
		img.pol.MaxAge != 168*time.Hour || img.pol.MinAge != 6*time.Hour || img.pruned != 10_000_000_000 {
		t.Fatalf("inputs: keep %v policy %+v pruned %d", img.keep, img.pol, img.pruned)
	}
	if len(st.Disks) != 1 || st.Disks[0].Path != "/var/lib/docker" || !st.Disks[0].Pressure || !st.DiskPressure {
		t.Fatalf("disks: %+v", st.Disks)
	}
	if d := st.Degraded(); len(d) != 1 || !strings.Contains(d[0], "/var/lib/docker 10.0% free") {
		t.Fatalf("degraded: %v", d)
	}
	if met.Get(metrics.DiskPressure, "") != 1 || met.Get(metrics.GCRuns, "") != 1 || met.Get(metrics.GCImagesRemoved, "") != 15 {
		t.Fatal("metrics not set")
	}

	// Totals add up over runs; errors and over-cap make it degraded; an unreadable default disk path is skipped.
	img.err = errors.New("list images: daemon down")
	img.pruneErr = errors.New("prune build cache: boom")
	r.usage = func(string) (uint64, uint64, error) { return 0, 0, errors.New("nope") }
	r.RunOnce(context.Background())
	st = r.Status()
	if *st.OK || len(st.Errors) != 2 || st.ImagesRemovedTotal != 15 || len(st.Disks) != 0 || st.DiskPressure {
		t.Fatalf("failed run: %+v", st)
	}
	if d := st.Degraded(); len(d) != 1 || !strings.Contains(d[0], "gc failed (2 errors)") {
		t.Fatalf("degraded: %v", d)
	}
	if met.Get(metrics.GCErrors, "") != 1 || met.Get(metrics.DiskPressure, "") != 0 {
		t.Fatal("error metrics")
	}
	img.err, img.pruneErr = nil, nil
	img.res = backend.ImageGCResult{Count: 3, TotalBytes: 40_000_000_000, OverCap: true}
	r.RunOnce(context.Background())
	if d := r.Status().Degraded(); len(d) != 1 || !strings.Contains(d[0], "over gc.images_max_bytes") {
		t.Fatalf("over cap: %v", d)
	}
	if b, err := json.Marshal(r.Status()); err != nil || !strings.Contains(string(b), `"images_over_cap":true`) {
		t.Fatalf("json: %s %v", b, err)
	}
}

func TestRunOnceDryRunAndNative(t *testing.T) {
	cfg := testConfig(t, `{"dry_run": true, "image_max_age": "168h", "build_cache_max_bytes": "10GB", "disk_paths": ["/data"]}`)
	img := &fakeImages{res: backend.ImageGCResult{Count: 4, TotalBytes: 4, Planned: []string{"abc"}}}
	dirs := &fakeDirs{res: backend.SweepResult{Removed: []string{"/srv/srg/jobs/a"}, Leftovers: []string{"/srv/srg/jobs/b"}}}
	r := New(cfg, img, dirs, slog.New(slog.DiscardHandler), metrics.New())
	r.usage = func(p string) (uint64, uint64, error) { return 0, 0, errors.New("not mounted") }
	r.RunOnce(context.Background())
	st := r.Status()
	if !img.dry || img.pruned != 0 {
		t.Fatalf("dry run must pass dry to the images and skip the build cache prune: %v %d", img.dry, img.pruned)
	}
	if !slices.Equal(st.ImagesPlanned, []string{"abc"}) || st.NativeDirsRemovedTotal != 0 || !slices.Equal(st.NativeLeftoverDirs, []string{"/srv/srg/jobs/b"}) {
		t.Fatalf("status: %+v", st)
	}
	// A configured disk path that cannot be read is an error.
	if *st.OK || len(st.Errors) != 1 || !strings.Contains(st.Errors[0], "disk /data") {
		t.Fatalf("configured disk path: %+v", st)
	}
	if d := st.Degraded(); len(d) != 2 {
		t.Fatalf("degraded: %v", d)
	}
}

func TestStatusStale(t *testing.T) {
	cfg := testConfig(t, `{"interval": "10m", "image_max_age": "168h"}`)
	r := New(cfg, &fakeImages{}, nil, slog.New(slog.DiscardHandler), metrics.New())
	r.usage = func(string) (uint64, uint64, error) { return 50, 100, nil }
	now := time.Now()
	r.now = func() time.Time { return now }
	r.RunOnce(context.Background())
	if r.Status().Stale {
		t.Fatal("fresh run is not stale")
	}
	now = now.Add(31 * time.Minute) // > 2x10m + 10m timeout
	if st := r.Status(); !st.Stale || !strings.Contains(strings.Join(st.Degraded(), ","), "not run") {
		t.Fatalf("stale: %+v", st)
	}
}

func TestLoopRunsAndStops(t *testing.T) {
	old := FirstRunDelay
	FirstRunDelay = time.Millisecond
	defer func() { FirstRunDelay = old }()
	cfg := testConfig(t, `{"image_max_age": "168h"}`)
	r := New(cfg, &fakeImages{}, nil, slog.New(slog.DiscardHandler), metrics.New())
	r.usage = func(string) (uint64, uint64, error) { return 50, 100, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Loop(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for r.Status().LastRun == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if r.Status().LastRun == nil {
		t.Fatal("the loop never ran")
	}
}

func TestDiskUsageReal(t *testing.T) {
	free, total, err := diskUsage(t.TempDir())
	if err != nil || total == 0 || free > total {
		t.Fatalf("diskUsage: %d/%d %v", free, total, err)
	}
}
