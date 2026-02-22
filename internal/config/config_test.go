package config

import (
	"os"
	"testing"
	"time"
)

func TestParse_ValidConfig(t *testing.T) {
	data := []byte(`{
		"repos": [
			{"owner": "acme", "repo": "app", "labels": ["gpu","docker"]},
			{"owner": "acme", "repo": "", "labels": ["cpu"]}
		],
		"global_pat": "ghp_test123",
		"poll_interval": "15s",
		"resources": {"max_runners": 10, "max_gpus": 4, "max_cpus": 16, "max_mem_mb": 32768},
		"docker": {"image": "myrunner:latest", "gpu_enabled": true},
		"metrics": {"enabled": true, "address": ":9091"},
		"log_level": "debug"
	}`)

	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(cfg.Repos))
	}
	if cfg.Repos[0].FullName() != "acme/app" {
		t.Errorf("expected acme/app, got %s", cfg.Repos[0].FullName())
	}
	if cfg.Repos[1].FullName() != "acme" {
		t.Errorf("expected acme, got %s", cfg.Repos[1].FullName())
	}
	if cfg.PollInterval.Duration != 15*time.Second {
		t.Errorf("expected 15s poll, got %v", cfg.PollInterval.Duration)
	}
	if cfg.Resources.MaxRunners != 10 {
		t.Errorf("expected 10 max runners, got %d", cfg.Resources.MaxRunners)
	}
	if cfg.Resources.MaxGPUs != 4 {
		t.Errorf("expected 4 max gpus, got %d", cfg.Resources.MaxGPUs)
	}
	if !cfg.Docker.GPUEnabled {
		t.Error("expected gpu_enabled true")
	}
	if cfg.Docker.Image != "myrunner:latest" {
		t.Errorf("expected myrunner:latest, got %s", cfg.Docker.Image)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("expected debug, got %s", cfg.LogLevel)
	}
}

func TestParse_Defaults(t *testing.T) {
	data := []byte(`{
		"repos": [{"owner": "org1", "repo": "repo1", "labels": ["self-hosted"]}],
		"global_pat": "ghp_abc"
	}`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PollInterval.Duration != 10*time.Second {
		t.Errorf("expected default 10s, got %v", cfg.PollInterval.Duration)
	}
	if cfg.Resources.MaxRunners != 5 {
		t.Errorf("expected default 5 runners, got %d", cfg.Resources.MaxRunners)
	}
	if cfg.Docker.Image != "ghcr.io/actions/runner:latest" {
		t.Errorf("expected default image, got %s", cfg.Docker.Image)
	}
}

func TestParse_NoRepos(t *testing.T) {
	data := []byte(`{"repos": [], "global_pat": "ghp_x"}`)
	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected error for empty repos")
	}
}

func TestParse_NoPAT(t *testing.T) {
	data := []byte(`{"repos": [{"owner": "org1", "repo": "r"}]}`)
	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected error for missing PAT")
	}
}

func TestParse_DurationAsNumber(t *testing.T) {
	data := []byte(`{
		"repos": [{"owner": "org1", "repo": "r", "labels": ["x"]}],
		"global_pat": "ghp_abc",
		"poll_interval": 30
	}`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PollInterval.Duration != 30*time.Second {
		t.Errorf("expected 30s, got %v", cfg.PollInterval.Duration)
	}
}

func TestRepoConfig_PerRepoPAT(t *testing.T) {
	data := []byte(`{
		"repos": [{"owner": "org1", "repo": "r", "labels": ["x"], "pat": "ghp_repo"}],
		"global_pat": "ghp_global"
	}`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.EffectivePAT(cfg.Repos[0]) != "ghp_repo" {
		t.Errorf("expected per-repo PAT")
	}
}

func TestEffectivePAT_FallbackGlobal(t *testing.T) {
	data := []byte(`{
		"repos": [{"owner": "org1", "repo": "r", "labels": ["x"]}],
		"global_pat": "ghp_global"
	}`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.EffectivePAT(cfg.Repos[0]) != "ghp_global" {
		t.Errorf("expected global PAT fallback")
	}
}

func TestLoadFile(t *testing.T) {
	tmp, err := os.CreateTemp("", "config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())

	content := `{
		"repos": [{"owner": "org1", "repo": "r1", "labels": ["self-hosted"]}],
		"global_pat": "ghp_file"
	}`
	if _, err := tmp.WriteString(content); err != nil {
		t.Fatal(err)
	}
	tmp.Close()

	cfg, err := LoadFile(tmp.Name())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GlobalPAT != "ghp_file" {
		t.Errorf("expected ghp_file, got %s", cfg.GlobalPAT)
	}
}

func TestLoadFromEnv(t *testing.T) {
	os.Setenv("GITHUB_PAT", "ghp_envtest")
	os.Setenv("GITHUB_OWNER", "envorg")
	os.Setenv("GITHUB_REPO", "envrepo")
	os.Setenv("RUNNER_LABELS", "gpu,docker")
	os.Setenv("MAX_RUNNERS", "3")
	os.Setenv("GPU_ENABLED", "true")
	defer func() {
		os.Unsetenv("GITHUB_PAT")
		os.Unsetenv("GITHUB_OWNER")
		os.Unsetenv("GITHUB_REPO")
		os.Unsetenv("RUNNER_LABELS")
		os.Unsetenv("MAX_RUNNERS")
		os.Unsetenv("GPU_ENABLED")
	}()

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Repos) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(cfg.Repos))
	}
	if cfg.Repos[0].FullName() != "envorg/envrepo" {
		t.Errorf("expected envorg/envrepo, got %s", cfg.Repos[0].FullName())
	}
	if cfg.Resources.MaxRunners != 3 {
		t.Errorf("expected 3 runners, got %d", cfg.Resources.MaxRunners)
	}
	if !cfg.Docker.GPUEnabled {
		t.Error("expected gpu enabled")
	}
}

func TestDuration_MarshalJSON(t *testing.T) {
	d := Duration{5 * time.Second}
	b, err := d.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"5s"` {
		t.Errorf("expected \"5s\", got %s", string(b))
	}
}
