// Package runner provides the Docker-based ephemeral runner launcher.
// It creates, monitors, and destroys runner containers.
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// ContainerInfo holds metadata about a launched runner container.
type ContainerInfo struct {
	ID          string
	Name        string
	Owner       string
	Repo        string
	JobID       int64
	Labels      []string
	StartedAt   time.Time
	GPUs        int
	CPUs        int
	MemMB       int
	Status      string // "running", "exited", "error"
	ExitCode    int
	CompletedAt time.Time
}

// DockerClient is an interface for the subset of Docker operations we need.
// This allows mocking in tests without a real Docker daemon.
type DockerClient interface {
	// CreateAndStart creates a container and starts it, returning the container ID.
	CreateAndStart(ctx context.Context, opts ContainerOpts) (string, error)
	// Wait blocks until the container exits and returns the exit code.
	Wait(ctx context.Context, containerID string) (int, error)
	// Remove forcefully removes a container.
	Remove(ctx context.Context, containerID string) error
	// IsRunning checks if a container is still running.
	IsRunning(ctx context.Context, containerID string) (bool, error)
}

// ContainerOpts holds the configuration for creating a runner container.
type ContainerOpts struct {
	Name        string
	Image       string
	Env         map[string]string
	Labels      map[string]string
	GPUEnabled  bool
	NetworkMode string
	ExtraArgs   []string
	AutoRemove  bool
}

// Launcher creates and manages ephemeral runner containers.
type Launcher struct {
	docker DockerClient
	logger *slog.Logger

	mu         sync.Mutex
	containers map[string]*ContainerInfo // containerID -> info
}

// NewLauncher creates a new runner launcher.
func NewLauncher(docker DockerClient, logger *slog.Logger) *Launcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Launcher{
		docker:     docker,
		logger:     logger,
		containers: make(map[string]*ContainerInfo),
	}
}

// LaunchRequest contains all the information needed to launch a runner.
type LaunchRequest struct {
	Owner       string
	Repo        string // empty for org-level runner
	JobID       int64
	Token       string // registration token
	Labels      []string
	Image       string
	GPUEnabled  bool
	GPUs        int
	CPUs        int
	MemMB       int
	NetworkMode string
	ExtraArgs   []string
	RepoURL     string // e.g. "https://github.com/org/repo"
}

// Launch creates and starts a new ephemeral runner container.
// It returns the container info and starts a goroutine to wait for completion.
func (l *Launcher) Launch(ctx context.Context, req LaunchRequest) (*ContainerInfo, error) {
	name := fmt.Sprintf("scalerunner-%s-%s-%d-%d",
		req.Owner, req.Repo, req.JobID, time.Now().UnixNano()%100000)
	name = sanitizeName(name)

	repoURL := req.RepoURL
	if repoURL == "" {
		if req.Repo != "" {
			repoURL = fmt.Sprintf("https://github.com/%s/%s", req.Owner, req.Repo)
		} else {
			repoURL = fmt.Sprintf("https://github.com/%s", req.Owner)
		}
	}

	env := map[string]string{
		"RUNNER_TOKEN":       req.Token,
		"RUNNER_REPOSITORY":  repoURL,
		"RUNNER_LABELS":      strings.Join(req.Labels, ","),
		"RUNNER_EPHEMERAL":   "true",
		"RUNNER_NAME":        name,
		"RUNNER_WORK_FOLDER": "/_work",
	}
	// For org-level registration
	if req.Repo == "" {
		env["RUNNER_ORGANIZATION"] = req.Owner
		delete(env, "RUNNER_REPOSITORY")
	}

	containerLabels := map[string]string{
		"scalerunner.owner":  req.Owner,
		"scalerunner.repo":   req.Repo,
		"scalerunner.job_id": fmt.Sprintf("%d", req.JobID),
		"scalerunner":        "true",
	}

	opts := ContainerOpts{
		Name:        name,
		Image:       req.Image,
		Env:         env,
		Labels:      containerLabels,
		GPUEnabled:  req.GPUEnabled,
		NetworkMode: req.NetworkMode,
		ExtraArgs:   req.ExtraArgs,
		AutoRemove:  true,
	}

	l.logger.Info("launching runner container",
		"name", name, "owner", req.Owner, "repo", req.Repo,
		"job_id", req.JobID, "image", req.Image, "gpu", req.GPUEnabled)

	containerID, err := l.docker.CreateAndStart(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("runner launch: %w", err)
	}

	info := &ContainerInfo{
		ID:        containerID,
		Name:      name,
		Owner:     req.Owner,
		Repo:      req.Repo,
		JobID:     req.JobID,
		Labels:    req.Labels,
		StartedAt: time.Now(),
		GPUs:      req.GPUs,
		CPUs:      req.CPUs,
		MemMB:     req.MemMB,
		Status:    "running",
	}

	l.mu.Lock()
	l.containers[containerID] = info
	l.mu.Unlock()

	return info, nil
}

// WaitAndCleanup blocks until the container exits, updates its status, and
// returns the exit code. This is typically called in a goroutine.
func (l *Launcher) WaitAndCleanup(ctx context.Context, containerID string) (int, error) {
	exitCode, err := l.docker.Wait(ctx, containerID)

	l.mu.Lock()
	info, ok := l.containers[containerID]
	if ok {
		info.ExitCode = exitCode
		info.CompletedAt = time.Now()
		if err != nil {
			info.Status = "error"
		} else {
			info.Status = "exited"
		}
	}
	l.mu.Unlock()

	if err != nil {
		l.logger.Error("runner container error",
			"container_id", containerID, "error", err)
		return exitCode, fmt.Errorf("wait container: %w", err)
	}

	l.logger.Info("runner container exited",
		"container_id", containerID, "exit_code", exitCode)

	// Attempt cleanup (container may already be removed if AutoRemove was set)
	if rmErr := l.docker.Remove(ctx, containerID); rmErr != nil {
		l.logger.Debug("container remove (may already be gone)",
			"container_id", containerID, "error", rmErr)
	}

	return exitCode, nil
}

// GetContainer returns info about a tracked container.
func (l *Launcher) GetContainer(containerID string) (*ContainerInfo, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	info, ok := l.containers[containerID]
	return info, ok
}

// RemoveTracking removes a container from the internal tracking map.
func (l *Launcher) RemoveTracking(containerID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.containers, containerID)
}

// ListContainers returns all tracked containers.
func (l *Launcher) ListContainers() []*ContainerInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	result := make([]*ContainerInfo, 0, len(l.containers))
	for _, c := range l.containers {
		result = append(result, c)
	}
	return result
}

// ActiveCount returns the number of running containers.
func (l *Launcher) ActiveCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, c := range l.containers {
		if c.Status == "running" {
			count++
		}
	}
	return count
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteRune(c)
		}
	}
	result := b.String()
	if len(result) > 63 {
		result = result[:63]
	}
	return result
}
