package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// GCConfig is the optional `gc` block: the service cleans up after itself on its own timer (unused docker images,
// the host's build cache, leftover native job dirs) and reports failures and disk pressure in its own /status and
// /health. Absent = nothing of this runs.
type GCConfig struct {
	// Interval between runs (default 1h, minimum 5m).
	Interval Duration `json:"interval"`
	// ImageMaxAge: unused images that got their newest tag on this daemon longer ago than this are removed (0 = no
	// age rule).
	ImageMaxAge Duration `json:"image_max_age"`
	// ImageMinAge: images younger than this are never removed (a pull or build in flight; default 6h).
	ImageMinAge Duration `json:"image_min_age"`
	// ImagesMaxBytes: after the age rule, unused images are removed oldest first until the images total is under
	// this (0 = no cap).
	ImagesMaxBytes ByteSize `json:"images_max_bytes"`
	// KeepImages are kept even when unused, on top of every docker scale set's image.
	KeepImages []string `json:"keep_images,omitempty"`
	// BuildCacheMaxBytes: the daemon's BuildKit cache is pruned down to this each run (0 = off; the daemon.json
	// builder.gc setting is the main fix, this is for hosts whose daemon.json cannot be changed).
	BuildCacheMaxBytes ByteSize `json:"build_cache_max_bytes"`
	// NativeMaxJobDirAge: a native job dir no live runner owns, older than this, is removed (default 1h).
	NativeMaxJobDirAge Duration `json:"native_max_job_dir_age"`
	// DiskPaths are checked for free space each run (default: native.root, and the docker root dir when it exists
	// on this machine).
	DiskPaths []string `json:"disk_paths,omitempty"`
	// MinFreeDiskPercent: a path with less free space than this is under pressure (default 10).
	MinFreeDiskPercent float64 `json:"min_free_disk_percent"`
	// DryRun plans and reports but removes nothing (first rollout on a host).
	DryRun bool `json:"dry_run"`
}

// MinGCInterval is the shortest gc interval accepted.
const MinGCInterval = 5 * time.Minute

func (g *GCConfig) validate(c *Config) error {
	if g.Interval.Duration == 0 {
		g.Interval.Duration = time.Hour
	}
	if g.Interval.Duration < MinGCInterval {
		return fmt.Errorf("config: gc.interval must be at least %s", MinGCInterval)
	}
	if g.ImageMaxAge.Duration < 0 || g.ImageMinAge.Duration < 0 || g.NativeMaxJobDirAge.Duration < 0 ||
		g.ImagesMaxBytes < 0 || g.BuildCacheMaxBytes < 0 {
		return errors.New("config: gc durations and sizes must be >= 0")
	}
	if g.ImageMinAge.Duration == 0 {
		g.ImageMinAge.Duration = 6 * time.Hour
	}
	if g.ImageMaxAge.Duration > 0 && g.ImageMaxAge.Duration < g.ImageMinAge.Duration {
		return errors.New("config: gc.image_max_age must not be shorter than gc.image_min_age")
	}
	if g.NativeMaxJobDirAge.Duration == 0 {
		g.NativeMaxJobDirAge.Duration = time.Hour
	}
	if g.MinFreeDiskPercent == 0 {
		g.MinFreeDiskPercent = 10
	}
	if g.MinFreeDiskPercent < 0 || g.MinFreeDiskPercent >= 100 {
		return errors.New("config: gc.min_free_disk_percent must be between 0 and 100")
	}
	for _, r := range g.KeepImages {
		if r == "" || strings.ContainsAny(r, " \t") {
			return fmt.Errorf("config: gc.keep_images: invalid image reference %q", r)
		}
	}
	for _, p := range g.DiskPaths {
		if p == "" {
			return errors.New("config: gc.disk_paths must not contain an empty path")
		}
	}
	imageRules := g.ImageMaxAge.Duration > 0 || g.ImagesMaxBytes > 0 || g.BuildCacheMaxBytes > 0 || len(g.KeepImages) > 0
	if imageRules && !c.UsesBackend(BackendDocker) {
		return errors.New("config: gc image and build cache settings need a docker scale set")
	}
	return nil
}

// ByteSize is a size in bytes; JSON accepts a number of bytes or a string with a unit: B, KB/MB/GB/TB (powers of
// 1000) or KiB/MiB/GiB/TiB (powers of 1024), e.g. "30GB".
type ByteSize int64

var sizeUnits = map[string]float64{
	"": 1, "B": 1,
	"KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
	"KIB": 1 << 10, "MIB": 1 << 20, "GIB": 1 << 30, "TIB": 1 << 40,
}

// ParseByteSize parses "30GB", "512MiB", "1024" or "1.5 GB".
func ParseByteSize(s string) (ByteSize, error) {
	t := strings.TrimSpace(s)
	i := len(t)
	for i > 0 && (t[i-1] < '0' || t[i-1] > '9') {
		i--
	}
	num, unit := strings.TrimSpace(t[:i]), strings.ToUpper(strings.TrimSpace(t[i:]))
	mult, ok := sizeUnits[unit]
	if !ok || num == "" {
		return 0, fmt.Errorf("size %q: want a number with an optional unit (B, KB, MB, GB, TB, KiB, MiB, GiB, TiB)", s)
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 || math.IsInf(f*mult, 0) || f*mult > math.MaxInt64/2 {
		return 0, fmt.Errorf("size %q: invalid number", s)
	}
	return ByteSize(f * mult), nil
}

// UnmarshalJSON accepts a number of bytes or a size string.
func (b *ByteSize) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		var n int64
		if err2 := json.Unmarshal(data, &n); err2 != nil {
			return fmt.Errorf("size must be a string or a whole number of bytes: %w", err)
		}
		*b = ByteSize(n)
		return nil
	}
	v, err := ParseByteSize(s)
	*b = v
	return err
}

// MarshalJSON writes the number of bytes.
func (b ByteSize) MarshalJSON() ([]byte, error) { return json.Marshal(int64(b)) }

// String is a human size in powers of 1000 (e.g. "24.7GB").
func (b ByteSize) String() string {
	f := float64(b)
	for _, u := range []string{"B", "KB", "MB", "GB", "TB"} {
		if math.Abs(f) < 1000 || u == "TB" {
			return strings.TrimSuffix(strconv.FormatFloat(f, 'f', 1, 64), ".0") + u
		}
		f /= 1000
	}
	return ""
}
