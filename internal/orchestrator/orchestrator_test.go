package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/emon5122/scalerunnergh/internal/config"
	gh "github.com/emon5122/scalerunnergh/internal/github"
	"github.com/emon5122/scalerunnergh/internal/metrics"
	"github.com/emon5122/scalerunnergh/internal/resource"
	"github.com/emon5122/scalerunnergh/internal/runner"
)

// mockDockerClient for orchestrator tests.
type mockDockerClient struct {
	mu         sync.Mutex
	containers map[string]bool
	nextID     int
}

func newMockDocker() *mockDockerClient {
	return &mockDockerClient{containers: make(map[string]bool)}
}

func (m *mockDockerClient) CreateAndStart(ctx context.Context, opts runner.ContainerOpts) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	id := fmt.Sprintf("mock-%d", m.nextID)
	m.containers[id] = true
	return id, nil
}

func (m *mockDockerClient) Wait(ctx context.Context, containerID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.containers[containerID] = false
	return 0, nil
}

func (m *mockDockerClient) Remove(ctx context.Context, containerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.containers, containerID)
	return nil
}

func (m *mockDockerClient) IsRunning(ctx context.Context, containerID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	running, ok := m.containers[containerID]
	if !ok {
		return false, nil
	}
	return running, nil
}

func setupTestServer(t *testing.T) (*httptest.Server, *config.Config) {
	t.Helper()
	mux := http.NewServeMux()

	// Queued runs
	mux.HandleFunc("/repos/testorg/testrepo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(gh.WorkflowRunList{
			TotalCount: 1,
			Runs:       []gh.WorkflowRun{{ID: 100, Status: "queued"}},
		})
	})

	// Jobs for run 100
	mux.HandleFunc("/repos/testorg/testrepo/actions/runs/100/jobs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(gh.WorkflowJobList{
			TotalCount: 1,
			Jobs: []gh.WorkflowJob{
				{ID: 501, RunID: 100, Name: "build", Status: "queued", Labels: []string{"self-hosted", "gpu"}},
			},
		})
	})

	// Registration token
	mux.HandleFunc("/repos/testorg/testrepo/actions/runners/registration-token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(gh.RegistrationToken{Token: "TESTTOKEN123"})
	})

	srv := httptest.NewServer(mux)

	cfg := &config.Config{
		Repos: []config.RepoConfig{
			{Owner: "testorg", Repo: "testrepo", Labels: []string{"self-hosted", "gpu"}},
		},
		GlobalPAT:    "test-pat",
		PollInterval: config.Duration{Duration: 1 * time.Second},
		Resources:    config.ResourceLimits{MaxRunners: 5, MaxGPUs: 4},
		Docker:       config.DockerConfig{Image: "runner:test", GPUEnabled: true},
		Metrics:      config.MetricsConfig{Enabled: false},
		LogLevel:     "debug",
	}

	return srv, cfg
}

