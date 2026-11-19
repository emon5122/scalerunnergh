package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func withGC(gc string) []byte {
	return []byte(strings.Replace(validDocker, `"scale_sets"`, `"gc": `+gc+`, "scale_sets"`, 1))
}

func TestGCDefaults(t *testing.T) {
	cfg, err := Parse([]byte(validDocker))
	if err != nil || cfg.GC != nil {
		t.Fatalf("no gc block must stay off: %v %+v", err, cfg)
	}
	cfg, err = Parse(withGC(`{"image_max_age": "168h", "images_max_bytes": "30GB", "keep_images": ["ghcr.io/o/base:1"]}`))
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.GC
	if g.Interval.Duration != time.Hour || g.ImageMinAge.Duration != 6*time.Hour || g.NativeMaxJobDirAge.Duration != time.Hour ||
		g.MinFreeDiskPercent != 10 || g.ImagesMaxBytes != 30_000_000_000 || g.ImageMaxAge.Duration != 168*time.Hour || g.DryRun {
		t.Fatalf("gc defaults: %+v", g)
	}
}

func TestGCRefusals(t *testing.T) {
	for name, c := range map[string]struct{ gc, want string }{
		"interval too short":  {`{"interval": "1m"}`, "at least 5m"},
		"negative age":        {`{"image_max_age": "-1h"}`, ">= 0"},
		"bad size":            {`{"images_max_bytes": "30 parsecs"}`, "size"},
		"max below min age":   {`{"image_max_age": "1h", "image_min_age": "2h"}`, "image_min_age"},
		"percent out of band": {`{"min_free_disk_percent": 100}`, "min_free_disk_percent"},
		"bad keep ref":        {`{"keep_images": ["a b"]}`, "keep_images"},
		"empty disk path":     {`{"disk_paths": [""]}`, "disk_paths"},
		"unknown field":       {`{"max_images": 3}`, "unknown field"},
	} {
		_, err := Parse(withGC(c.gc))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error with %q, got %v", name, c.want, err)
		}
	}
}

func TestGCImageRulesNeedDocker(t *testing.T) {
	native := `{
  "host": {"name": "w1", "max_runners": 1},
  "auth": {"pat_file": "/run/secrets/pat"},
  "native": {"root": "/srv/srg", "runner_version": "2.330.0", "runner_sha256": "` + strings.Repeat("a", 64) + `"},
  "gc": GC,
  "scale_sets": [{"name": "n", "url": "https://github.com/o", "runner_group": "g", "backend": "native"}]
}`
	if _, err := Parse([]byte(strings.Replace(native, "GC", `{"native_max_job_dir_age": "2h"}`, 1))); err != nil {
		t.Fatalf("native-only gc must parse: %v", err)
	}
	_, err := Parse([]byte(strings.Replace(native, "GC", `{"images_max_bytes": "10GB"}`, 1)))
	if err == nil || !strings.Contains(err.Error(), "docker scale set") {
		t.Fatalf("image rules without docker: %v", err)
	}
}

func TestByteSize(t *testing.T) {
	for in, want := range map[string]ByteSize{
		"1024": 1024, "10GB": 10_000_000_000, "1.5 GB": 1_500_000_000, "512MiB": 512 << 20, "2tib": 2 << 40, "7B": 7,
	} {
		got, err := ParseByteSize(in)
		if err != nil || got != want {
			t.Errorf("%q: %d %v, want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "GB", "-1GB", "1PB", "1e400GB"} {
		if _, err := ParseByteSize(bad); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
	var b ByteSize
	if err := json.Unmarshal([]byte(`123`), &b); err != nil || b != 123 {
		t.Errorf("number: %d %v", b, err)
	}
	if s := ByteSize(24_700_000_000).String(); s != "24.7GB" {
		t.Errorf("String: %s", s)
	}
	if s := ByteSize(999).String(); s != "999B" {
		t.Errorf("String: %s", s)
	}
}
