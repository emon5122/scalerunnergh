// Package config loads and validates the scalerunnergh host configuration.
//
// One config file describes one runner host: its shared capacity (slot pool,
// load gate, kill switch), the backends it offers (docker, native) and the
// runner scale sets it serves. Credentials are never inline: the config
// only names files (Docker/systemd secrets, an ACL'd file on Windows).
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Backend names accepted in ScaleSetConfig.Backend.
const (
	BackendDocker = "docker"
	BackendNative = "native"
)

// Config is the top-level configuration for one host.
type Config struct {
	Host      HostConfig          `json:"host"`
	Auth      *AuthConfig         `json:"auth,omitempty"` // default for every scale set
	Docker    DockerBackendConfig `json:"docker"`
	Native    NativeBackendConfig `json:"native"`
	ScaleSets []ScaleSetConfig    `json:"scale_sets"`
	Metrics   MetricsConfig       `json:"metrics"`
	LogLevel  string              `json:"log_level"`
}

// HostConfig is the capacity shared by every scale set on this host.
type HostConfig struct {
	// Name identifies this host in runner names and container labels.
	Name string `json:"name"`
	// MaxRunners is the total number of concurrent runners on this host.
	MaxRunners int `json:"max_runners"`
	// MaxHeavy caps concurrent runners of scale sets marked heavy (0 = no extra cap).
	MaxHeavy int `json:"max_heavy"`
	// MaxLoadPerCore: no new runner starts while load1/cores is above it (0 = off, Linux only).
	MaxLoadPerCore float64 `json:"max_load_per_core"`
	// MinMemAvailableMB: no new runner starts while MemAvailable is below it (0 = off, Linux only).
	MinMemAvailableMB int `json:"min_mem_available_mb"`
	// KillSwitchFile: while this file exists the host takes no new jobs.
	KillSwitchFile string `json:"kill_switch_file"`
	// KillSwitchStopsRunning also stops running jobs when the kill switch appears.
	KillSwitchStopsRunning bool `json:"kill_switch_stops_running"`
	// IdleTimeout removes surplus idle runners after this long (default 3m).
	IdleTimeout Duration `json:"idle_timeout"`
	// MaxJobDuration force-stops a runner after this long (default 6h30m).
	MaxJobDuration Duration `json:"max_job_duration"`
	// DrainTimeout: on shutdown (SIGTERM, service stop, image update) take no
	// new jobs and let running jobs finish for up to this long before
	// stopping them (default 0 = stop at once). Give the service manager at
	// least this much stop time.
	DrainTimeout Duration `json:"drain_timeout"`
}

// AuthConfig names the credential files for one GitHub org or repo.
// Exactly one of PATFile or App must be set.
type AuthConfig struct {
	PATFile string   `json:"pat_file,omitempty"`
	App     *AppAuth `json:"app,omitempty"`
}

// AppAuth is a GitHub App installation.
type AppAuth struct {
	ClientID       string `json:"client_id"`
	InstallationID int64  `json:"installation_id"`
	PrivateKeyFile string `json:"private_key_file"`
}

// Limits are per-runner resource caps. Zero means "not set".
type Limits struct {
	CPUs        float64 `json:"cpus,omitempty"`
	MemoryMB    int     `json:"memory_mb,omitempty"`
	Pids        int     `json:"pids,omitempty"`
	CPUShares   int     `json:"cpu_shares,omitempty"`
	BlkioWeight int     `json:"blkio_weight,omitempty"`
	OOMScoreAdj int     `json:"oom_score_adj,omitempty"`
	StorageSize string  `json:"storage_opt_size,omitempty"`
	// GPUs is passed to docker --gpus ("all", "1", "device=0").
	GPUs string `json:"gpus,omitempty"`
	// Nice is the niceness of native runners on Linux (0-19). On Windows any
	// value > 0 selects the below-normal priority class.
	Nice int `json:"nice,omitempty"`
}

// ScaleSetConfig is one runner scale set served by this host.
type ScaleSetConfig struct {
	// Name of the scale set; it is also a runs-on label.
	Name string `json:"name"`
	// URL of the org (https://github.com/my-org) or repo (https://github.com/my-org/my-repo).
	URL string `json:"url"`
	// RunnerGroup the scale set lives in. It must not allow public repositories.
	RunnerGroup string `json:"runner_group"`
	// Labels are extra runs-on labels besides Name (e.g. ["vps", "Linux"]).
	Labels []string `json:"labels,omitempty"`
	// Backend is "docker" or "native".
	Backend string `json:"backend"`
	// MinRunners are kept idle ahead of demand.
	MinRunners int `json:"min_runners"`
	// MaxRunners caps this scale set (the host pool caps the total).
	MaxRunners int `json:"max_runners"`
	// Heavy scale sets also count against host.max_heavy.
	Heavy bool `json:"heavy"`
	// Auth overrides the host-level auth (an App installation is per org).
	Auth *AuthConfig `json:"auth,omitempty"`
	// Image is the runner image (docker backend).
	Image string `json:"image,omitempty"`
	// DinD starts a private dockerd inside the job container (docker backend).
	DinD bool `json:"dind,omitempty"`
	// Limits per runner.
	Limits Limits `json:"limits"`
	// AllowRepos optionally restricts jobs to these repos ("repo" or "owner/repo").
	AllowRepos []string `json:"allow_repos,omitempty"`
}

