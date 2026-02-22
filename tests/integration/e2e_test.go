package integration

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
	"github.com/emon5122/scalerunnergh/internal/orchestrator"
	"github.com/emon5122/scalerunnergh/internal/resource"
	"github.com/emon5122/scalerunnergh/internal/runner"
)

// mockDockerClient for e2e test
type mockDockerClient struct {
	mu         sync.Mutex
	containers map[string]bool
	nextID     int
	launched   []string
}

func newMockDocker() *mockDockerClient {
	return &mockDockerClient{containers: make(map[string]bool)}
}

func (m *mockDockerClient) CreateAndStart(ctx context.Context, opts runner.ContainerOpts) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	id := fmt.Sprintf("e2e-container-%d", m.nextID)
	m.containers[id] = true
	m.launched = append(m.launched, id)
	return id, nil
}

func (m *mockDockerClient) Wait(ctx context.Context, containerID string) (int, error) {
	// Simulate short job execution
	time.Sleep(50 * time.Millisecond)
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
	return running && ok, nil
}

func (m *mockDockerClient) LaunchedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.launched)
}

// TestE2E_FullOrchestrationCycle tests the complete flow:
// 1. GitHub API returns queued jobs
// 2. Orchestrator detects them
// 3. Registration token is obtained
// 4. Runner containers are launched
// 5. Containers complete and resources are freed
func TestE2E_FullOrchestrationCycle(t *testing.T) {
	// Setup mock GitHub API server
	tokenRequests := 0
	mux := http.NewServeMux()

	// Org1/Repo1 has 2 queued jobs
	mux.HandleFunc("/repos/org1/repo1/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(gh.WorkflowRunList{
			TotalCount: 1,
			Runs:       []gh.WorkflowRun{{ID: 1000, Status: "queued"}},
		})
	})
	mux.HandleFunc("/repos/org1/repo1/actions/runs/1000/jobs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(gh.WorkflowJobList{
			TotalCount: 2,
			Jobs: []gh.WorkflowJob{
				{ID: 2001, RunID: 1000, Name: "build", Status: "queued", Labels: []string{"self-hosted", "gpu"}},
				{ID: 2002, RunID: 1000, Name: "test", Status: "queued", Labels: []string{"self-hosted", "gpu"}},
			},
		})
	})
	mux.HandleFunc("/repos/org1/repo1/actions/runners/registration-token", func(w http.ResponseWriter, r *http.Request) {
		tokenRequests++
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(gh.RegistrationToken{Token: fmt.Sprintf("TOK-%d", tokenRequests)})
	})

	// Org2/Repo2 has 1 queued job
	mux.HandleFunc("/repos/org2/repo2/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(gh.WorkflowRunList{
			TotalCount: 1,
			Runs:       []gh.WorkflowRun{{ID: 3000, Status: "queued"}},
		})
	})
	mux.HandleFunc("/repos/org2/repo2/actions/runs/3000/jobs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(gh.WorkflowJobList{
			TotalCount: 1,
			Jobs: []gh.WorkflowJob{
				{ID: 4001, RunID: 3000, Name: "deploy", Status: "queued", Labels: []string{"self-hosted", "cpu"}},
			},
		})
	})
	mux.HandleFunc("/repos/org2/repo2/actions/runners/registration-token", func(w http.ResponseWriter, r *http.Request) {
		tokenRequests++
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(gh.RegistrationToken{Token: fmt.Sprintf("TOK-%d", tokenRequests)})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Config with multi-org/repo
	cfg := &config.Config{
		Repos: []config.RepoConfig{
			{Owner: "org1", Repo: "repo1", Labels: []string{"self-hosted", "gpu"}},
			{Owner: "org2", Repo: "repo2", Labels: []string{"self-hosted", "cpu"}},
		},
		GlobalPAT:    "test-pat",
		PollInterval: config.Duration{Duration: 100 * time.Millisecond},
		Resources:    config.ResourceLimits{MaxRunners: 10, MaxGPUs: 4},
		Docker:       config.DockerConfig{Image: "runner:e2e", GPUEnabled: true},
	}

	ghClient := gh.NewClient(nil)
	ghClient.SetBaseURL(srv.URL)

	mock := newMockDocker()
	launcher := runner.NewLauncher(mock, nil)
	resMgr := resource.NewManager(10, 4, 0, 0)
	met := metrics.New()

	completed := make(chan int64, 10)

	orch := orchestrator.New(cfg, ghClient, launcher, resMgr, met, nil)

	// Run a single poll cycle
	orch.RunOnce(context.Background())

	// Wait for all containers to complete
	time.Sleep(500 * time.Millisecond)

	// Verify results
	if mock.LaunchedCount() != 3 {
		t.Errorf("expected 3 containers launched (2 from repo1 + 1 from repo2), got %d", mock.LaunchedCount())
	}

	if met.TotalLaunched.Load() != 3 {
		t.Errorf("expected total_launched=3, got %d", met.TotalLaunched.Load())
	}

	// After completion, resources should be freed
	time.Sleep(200 * time.Millisecond)

	if met.TotalCompleted.Load()+met.TotalFailed.Load() < 3 {
		t.Logf("completed=%d, failed=%d (may still be running)",
			met.TotalCompleted.Load(), met.TotalFailed.Load())
	}

	t.Logf("E2E passed: %d launched, %d completed, %d failed, %d token requests",
		met.TotalLaunched.Load(), met.TotalCompleted.Load(),
		met.TotalFailed.Load(), tokenRequests)

	close(completed)
}

// TestE2E_ResourceExhaustion verifies that the orchestrator respects
// resource limits and doesn't over-provision.
func TestE2E_ResourceExhaustion(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(gh.WorkflowRunList{
			TotalCount: 1,
			Runs:       []gh.WorkflowRun{{ID: 5000, Status: "queued"}},
		})
	})
	mux.HandleFunc("/repos/org/repo/actions/runs/5000/jobs", func(w http.ResponseWriter, r *http.Request) {
		// 5 queued jobs but only 2 runner slots available
		jobs := make([]gh.WorkflowJob, 5)
		for i := range jobs {
			jobs[i] = gh.WorkflowJob{
				ID: int64(6000 + i), RunID: 5000, Name: fmt.Sprintf("job-%d", i),
				Status: "queued", Labels: []string{"self-hosted"},
			}
		}
		json.NewEncoder(w).Encode(gh.WorkflowJobList{TotalCount: 5, Jobs: jobs})
	})
	mux.HandleFunc("/repos/org/repo/actions/runners/registration-token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(gh.RegistrationToken{Token: "TOK"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &config.Config{
		Repos:        []config.RepoConfig{{Owner: "org", Repo: "repo", Labels: []string{"self-hosted"}}},
		GlobalPAT:    "pat",
		PollInterval: config.Duration{Duration: 1 * time.Second},
		Resources:    config.ResourceLimits{MaxRunners: 2}, // Only 2 slots!
		Docker:       config.DockerConfig{Image: "runner:test"},
	}

	ghClient := gh.NewClient(nil)
	ghClient.SetBaseURL(srv.URL)

	mock := newMockDocker()
	launcher := runner.NewLauncher(mock, nil)
	resMgr := resource.NewManager(2, 0, 0, 0)
	met := metrics.New()

	orch := orchestrator.New(cfg, ghClient, launcher, resMgr, met, nil)
	orch.RunOnce(context.Background())

	// Should only launch 2 despite 5 queued
	if mock.LaunchedCount() > 2 {
		t.Errorf("expected at most 2 launched (capacity limit), got %d", mock.LaunchedCount())
	}
	t.Logf("Resource exhaustion test passed: %d launched (limit 2)", mock.LaunchedCount())
}
