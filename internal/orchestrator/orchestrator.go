// Package orchestrator implements the main polling loop that monitors
// GitHub Actions for queued jobs, launches ephemeral runner containers,
// and manages their lifecycle. It supports multiple orgs and repos.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/emon5122/scalerunnergh/internal/config"
	gh "github.com/emon5122/scalerunnergh/internal/github"
	"github.com/emon5122/scalerunnergh/internal/metrics"
	"github.com/emon5122/scalerunnergh/internal/resource"
	"github.com/emon5122/scalerunnergh/internal/runner"
)

// Orchestrator is the main control loop that polls for queued jobs
// and launches ephemeral runners.
type Orchestrator struct {
	cfg      *config.Config
	ghClient *gh.Client
	launcher *runner.Launcher
	resMgr   *resource.Manager
	metrics  *metrics.Collector
	logger   *slog.Logger

	// Track which jobs we've already dispatched a runner for.
	mu             sync.Mutex
	dispatchedJobs map[int64]string // jobID -> containerID

	// Callbacks for testing
	onJobDispatched func(jobID int64, containerID string)
	onJobCompleted  func(jobID int64, exitCode int)
}

// New creates a new orchestrator.
func New(
	cfg *config.Config,
	ghClient *gh.Client,
	launcher *runner.Launcher,
	resMgr *resource.Manager,
	met *metrics.Collector,
	logger *slog.Logger,
) *Orchestrator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Orchestrator{
		cfg:            cfg,
		ghClient:       ghClient,
		launcher:       launcher,
		resMgr:         resMgr,
		metrics:        met,
		logger:         logger,
		dispatchedJobs: make(map[int64]string),
	}
}

// Run starts the orchestrator loop. It blocks until ctx is cancelled.
func (o *Orchestrator) Run(ctx context.Context) error {
	o.logger.Info("orchestrator starting",
		"repos", len(o.cfg.Repos),
		"poll_interval", o.cfg.PollInterval.Duration)

	ticker := time.NewTicker(o.cfg.PollInterval.Duration)
	defer ticker.Stop()

	// Run immediately on start
	o.poll(ctx)

	for {
		select {
		case <-ctx.Done():
			o.logger.Info("orchestrator shutting down")
			return ctx.Err()
		case <-ticker.C:
			o.poll(ctx)
		}
	}
}

// RunOnce performs a single polling cycle. Useful for testing.
func (o *Orchestrator) RunOnce(ctx context.Context) {
	o.poll(ctx)
}

// poll iterates over all configured repos, finds queued jobs, and launches runners.
func (o *Orchestrator) poll(ctx context.Context) {
	var totalQueued int64

	for _, repo := range o.cfg.Repos {
		select {
		case <-ctx.Done():
			return
		default:
		}

		pat := o.cfg.EffectivePAT(repo)
		jobs, err := o.fetchQueuedJobs(ctx, pat, repo)
		if err != nil {
			o.logger.Error("failed to fetch queued jobs",
				"repo", repo.FullName(), "error", err)
			o.metrics.PollErrors.Add(1)
			continue
		}

		repoQueued := int64(0)
		for _, job := range jobs {
			// Skip if already dispatched
			if o.isDispatched(job.ID) {
				repoQueued++
				continue
			}

			// Check label match
			if !gh.MatchesLabels(job.Labels, repo.Labels) {
				o.logger.Debug("job labels don't match",
					"job_id", job.ID, "job_labels", job.Labels,
					"runner_labels", repo.Labels)
				continue
			}

			repoQueued++

			// Check capacity
			gpus := 0
			if o.cfg.Docker.GPUEnabled {
				gpus = 1 // each GPU job gets 1 GPU by default
			}
			if !o.resMgr.CanAllocate(gpus, 0, 0) {
				o.logger.Warn("no capacity for job",
					"job_id", job.ID, "repo", repo.FullName())
				continue
			}

			// Launch runner
			o.launchRunner(ctx, repo, job, pat, gpus)
		}

		o.metrics.SetRepoQueuedJobs(repo.FullName(), repoQueued)
		totalQueued += repoQueued
	}

	o.metrics.QueuedJobs.Store(totalQueued)
	o.metrics.ActiveRunners.Store(int64(o.resMgr.ActiveCount()))
	snap := o.resMgr.Snapshot()
	o.metrics.UsedGPUs.Store(int64(snap.UsedGPUs))
	o.metrics.AvailableGPUs.Store(int64(snap.MaxGPUs - snap.UsedGPUs))
}