// DockerBackendConfig holds docker backend settings shared by its scale sets.
type DockerBackendConfig struct {
	// Binary is the docker CLI (default "docker").
	Binary string `json:"binary,omitempty"`
	// Network for runner containers (default: docker's default bridge).
	Network string `json:"network,omitempty"`
	// Runtime, e.g. "sysbox-runc" (default: docker's default).
	Runtime string `json:"runtime,omitempty"`
	// RegistryMirror is passed to the dockerd inside dind runners.
	RegistryMirror string `json:"registry_mirror,omitempty"`
	// ExtraArgs are appended to docker create (escape hatch).
	ExtraArgs []string `json:"extra_args,omitempty"`
}

// NativeBackendConfig holds native (process) backend settings.
type NativeBackendConfig struct {
	// Root holds the runner release, per-job dirs and logs.
	Root string `json:"root,omitempty"`
	// RunnerVersion of actions/runner, e.g. "2.338.2".
	RunnerVersion string `json:"runner_version,omitempty"`
	// RunnerSHA256 of the release archive for this OS/arch.
	RunnerSHA256 string `json:"runner_sha256,omitempty"`
	// RunnerURL overrides the download URL (mirrors, tests).
	RunnerURL string `json:"runner_url,omitempty"`
}

// MetricsConfig configures the Prometheus/health endpoint.
type MetricsConfig struct {
	Enabled bool   `json:"enabled"`
	Address string `json:"address"`
}

// Duration wraps time.Duration for JSON ("10s" or a number of seconds).
type Duration struct {
	time.Duration
}

// UnmarshalJSON accepts "1m30s" or a number of seconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
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

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Duration.String())
}

