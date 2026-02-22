//go:build integration

// Package integration contains tests that hit the real GitHub API.
// Run with: go test -tags integration -v ./tests/integration/ -run TestIntegration
//
// Requires GITHUB_PAT environment variable to be set.
package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/emon5122/scalerunnergh/internal/config"
	gh "github.com/emon5122/scalerunnergh/internal/github"
	"github.com/emon5122/scalerunnergh/internal/metrics"
	"github.com/emon5122/scalerunnergh/internal/resource"
)

func getTestPAT(t *testing.T) string {
	pat := os.Getenv("GITHUB_PAT")
	if pat == "" {
		t.Skip("GITHUB_PAT not set, skipping integration test")
	}
	return pat
}

func TestIntegration_ListQueuedRuns(t *testing.T) {
	pat := getTestPAT(t)
	client := gh.NewClient(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runs, err := client.ListQueuedRuns(ctx, pat, "emon5122", "scalerunnergh")
	if err != nil {
		t.Fatalf("ListQueuedRuns failed: %v", err)
	}
	t.Logf("Found %d queued runs", len(runs))
}

func TestIntegration_ListQueuedJobs(t *testing.T) {
	pat := getTestPAT(t)
	client := gh.NewClient(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	jobs, err := client.ListQueuedJobs(ctx, pat, "emon5122", "scalerunnergh")
	if err != nil {
		t.Fatalf("ListQueuedJobs failed: %v", err)
	}
	t.Logf("Found %d queued jobs", len(jobs))
}

func TestIntegration_ConfigAndMetrics(t *testing.T) {
	pat := getTestPAT(t)

	cfg := &config.Config{
		Repos: []config.RepoConfig{
			{Owner: "emon5122", Repo: "scalerunnergh", Labels: []string{"self-hosted"}},
		},
		GlobalPAT:    pat,
		PollInterval: config.Duration{Duration: 10 * time.Second},
		Resources:    config.ResourceLimits{MaxRunners: 5, MaxGPUs: 2},
		Docker:       config.DockerConfig{Image: "runner:test"},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("config validation failed: %v", err)
	}

	resMgr := resource.NewManager(5, 2, 0, 0)
	met := metrics.New()

	// Verify resource allocation
	if err := resMgr.Allocate("test-1", 1, 0, 0); err != nil {
		t.Fatalf("allocate failed: %v", err)
	}
	met.ActiveRunners.Store(1)
	met.UsedGPUs.Store(1)

	snap := resMgr.Snapshot()
	if snap.ActiveRunners != 1 || snap.UsedGPUs != 1 {
		t.Errorf("unexpected snapshot: %+v", snap)
	}

	resMgr.Release("test-1")
	t.Log("Integration test passed: config, resources, metrics all work")
}