// fetchQueuedJobs retrieves queued jobs for a repo (or org).
func (o *Orchestrator) fetchQueuedJobs(ctx context.Context, pat string, repo config.RepoConfig) ([]gh.WorkflowJob, error) {
	if repo.Repo == "" {
		// Org-level: for now return empty (org-level job listing requires different API)
		return o.ghClient.ListOrgQueuedJobs(ctx, pat, repo.Owner)
	}
	return o.ghClient.ListQueuedJobs(ctx, pat, repo.Owner, repo.Repo)
}

// launchRunner creates a registration token, reserves resources, and starts a container.
func (o *Orchestrator) launchRunner(ctx context.Context, repo config.RepoConfig, job gh.WorkflowJob, pat string, gpus int) {
	o.logger.Info("launching runner for job",
		"job_id", job.ID, "job_name", job.Name, "repo", repo.FullName())

	// Get registration token
	var token string
	var err error
	if repo.Repo == "" {
		tok, e := o.ghClient.CreateOrgRegistrationToken(ctx, pat, repo.Owner)
		if e != nil {
			o.logger.Error("failed to create org token", "error", e)
			o.metrics.TokenErrors.Add(1)
			return
		}
		token = tok.Token
	} else {
		tok, e := o.ghClient.CreateRepoRegistrationToken(ctx, pat, repo.Owner, repo.Repo)
		if e != nil {
			o.logger.Error("failed to create repo token",
				"repo", repo.FullName(), "error", e)
			o.metrics.TokenErrors.Add(1)
			return
		}
		token = tok.Token
	}

	// Reserve resources
	allocID := fmt.Sprintf("job-%d", job.ID)
	if err = o.resMgr.Allocate(allocID, gpus, 0, 0); err != nil {
		o.logger.Error("failed to allocate resources",
			"job_id", job.ID, "error", err)
		return
	}

	// Launch container
	info, err := o.launcher.Launch(ctx, runner.LaunchRequest{
		Owner:       repo.Owner,
		Repo:        repo.Repo,
		JobID:       job.ID,
		Token:       token,
		Labels:      repo.Labels,
		Image:       o.cfg.Docker.Image,
		GPUEnabled:  o.cfg.Docker.GPUEnabled,
		GPUs:        gpus,
		NetworkMode: o.cfg.Docker.NetworkMode,
		ExtraArgs:   o.cfg.Docker.ExtraArgs,
	})
	if err != nil {
		o.logger.Error("failed to launch runner",
			"job_id", job.ID, "error", err)
		o.metrics.LaunchErrors.Add(1)
		o.resMgr.Release(allocID)
		return
	}

	o.markDispatched(job.ID, info.ID)
	o.metrics.TotalLaunched.Add(1)

	if o.onJobDispatched != nil {
		o.onJobDispatched(job.ID, info.ID)
	}

	// Monitor container lifecycle in background
	go o.monitorContainer(context.Background(), info.ID, job.ID, allocID)
}

// monitorContainer waits for a container to exit and cleans up resources.
func (o *Orchestrator) monitorContainer(ctx context.Context, containerID string, jobID int64, allocID string) {
	exitCode, err := o.launcher.WaitAndCleanup(ctx, containerID)

	// Release resources
	if relErr := o.resMgr.Release(allocID); relErr != nil {
		o.logger.Error("failed to release resources",
			"alloc_id", allocID, "error", relErr)
	}

	// Clean up dispatch tracking
	o.clearDispatched(jobID)
	o.launcher.RemoveTracking(containerID)

	if err != nil || exitCode != 0 {
		o.metrics.TotalFailed.Add(1)
		o.logger.Warn("runner completed with error",
			"container_id", containerID, "job_id", jobID,
			"exit_code", exitCode, "error", err)
	} else {
		o.metrics.TotalCompleted.Add(1)
		o.logger.Info("runner completed successfully",
			"container_id", containerID, "job_id", jobID)
	}

	if o.onJobCompleted != nil {
		o.onJobCompleted(jobID, exitCode)
	}
}

func (o *Orchestrator) isDispatched(jobID int64) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.dispatchedJobs[jobID]
	return ok
}

func (o *Orchestrator) markDispatched(jobID int64, containerID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.dispatchedJobs[jobID] = containerID
}

func (o *Orchestrator) clearDispatched(jobID int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.dispatchedJobs, jobID)
}

// DispatchedCount returns the number of currently dispatched jobs.
func (o *Orchestrator) DispatchedCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.dispatchedJobs)
}