var (
	nameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,44}$`)
	sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// LoadFile reads and validates a config file.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read file: %w", err)
	}
	return Parse(data)
}

// Parse parses JSON, applies defaults and validates. Unknown fields are errors,
// so a typo never silently drops a limit.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate applies defaults and checks the config.
func (c *Config) Validate() error {
	if c.Host.Name == "" {
		h, _ := os.Hostname()
		c.Host.Name = h
	}
	c.Host.Name = SanitizeName(c.Host.Name)
	if c.Host.Name == "" {
		return errors.New("config: host.name is required")
	}
	if c.Host.MaxRunners <= 0 {
		return errors.New("config: host.max_runners must be > 0")
	}
	if c.Host.MaxHeavy < 0 || c.Host.MaxHeavy > c.Host.MaxRunners {
		return errors.New("config: host.max_heavy must be between 0 and host.max_runners")
	}
	if c.Host.MaxLoadPerCore < 0 || c.Host.MinMemAvailableMB < 0 {
		return errors.New("config: host load gate values must be >= 0")
	}
	if c.Host.IdleTimeout.Duration <= 0 {
		c.Host.IdleTimeout.Duration = 3 * time.Minute
	}
	if c.Host.MaxJobDuration.Duration <= 0 {
		c.Host.MaxJobDuration.Duration = 6*time.Hour + 30*time.Minute
	}
	if c.Host.DrainTimeout.Duration < 0 {
		return errors.New("config: host.drain_timeout must be >= 0")
	}
	if c.Docker.Binary == "" {
		c.Docker.Binary = "docker"
	}
	if c.Metrics.Address == "" {
		c.Metrics.Address = ":9090"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if len(c.ScaleSets) == 0 {
		return errors.New("config: at least one scale set is required")
	}

	seen := map[string]bool{}
	usesNative := false
	for i := range c.ScaleSets {
		s := &c.ScaleSets[i]
		where := fmt.Sprintf("config: scale_sets[%d]", i)
		if !nameRe.MatchString(s.Name) {
			return fmt.Errorf("%s: name %q must match %s", where, s.Name, nameRe)
		}
		if seen[s.Name] {
			return fmt.Errorf("%s: duplicate scale set name %q on this host", where, s.Name)
		}
		seen[s.Name] = true
		if _, _, err := ParseGitHubURL(s.URL); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if s.RunnerGroup == "" {
			return fmt.Errorf("%s: runner_group is required (a private group that refuses public repos)", where)
		}
		if s.MaxRunners <= 0 || s.MaxRunners > c.Host.MaxRunners {
			s.MaxRunners = c.Host.MaxRunners
		}
		if s.MinRunners < 0 || s.MinRunners > s.MaxRunners {
			return fmt.Errorf("%s: min_runners must be between 0 and max_runners", where)
		}
		if s.Heavy && c.Host.MaxHeavy == 0 {
			return fmt.Errorf("%s: a heavy scale set needs host.max_heavy > 0", where)
		}
		if err := c.EffectiveAuth(*s).validate(); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if err := s.Limits.validate(); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		switch s.Backend {
		case BackendDocker:
			if runtime.GOOS != "linux" {
				return fmt.Errorf("%s: the docker backend runs on Linux hosts only", where)
			}
			if s.Image == "" {
				return fmt.Errorf("%s: image is required for the docker backend", where)
			}
		case BackendNative:
			usesNative = true
			if s.DinD || s.Image != "" || s.Limits.GPUs != "" {
				return fmt.Errorf("%s: image, dind and gpus are docker-backend settings", where)
			}
		default:
			return fmt.Errorf("%s: backend must be %q or %q", where, BackendDocker, BackendNative)
		}
	}
	if usesNative {
		if c.Native.Root == "" {
			return errors.New("config: native.root is required for native scale sets")
		}
		if c.Native.RunnerVersion == "" {
			return errors.New("config: native.runner_version is required for native scale sets")
		}
		c.Native.RunnerSHA256 = strings.ToLower(c.Native.RunnerSHA256)
		if !sha256Re.MatchString(c.Native.RunnerSHA256) {
			return errors.New("config: native.runner_sha256 must be the 64-hex sha256 of the runner archive")
		}
	}
	return nil
}

// EffectiveAuth returns the auth for a scale set (its own, else the host default).
func (c *Config) EffectiveAuth(s ScaleSetConfig) *AuthConfig {
	if s.Auth != nil {
		return s.Auth
	}
	return c.Auth
}

// UsesBackend reports whether any scale set uses the backend.
func (c *Config) UsesBackend(name string) bool {
	for _, s := range c.ScaleSets {
		if s.Backend == name {
			return true
		}
	}
	return false
}

func (a *AuthConfig) validate() error {
	if a == nil {
		return errors.New("auth is required (scale set auth or host-level auth)")
	}
	switch {
	case a.PATFile != "" && a.App != nil:
		return errors.New("auth: set either pat_file or app, not both")
	case a.PATFile != "":
		return nil
	case a.App != nil:
		if a.App.ClientID == "" || a.App.InstallationID == 0 || a.App.PrivateKeyFile == "" {
			return errors.New("auth.app needs client_id, installation_id and private_key_file")
		}
		return nil
	default:
		return errors.New("auth: pat_file or app is required")
	}
}

func (l Limits) validate() error {
	if l.CPUs < 0 || l.MemoryMB < 0 || l.Pids < 0 || l.CPUShares < 0 {
		return errors.New("limits must be >= 0")
	}
	if l.BlkioWeight != 0 && (l.BlkioWeight < 10 || l.BlkioWeight > 1000) {
		return errors.New("limits.blkio_weight must be 10-1000")
	}
	if l.OOMScoreAdj < 0 || l.OOMScoreAdj > 1000 {
		return errors.New("limits.oom_score_adj must be 0-1000 (CI jobs are never protected from the OOM killer)")
	}
	if l.Nice < 0 || l.Nice > 19 {
		return errors.New("limits.nice must be 0-19")
	}
	return nil
}

// ParseGitHubURL returns owner and repo ("" for an org) of a GitHub URL.
func ParseGitHubURL(raw string) (owner, repo string, err error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", "", fmt.Errorf("url %q must be https://github.com/<org>[/<repo>]", raw)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch {
	case len(parts) == 1 && parts[0] != "":
		return parts[0], "", nil
	case len(parts) == 2 && parts[0] != "" && parts[1] != "":
		return parts[0], parts[1], nil
	default:
		return "", "", fmt.Errorf("url %q must be https://github.com/<org>[/<repo>]", raw)
	}
}

// ReadSecretFile reads a credential file and trims surrounding whitespace.
// The error never contains the file content.
func ReadSecretFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret file: %w", err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("secret file %s is empty", path)
	}
	return s, nil
}

// SanitizeName keeps [A-Za-z0-9._-] so the result is safe for runner and container names.
func SanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
