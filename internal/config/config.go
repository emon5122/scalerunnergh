// Package config provides configuration loading and validation for the
// scalerunnergh orchestrator. It supports multi-org and multi-repo setups,
// per-repo label filtering, and resource limits.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// RepoConfig represents a single repository (or org-level) configuration.
type RepoConfig struct {
	// Owner is the GitHub org or user, e.g. "my-org".
	Owner string `json:"owner"`
	// Repo is the repository name. Empty string means org-level runner.
	Repo string `json:"repo,omitempty"`
	// Labels that runners created for this repo will claim, e.g. ["gpu","docker"].
	Labels []string `json:"labels"`
	// PAT is a GitHub Personal Access Token (or fine-grained token) with admin
	// access to the repo/org runners. If empty, the global token is used.
	PAT string `json:"pat,omitempty"`
}

// FullName returns "owner/repo" or "owner" for org-level configs.
func (r RepoConfig) FullName() string {
	if r.Repo == "" {
		return r.Owner
	}
	return r.Owner + "/" + r.Repo
}

// ResourceLimits describes the host resources the orchestrator may use.
type ResourceLimits struct {
	MaxRunners int `json:"max_runners"` // max concurrent runner containers
	MaxGPUs    int `json:"max_gpus"`    // total GPUs available on host
	MaxCPUs    int `json:"max_cpus"`    // max CPUs to allocate across runners
	MaxMemMB   int `json:"max_mem_mb"`  // max memory in MB across runners
}

// DockerConfig holds Docker-specific settings for runner containers.
type DockerConfig struct {
	Image       string   `json:"image"`                  // runner image, e.g. "ghcr.io/actions/runner:latest"
	NetworkMode string   `json:"network_mode,omitempty"` // e.g. "bridge", "host"
	ExtraArgs   []string `json:"extra_args,omitempty"`   // additional docker run flags
	GPUEnabled  bool     `json:"gpu_enabled"`            // pass --gpus all
}

// MetricsConfig configures the Prometheus metrics endpoint.
type MetricsConfig struct {
	Enabled bool   `json:"enabled"`
	Address string `json:"address"` // e.g. ":9090"
}

// Config is the top-level configuration for the orchestrator.
type Config struct {
	// Repos lists every org/repo the orchestrator manages.
	Repos []RepoConfig `json:"repos"`
	// GlobalPAT is used when a repo-specific PAT is not set.
	GlobalPAT string `json:"global_pat,omitempty"`
	// PollInterval is how often to check the GitHub API for queued jobs.
	PollInterval Duration `json:"poll_interval"`
	// Resources describes host resource limits.
	Resources ResourceLimits `json:"resources"`
	// Docker holds container settings.
	Docker DockerConfig `json:"docker"`
	// Metrics configures the Prometheus endpoint.
	Metrics MetricsConfig `json:"metrics"`
	// LogLevel: "debug", "info", "warn", "error".
	LogLevel string `json:"log_level"`
}

// Duration wraps time.Duration to support JSON string unmarshalling.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		// try as number (seconds)
		var n float64
		if err2 := json.Unmarshal(b, &n); err2 != nil {
			return fmt.Errorf("duration must be a string or number: %w", err)
		}
		d.Duration = time.Duration(n * float64(time.Second))
		return nil
	}
	var err error
	d.Duration, err = time.ParseDuration(s)
	return err
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Duration.String())
}

// Validate checks the config for obvious problems.
func (c *Config) Validate() error {
	if len(c.Repos) == 0 {
		return errors.New("config: at least one repo must be configured")
	}
	for i, r := range c.Repos {
		if r.Owner == "" {
			return fmt.Errorf("config: repos[%d].owner is required", i)
		}
		pat := r.PAT
		if pat == "" {
			pat = c.GlobalPAT
		}
		if pat == "" {
			return fmt.Errorf("config: repos[%d] (%s) has no PAT and no global_pat", i, r.FullName())
		}
	}
	if c.PollInterval.Duration <= 0 {
		c.PollInterval.Duration = 10 * time.Second
	}
	if c.Resources.MaxRunners <= 0 {
		c.Resources.MaxRunners = 5
	}
	if c.Docker.Image == "" {
		c.Docker.Image = "ghcr.io/actions/runner:latest"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	return nil
}

// EffectivePAT returns the PAT to use for a given repo config.
func (c *Config) EffectivePAT(r RepoConfig) string {
	if r.PAT != "" {
		return r.PAT
	}
	return c.GlobalPAT
}

// LoadFile reads a config JSON file from disk.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read file: %w", err)
	}
	return Parse(data)
}

// Parse parses raw JSON bytes into a Config, applying defaults and validation.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// LoadFromEnv constructs a minimal config from environment variables.
// Useful for simple single-repo setups.
//
// Env vars: GITHUB_PAT, GITHUB_OWNER, GITHUB_REPO, RUNNER_LABELS,
// POLL_INTERVAL, MAX_RUNNERS, MAX_GPUS, RUNNER_IMAGE, GPU_ENABLED,
// METRICS_ADDR, LOG_LEVEL.
func LoadFromEnv() (*Config, error) {
	pat := os.Getenv("GITHUB_PAT")
	owner := os.Getenv("GITHUB_OWNER")
	if pat == "" || owner == "" {
		return nil, errors.New("config: GITHUB_PAT and GITHUB_OWNER are required")
	}
	repo := os.Getenv("GITHUB_REPO")

	labels := strings.Split(os.Getenv("RUNNER_LABELS"), ",")
	if len(labels) == 1 && labels[0] == "" {
		labels = []string{"self-hosted"}
	}

	cfg := Config{
		Repos: []RepoConfig{{
			Owner:  owner,
			Repo:   repo,
			Labels: labels,
		}},
		GlobalPAT:    pat,
		PollInterval: Duration{10 * time.Second},
		Resources: ResourceLimits{
			MaxRunners: envInt("MAX_RUNNERS", 5),
			MaxGPUs:    envInt("MAX_GPUS", 0),
			MaxCPUs:    envInt("MAX_CPUS", 0),
			MaxMemMB:   envInt("MAX_MEM_MB", 0),
		},
		Docker: DockerConfig{
			Image:      envStr("RUNNER_IMAGE", "ghcr.io/actions/runner:latest"),
			GPUEnabled: os.Getenv("GPU_ENABLED") == "true",
		},
		Metrics: MetricsConfig{
			Enabled: os.Getenv("METRICS_ADDR") != "",
			Address: envStr("METRICS_ADDR", ":9090"),
		},
		LogLevel: envStr("LOG_LEVEL", "info"),
	}

	if s := os.Getenv("POLL_INTERVAL"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("config: POLL_INTERVAL: %w", err)
		}
		cfg.PollInterval.Duration = d
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	s := os.Getenv(key)
	if s == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return def
	}
	return n
}
