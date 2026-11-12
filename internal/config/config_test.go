package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validDocker = `{
  "host": {"name": "vps 1!", "max_runners": 3, "max_heavy": 1},
  "auth": {"pat_file": "/run/secrets/pat"},
  "scale_sets": [
    {"name": "srg-vps", "url": "https://github.com/nexisltd", "runner_group": "vps-private",
     "labels": ["vps"], "backend": "docker", "image": "img:1", "dind": true, "heavy": true,
     "limits": {"cpus": 2, "memory_mb": 4096, "oom_score_adj": 1000}}
  ]
}`

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse([]byte(validDocker))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host.Name != "vps1" {
		t.Errorf("host name not sanitized: %q", cfg.Host.Name)
	}
	if cfg.Host.IdleTimeout.Duration != 3*time.Minute || cfg.Host.MaxJobDuration.Duration != 390*time.Minute {
		t.Errorf("defaults: %v %v", cfg.Host.IdleTimeout, cfg.Host.MaxJobDuration)
	}
	if cfg.ScaleSets[0].MaxRunners != 3 {
		t.Errorf("scale set max_runners should default to the host max, got %d", cfg.ScaleSets[0].MaxRunners)
	}
	if cfg.Docker.Binary != "docker" || cfg.Metrics.Address != ":9090" || cfg.LogLevel != "info" {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if !cfg.UsesBackend(BackendDocker) || cfg.UsesBackend(BackendNative) {
		t.Error("UsesBackend wrong")
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]struct{ old, new, want string }{
		"unknown field":   {`"max_heavy": 1`, `"max_heavy": 1, "max_runers": 9`, "unknown field"},
		"no slots":        {`"max_runners": 3`, `"max_runners": 0`, "max_runners"},
		"heavy over max":  {`"max_heavy": 1`, `"max_heavy": 5`, "max_heavy"},
		"heavy needs cap": {`"max_heavy": 1`, `"max_heavy": 0`, "heavy scale set"},
		"no group":        {`"runner_group": "vps-private"`, `"runner_group": ""`, "runner_group"},
		"bad url":         {`https://github.com/nexisltd`, `http://github.com/nexisltd`, "must be https"},
		"bad backend":     {`"backend": "docker"`, `"backend": "k8s"`, "backend must be"},
		"no image":        {`"image": "img:1"`, `"image": ""`, "image is required"},
		"no auth":         {`"auth": {"pat_file": "/run/secrets/pat"}`, `"auth": {}`, "pat_file or app"},
		"both auth":       {`{"pat_file": "/run/secrets/pat"}`, `{"pat_file": "x", "app": {"client_id": "a", "installation_id": 1, "private_key_file": "k"}}`, "not both"},
		"protect oom":     {`"oom_score_adj": 1000`, `"oom_score_adj": -500`, "oom_score_adj"},
		"bad name":        {`"name": "srg-vps"`, `"name": "bad name"`, "must match"},
		"native w/ image": {`"backend": "docker"`, `"backend": "native"`, "docker-backend settings"},
		"docker no name":  {`"name": "vps 1!", `, ``, "host.name is required"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(validDocker, c.old) {
				t.Fatalf("test bug: %q not in base config", c.old)
			}
			_, err := Parse([]byte(strings.Replace(validDocker, c.old, c.new, 1)))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestNativeNeedsPinnedRunner(t *testing.T) {
	base := `{"host": {"name": "pc09", "max_runners": 1},
	  "auth": {"app": {"client_id": "Iv1", "installation_id": 7, "private_key_file": "k.pem"}},
	  "native": NATIVE,
	  "scale_sets": [{"name": "w", "url": "https://github.com/o/r", "runner_group": "g", "backend": "native", "limits": {"nice": 10}}]}`
	sha := strings.Repeat("AB", 32)
	for native, want := range map[string]string{
		`{}`:                   "native.root",
		`{"root": "/srv/srg"}`: "runner_version",
		`{"root": "/srv/srg", "runner_version": "2.338.2", "runner_sha256": "abc"}`: "runner_sha256",
	} {
		if _, err := Parse([]byte(strings.Replace(base, "NATIVE", native, 1))); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("native %s: want %q, got %v", native, want, err)
		}
	}
	cfg, err := Parse([]byte(strings.Replace(base, "NATIVE", `{"root": "/srv/srg", "runner_version": "2.338.2", "runner_sha256": "`+sha+`"}`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Native.RunnerSHA256 != strings.ToLower(sha) {
		t.Error("sha256 not lower-cased")
	}
}

func TestDuplicateScaleSetName(t *testing.T) {
	dup := strings.Replace(validDocker, `"scale_sets": [`, `"scale_sets": [
	  {"name": "srg-vps", "url": "https://github.com/other", "runner_group": "g", "backend": "docker", "image": "i"},`, 1)
	if _, err := Parse([]byte(dup)); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate error, got %v", err)
	}
}

func TestParseGitHubURL(t *testing.T) {
	if o, r, err := ParseGitHubURL("https://github.com/nexisltd/"); err != nil || o != "nexisltd" || r != "" {
		t.Errorf("org url: %q %q %v", o, r, err)
	}
	if o, r, err := ParseGitHubURL("https://github.com/nexisltd/home-pbx"); err != nil || o != "nexisltd" || r != "home-pbx" {
		t.Errorf("repo url: %q %q %v", o, r, err)
	}
	for _, bad := range []string{"", "nexisltd", "https://github.com/", "https://github.com/a/b/c"} {
		if _, _, err := ParseGitHubURL(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestReadSecretFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pat")
	os.WriteFile(p, []byte("  s3cret\n"), 0o600)
	if s, err := ReadSecretFile(p); err != nil || s != "s3cret" {
		t.Errorf("got %q %v", s, err)
	}
	os.WriteFile(p, []byte("\n"), 0o600)
	if _, err := ReadSecretFile(p); err == nil {
		t.Error("empty secret must fail")
	}
	if _, err := ReadSecretFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing secret must fail")
	}
}

func TestDurationJSON(t *testing.T) {
	var d Duration
	if err := d.UnmarshalJSON([]byte(`"90s"`)); err != nil || d.Duration != 90*time.Second {
		t.Errorf("string: %v %v", d, err)
	}
	if err := d.UnmarshalJSON([]byte(`2.5`)); err != nil || d.Duration != 2500*time.Millisecond {
		t.Errorf("number: %v %v", d, err)
	}
	if err := d.UnmarshalJSON([]byte(`true`)); err == nil {
		t.Error("bool must fail")
	}
}
