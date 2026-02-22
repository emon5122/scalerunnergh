package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsHandler(t *testing.T) {
	c := New()
	c.ActiveRunners.Store(3)
	c.QueuedJobs.Store(5)
	c.TotalLaunched.Store(10)
	c.TotalCompleted.Store(7)
	c.TotalFailed.Store(2)
	c.UsedGPUs.Store(2)
	c.AvailableGPUs.Store(6)
	c.PollErrors.Store(1)
	c.SetRepoQueuedJobs("org1/repo1", 3)
	c.SetRepoQueuedJobs("org2/repo2", 2)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	checks := []string{
		"scalerunner_active_runners 3",
		"scalerunner_queued_jobs 5",
		"scalerunner_total_launched 10",
		"scalerunner_total_completed 7",
		"scalerunner_total_failed 2",
		"scalerunner_used_gpus 2",
		"scalerunner_available_gpus 6",
		"scalerunner_poll_errors_total 1",
		`scalerunner_repo_queued_jobs{repo="org1/repo1"} 3`,
		`scalerunner_repo_queued_jobs{repo="org2/repo2"} 2`,
	}
	for _, check := range checks {
		if !strings.Contains(text, check) {
			t.Errorf("expected metrics output to contain %q", check)
		}
	}
}

func TestSetGetRepoQueuedJobs(t *testing.T) {
	c := New()
	c.SetRepoQueuedJobs("org/repo", 10)
	if got := c.GetRepoQueuedJobs("org/repo"); got != 10 {
		t.Errorf("expected 10, got %d", got)
	}
	if got := c.GetRepoQueuedJobs("other"); got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

func TestAtomicCounters(t *testing.T) {
	c := New()
	c.TotalLaunched.Add(5)
	c.TotalLaunched.Add(3)
	if got := c.TotalLaunched.Load(); got != 8 {
		t.Errorf("expected 8, got %d", got)
	}
}