func TestRunOnce_DispatchesJob(t *testing.T) {
	srv, cfg := setupTestServer(t)
	defer srv.Close()

	ghClient := gh.NewClient(nil)
	ghClient.SetBaseURL(srv.URL)

	mock := newMockDocker()
	launcher := runner.NewLauncher(mock, nil)
	resMgr := resource.NewManager(5, 4, 0, 0)
	met := metrics.New()

	dispatched := make(chan int64, 1)

	orch := New(cfg, ghClient, launcher, resMgr, met, nil)
	orch.onJobDispatched = func(jobID int64, containerID string) {
		dispatched <- jobID
	}

	ctx := context.Background()
	orch.RunOnce(ctx)

	select {
	case jobID := <-dispatched:
		if jobID != 501 {
			t.Errorf("expected job 501, got %d", jobID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for job dispatch")
	}

	if orch.DispatchedCount() != 1 {
		t.Errorf("expected 1 dispatched, got %d", orch.DispatchedCount())
	}

	if met.TotalLaunched.Load() != 1 {
		t.Errorf("expected total_launched=1, got %d", met.TotalLaunched.Load())
	}
}

func TestRunOnce_SkipsAlreadyDispatched(t *testing.T) {
	srv, cfg := setupTestServer(t)
	defer srv.Close()

	ghClient := gh.NewClient(nil)
	ghClient.SetBaseURL(srv.URL)

	mock := newMockDocker()
	launcher := runner.NewLauncher(mock, nil)
	resMgr := resource.NewManager(5, 4, 0, 0)
	met := metrics.New()

	orch := New(cfg, ghClient, launcher, resMgr, met, nil)
	orch.markDispatched(501, "existing-container")

	ctx := context.Background()
	orch.RunOnce(ctx)

	// Should not launch another runner
	if met.TotalLaunched.Load() != 0 {
		t.Errorf("should not launch when already dispatched, got %d", met.TotalLaunched.Load())
	}
}

func TestRunOnce_NoCapacity(t *testing.T) {
	srv, cfg := setupTestServer(t)
	defer srv.Close()

	ghClient := gh.NewClient(nil)
	ghClient.SetBaseURL(srv.URL)

	mock := newMockDocker()
	launcher := runner.NewLauncher(mock, nil)
	// Only 1 runner allowed and it's full
	resMgr := resource.NewManager(1, 4, 0, 0)
	resMgr.Allocate("existing", 0, 0, 0) // fill all slots
	met := metrics.New()

	orch := New(cfg, ghClient, launcher, resMgr, met, nil)

	ctx := context.Background()
	orch.RunOnce(ctx)

	if met.TotalLaunched.Load() != 0 {
		t.Errorf("should not launch when no capacity, got %d", met.TotalLaunched.Load())
	}
}

func TestRunOnce_LabelMismatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/testorg/testrepo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(gh.WorkflowRunList{
			TotalCount: 1,
			Runs:       []gh.WorkflowRun{{ID: 200, Status: "queued"}},
		})
	})
	mux.HandleFunc("/repos/testorg/testrepo/actions/runs/200/jobs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(gh.WorkflowJobList{
			TotalCount: 1,
			Jobs: []gh.WorkflowJob{
				{ID: 601, RunID: 200, Name: "build", Status: "queued", Labels: []string{"special-label"}},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &config.Config{
		Repos:        []config.RepoConfig{{Owner: "testorg", Repo: "testrepo", Labels: []string{"gpu"}}},
		GlobalPAT:    "pat",
		PollInterval: config.Duration{Duration: 1 * time.Second},
		Resources:    config.ResourceLimits{MaxRunners: 5},
		Docker:       config.DockerConfig{Image: "runner:test"},
	}

	ghClient := gh.NewClient(nil)
	ghClient.SetBaseURL(srv.URL)

	mock := newMockDocker()
	launcher := runner.NewLauncher(mock, nil)
	resMgr := resource.NewManager(5, 0, 0, 0)
	met := metrics.New()

	orch := New(cfg, ghClient, launcher, resMgr, met, nil)
	orch.RunOnce(context.Background())

	if met.TotalLaunched.Load() != 0 {
		t.Errorf("should not launch for label mismatch, got %d", met.TotalLaunched.Load())
	}
}

func TestRunOnce_MultiRepo(t *testing.T) {
	mux := http.NewServeMux()

	// Repo 1
	mux.HandleFunc("/repos/org1/repo1/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(gh.WorkflowRunList{
			TotalCount: 1,
			Runs:       []gh.WorkflowRun{{ID: 300, Status: "queued"}},
		})
	})
	mux.HandleFunc("/repos/org1/repo1/actions/runs/300/jobs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(gh.WorkflowJobList{
			TotalCount: 1,
			Jobs: []gh.WorkflowJob{
				{ID: 701, RunID: 300, Name: "build", Status: "queued", Labels: []string{"gpu"}},
			},
		})
	})
	mux.HandleFunc("/repos/org1/repo1/actions/runners/registration-token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(gh.RegistrationToken{Token: "TOK1"})
	})

	// Repo 2
	mux.HandleFunc("/repos/org2/repo2/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(gh.WorkflowRunList{
			TotalCount: 1,
			Runs:       []gh.WorkflowRun{{ID: 400, Status: "queued"}},
		})
	})
	mux.HandleFunc("/repos/org2/repo2/actions/runs/400/jobs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(gh.WorkflowJobList{
			TotalCount: 1,
			Jobs: []gh.WorkflowJob{
				{ID: 801, RunID: 400, Name: "test", Status: "queued", Labels: []string{"cpu"}},
			},
		})
	})
	mux.HandleFunc("/repos/org2/repo2/actions/runners/registration-token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(gh.RegistrationToken{Token: "TOK2"})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &config.Config{
		Repos: []config.RepoConfig{
			{Owner: "org1", Repo: "repo1", Labels: []string{"gpu"}},
			{Owner: "org2", Repo: "repo2", Labels: []string{"cpu"}},
		},
		GlobalPAT:    "pat",
		PollInterval: config.Duration{Duration: 1 * time.Second},
		Resources:    config.ResourceLimits{MaxRunners: 10},
		Docker:       config.DockerConfig{Image: "runner:test"},
	}

	ghClient := gh.NewClient(nil)
	ghClient.SetBaseURL(srv.URL)

	mock := newMockDocker()
	launcher := runner.NewLauncher(mock, nil)
	resMgr := resource.NewManager(10, 0, 0, 0)
	met := metrics.New()

	dispatched := make(chan int64, 2)
	orch := New(cfg, ghClient, launcher, resMgr, met, nil)
	orch.onJobDispatched = func(jobID int64, containerID string) {
		dispatched <- jobID
	}

	orch.RunOnce(context.Background())

	// Wait for both dispatches
	seen := make(map[int64]bool)
	for i := 0; i < 2; i++ {
		select {
		case id := <-dispatched:
			seen[id] = true
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for dispatch")
		}
	}

	if !seen[701] || !seen[801] {
		t.Errorf("expected jobs 701 and 801, got %v", seen)
	}
}
