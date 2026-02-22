package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListQueuedRuns(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org1/repo1/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") != "queued" {
			t.Errorf("expected status=queued query param")
		}
		if r.Header.Get("Authorization") != "Bearer test-pat" {
			t.Errorf("expected auth header")
		}
		json.NewEncoder(w).Encode(WorkflowRunList{
			TotalCount: 1,
			Runs: []WorkflowRun{
				{ID: 100, Name: "CI", Status: "queued"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(nil)
	c.SetBaseURL(srv.URL)

	runs, err := c.ListQueuedRuns(context.Background(), "test-pat", "org1", "repo1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	if runs[0].ID != 100 {
		t.Errorf("expected run id 100, got %d", runs[0].ID)
	}
}

func TestListQueuedJobs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org1/repo1/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(WorkflowRunList{
			TotalCount: 1,
			Runs:       []WorkflowRun{{ID: 200, Status: "queued"}},
		})
	})
	mux.HandleFunc("/repos/org1/repo1/actions/runs/200/jobs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(WorkflowJobList{
			TotalCount: 2,
			Jobs: []WorkflowJob{
				{ID: 301, RunID: 200, Name: "build", Status: "queued", Labels: []string{"gpu"}},
				{ID: 302, RunID: 200, Name: "test", Status: "in_progress", Labels: []string{"cpu"}},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(nil)
	c.SetBaseURL(srv.URL)

	jobs, err := c.ListQueuedJobs(context.Background(), "test-pat", "org1", "repo1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 queued job, got %d", len(jobs))
	}
	if jobs[0].ID != 301 {
		t.Errorf("expected job id 301, got %d", jobs[0].ID)
	}
}

func TestCreateRepoRegistrationToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org1/repo1/actions/runners/registration-token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(RegistrationToken{
			Token: "AABBC123",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(nil)
	c.SetBaseURL(srv.URL)

	tok, err := c.CreateRepoRegistrationToken(context.Background(), "pat", "org1", "repo1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.Token != "AABBC123" {
		t.Errorf("expected AABBC123, got %s", tok.Token)
	}
}

func TestCreateOrgRegistrationToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/myorg/actions/runners/registration-token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(RegistrationToken{Token: "ORGTOKEN"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(nil)
	c.SetBaseURL(srv.URL)

	tok, err := c.CreateOrgRegistrationToken(context.Background(), "pat", "myorg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.Token != "ORGTOKEN" {
		t.Errorf("expected ORGTOKEN, got %s", tok.Token)
	}
}

func TestMatchesLabels(t *testing.T) {
	tests := []struct {
		jobLabels    []string
		runnerLabels []string
		want         bool
	}{
		{[]string{"gpu"}, []string{"gpu", "docker"}, true},
		{[]string{"gpu", "docker"}, []string{"gpu", "docker"}, true},
		{[]string{"gpu", "docker"}, []string{"gpu"}, false},
		{[]string{}, []string{"gpu"}, true},
		{[]string{"cpu"}, []string{}, false},
	}
	for i, tt := range tests {
		got := MatchesLabels(tt.jobLabels, tt.runnerLabels)
		if got != tt.want {
			t.Errorf("test %d: MatchesLabels(%v, %v) = %v, want %v",
				i, tt.jobLabels, tt.runnerLabels, got, tt.want)
		}
	}
}

func TestListQueuedRuns_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org1/repo1/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Forbidden"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(nil)
	c.SetBaseURL(srv.URL)

	_, err := c.ListQueuedRuns(context.Background(), "bad-pat", "org1", "repo1")
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}
