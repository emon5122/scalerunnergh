// Package github provides a client wrapper around the GitHub Actions API
// for listing queued workflow runs/jobs and managing runner registration tokens.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// WorkflowJob represents a single job within a workflow run as returned by
// the GitHub Actions API.
type WorkflowJob struct {
	ID          int64    `json:"id"`
	RunID       int64    `json:"run_id"`
	Name        string   `json:"name"`
	Status      string   `json:"status"`     // queued, in_progress, completed
	Conclusion  string   `json:"conclusion"` // success, failure, cancelled, ...
	Labels      []string `json:"labels"`
	RunnerName  string   `json:"runner_name"`
	StartedAt   string   `json:"started_at"`
	CompletedAt string   `json:"completed_at"`
}

// WorkflowJobList is the envelope returned by the list-jobs API.
type WorkflowJobList struct {
	TotalCount int           `json:"total_count"`
	Jobs       []WorkflowJob `json:"workflow_jobs"`
}

// WorkflowRun represents a workflow run.
type WorkflowRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"` // queued, in_progress, completed
	Conclusion string `json:"conclusion"`
}

// WorkflowRunList is the envelope returned by the list-runs API.
type WorkflowRunList struct {
	TotalCount int           `json:"total_count"`
	Runs       []WorkflowRun `json:"workflow_runs"`
}

// RegistrationToken is the response from the create-registration-token endpoint.
type RegistrationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Client wraps HTTP calls to the GitHub API.
type Client struct {
	httpClient *http.Client
	baseURL    string
	mu         sync.Mutex
}

// NewClient creates a new GitHub API client. If httpClient is nil,
// http.DefaultClient is used.
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		httpClient: httpClient,
		baseURL:    "https://api.github.com",
	}
}

// SetBaseURL overrides the API base URL (useful for testing).
func (c *Client) SetBaseURL(url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.baseURL = url
}

func (c *Client) getBaseURL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.baseURL
}

// ListQueuedRuns returns workflow runs in "queued" status for a repo.
func (c *Client) ListQueuedRuns(ctx context.Context, pat, owner, repo string) ([]WorkflowRun, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/runs?status=queued&per_page=100",
		c.getBaseURL(), owner, repo)
	var result WorkflowRunList
	if err := c.doGet(ctx, pat, url, &result); err != nil {
		return nil, fmt.Errorf("list queued runs: %w", err)
	}
	return result.Runs, nil
}

// ListQueuedJobs returns workflow jobs in "queued" status for a repo.
// This queries all queued runs and fetches their jobs, filtering for queued ones.
func (c *Client) ListQueuedJobs(ctx context.Context, pat, owner, repo string) ([]WorkflowJob, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/runs?status=queued&per_page=100",
		c.getBaseURL(), owner, repo)
	var runList WorkflowRunList
	if err := c.doGet(ctx, pat, url, &runList); err != nil {
		return nil, fmt.Errorf("list queued jobs (runs): %w", err)
	}

	var allJobs []WorkflowJob
	for _, run := range runList.Runs {
		jobURL := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/jobs?per_page=100",
			c.getBaseURL(), owner, repo, run.ID)
		var jobList WorkflowJobList
		if err := c.doGet(ctx, pat, jobURL, &jobList); err != nil {
			return nil, fmt.Errorf("list jobs for run %d: %w", run.ID, err)
		}
		for _, j := range jobList.Jobs {
			if j.Status == "queued" {
				allJobs = append(allJobs, j)
			}
		}
	}
	return allJobs, nil
}

// ListOrgQueuedJobs lists all queued jobs across an organization's repos.
// This is a convenience that lists org-level runners' queued jobs.
func (c *Client) ListOrgQueuedJobs(ctx context.Context, pat, org string) ([]WorkflowJob, error) {
	// The org-level API doesn't have a direct "queued jobs" endpoint.
	// We use the org actions runners endpoint to approximate this.
	// For now, return empty (org support is future).
	return nil, nil
}

// CreateRepoRegistrationToken creates a short-lived runner registration token
// for a specific repo.
func (c *Client) CreateRepoRegistrationToken(ctx context.Context, pat, owner, repo string) (*RegistrationToken, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/runners/registration-token",
		c.getBaseURL(), owner, repo)
	var tok RegistrationToken
	if err := c.doPost(ctx, pat, url, &tok); err != nil {
		return nil, fmt.Errorf("create repo registration token: %w", err)
	}
	return &tok, nil
}

// CreateOrgRegistrationToken creates a short-lived runner registration token
// for an organization.
func (c *Client) CreateOrgRegistrationToken(ctx context.Context, pat, org string) (*RegistrationToken, error) {
	url := fmt.Sprintf("%s/orgs/%s/actions/runners/registration-token",
		c.getBaseURL(), org)
	var tok RegistrationToken
	if err := c.doPost(ctx, pat, url, &tok); err != nil {
		return nil, fmt.Errorf("create org registration token: %w", err)
	}
	return &tok, nil
}

// doGet performs a GET request with auth and decodes JSON.
func (c *Client) doGet(ctx context.Context, pat, url string, dest interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github API %s returned %d: %s", url, resp.StatusCode, string(body))
	}

	return json.NewDecoder(resp.Body).Decode(dest)
}

// doPost performs a POST request with auth and decodes JSON response.
func (c *Client) doPost(ctx context.Context, pat, url string, dest interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github API POST %s returned %d: %s", url, resp.StatusCode, string(body))
	}

	return json.NewDecoder(resp.Body).Decode(dest)
}

// MatchesLabels checks if a job's labels are a subset of the given runner labels.
func MatchesLabels(jobLabels, runnerLabels []string) bool {
	set := make(map[string]struct{}, len(runnerLabels))
	for _, l := range runnerLabels {
		set[l] = struct{}{}
	}
	for _, jl := range jobLabels {
		if _, ok := set[jl]; !ok {
			return false
		}
	}
	return true
}
