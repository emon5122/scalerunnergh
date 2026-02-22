package runner

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// mockDockerClient is a fake DockerClient for unit tests.
type mockDockerClient struct {
	mu         sync.Mutex
	containers map[string]bool // id -> running
	nextID     int
	failCreate bool
	failWait   bool
}

func newMockDocker() *mockDockerClient {
	return &mockDockerClient{containers: make(map[string]bool)}
}

func (m *mockDockerClient) CreateAndStart(ctx context.Context, opts ContainerOpts) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failCreate {
		return "", fmt.Errorf("mock: create failed")
	}
	m.nextID++
	id := fmt.Sprintf("mock-%d", m.nextID)
	m.containers[id] = true
	return id, nil
}

func (m *mockDockerClient) Wait(ctx context.Context, containerID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWait {
		return -1, fmt.Errorf("mock: wait failed")
	}
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
		return false, fmt.Errorf("container not found")
	}
	return running, nil
}

func TestLaunch(t *testing.T) {
	mock := newMockDocker()
	launcher := NewLauncher(mock, nil)

	info, err := launcher.Launch(context.Background(), LaunchRequest{
		Owner:  "org1",
		Repo:   "repo1",
		JobID:  42,
		Token:  "test-token",
		Labels: []string{"gpu", "docker"},
		Image:  "runner:latest",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.ID != "mock-1" {
		t.Errorf("expected mock-1, got %s", info.ID)
	}
	if info.Status != "running" {
		t.Errorf("expected running, got %s", info.Status)
	}
	if info.Owner != "org1" {
		t.Errorf("expected org1, got %s", info.Owner)
	}
	if info.JobID != 42 {
		t.Errorf("expected job 42, got %d", info.JobID)
	}
	if launcher.ActiveCount() != 1 {
		t.Errorf("expected 1 active, got %d", launcher.ActiveCount())
	}
}

func TestLaunch_OrgLevel(t *testing.T) {
	mock := newMockDocker()
	launcher := NewLauncher(mock, nil)

	info, err := launcher.Launch(context.Background(), LaunchRequest{
		Owner:  "myorg",
		Repo:   "", // org-level
		JobID:  99,
		Token:  "org-token",
		Labels: []string{"self-hosted"},
		Image:  "runner:latest",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Repo != "" {
		t.Errorf("expected empty repo for org-level, got %s", info.Repo)
	}
}

func TestLaunch_CreateFails(t *testing.T) {
	mock := newMockDocker()
	mock.failCreate = true
	launcher := NewLauncher(mock, nil)

	_, err := launcher.Launch(context.Background(), LaunchRequest{
		Owner: "org1", Repo: "repo1", JobID: 1,
		Token: "t", Image: "i",
	})
	if err == nil {
		t.Fatal("expected error when create fails")
	}
}

func TestWaitAndCleanup(t *testing.T) {
	mock := newMockDocker()
	launcher := NewLauncher(mock, nil)

	info, _ := launcher.Launch(context.Background(), LaunchRequest{
		Owner: "org1", Repo: "repo1", JobID: 1,
		Token: "t", Labels: []string{"gpu"}, Image: "runner:latest",
	})

	exitCode, err := launcher.WaitAndCleanup(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("expected exit 0, got %d", exitCode)
	}

	updated, ok := launcher.GetContainer(info.ID)
	if !ok {
		t.Fatal("container not found after wait")
	}
	if updated.Status != "exited" {
		t.Errorf("expected exited, got %s", updated.Status)
	}
}

func TestWaitAndCleanup_WaitError(t *testing.T) {
	mock := newMockDocker()
	mock.failWait = true
	launcher := NewLauncher(mock, nil)

	info, _ := launcher.Launch(context.Background(), LaunchRequest{
		Owner: "org1", Repo: "repo1", JobID: 1,
		Token: "t", Image: "i",
	})

	_, err := launcher.WaitAndCleanup(context.Background(), info.ID)
	if err == nil {
		t.Fatal("expected error when wait fails")
	}

	updated, _ := launcher.GetContainer(info.ID)
	if updated.Status != "error" {
		t.Errorf("expected error status, got %s", updated.Status)
	}
}

func TestListContainers(t *testing.T) {
	mock := newMockDocker()
	launcher := NewLauncher(mock, nil)

	launcher.Launch(context.Background(), LaunchRequest{
		Owner: "o", Repo: "r1", JobID: 1, Token: "t", Image: "i",
	})
	launcher.Launch(context.Background(), LaunchRequest{
		Owner: "o", Repo: "r2", JobID: 2, Token: "t", Image: "i",
	})

	containers := launcher.ListContainers()
	if len(containers) != 2 {
		t.Errorf("expected 2 containers, got %d", len(containers))
	}
}

func TestRemoveTracking(t *testing.T) {
	mock := newMockDocker()
	launcher := NewLauncher(mock, nil)

	info, _ := launcher.Launch(context.Background(), LaunchRequest{
		Owner: "o", Repo: "r", JobID: 1, Token: "t", Image: "i",
	})

	launcher.RemoveTracking(info.ID)
	_, ok := launcher.GetContainer(info.ID)
	if ok {
		t.Error("expected container to be removed from tracking")
	}
}

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello-world_123", "hello-world_123"},
		{"hello world!", "helloworld"},
		{"abc/def:latest", "abcdeflatest"},
		{strings.Repeat("a", 100), strings.Repeat("a", 63)},
	}
	for _, tt := range tests {
		got := sanitizeName(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
