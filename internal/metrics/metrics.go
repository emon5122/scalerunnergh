// Package metrics provides Prometheus metrics for the scalerunnergh orchestrator.
package metrics

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
)

// Collector holds all Prometheus-style metrics counters.
// For simplicity and zero external dependencies in the core package,
// we use simple atomic counters and expose them via a /metrics endpoint.
type Collector struct {
	ActiveRunners  atomic.Int64
	QueuedJobs     atomic.Int64
	TotalLaunched  atomic.Int64
	TotalCompleted atomic.Int64
	TotalFailed    atomic.Int64
	UsedGPUs       atomic.Int64
	AvailableGPUs  atomic.Int64
	PollErrors     atomic.Int64
	LaunchErrors   atomic.Int64
	TokenErrors    atomic.Int64

	mu       sync.Mutex
	repoJobs map[string]int64 // "owner/repo" -> queued count
}

// New creates a new metrics collector.
func New() *Collector {
	return &Collector{
		repoJobs: make(map[string]int64),
	}
}

// SetRepoQueuedJobs updates the queued job count for a specific repo.
func (c *Collector) SetRepoQueuedJobs(fullName string, count int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.repoJobs[fullName] = count
}

// GetRepoQueuedJobs returns the queued job count for a specific repo.
func (c *Collector) GetRepoQueuedJobs(fullName string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.repoJobs[fullName]
}

// Handler returns an HTTP handler that serves metrics in Prometheus exposition format.
func (c *Collector) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		writeGauge(w, "scalerunner_active_runners", "Number of currently running runner containers", c.ActiveRunners.Load())
		writeGauge(w, "scalerunner_queued_jobs", "Number of queued jobs across all repos", c.QueuedJobs.Load())
		writeCounter(w, "scalerunner_total_launched", "Total number of runners launched", c.TotalLaunched.Load())
		writeCounter(w, "scalerunner_total_completed", "Total number of runners completed successfully", c.TotalCompleted.Load())
		writeCounter(w, "scalerunner_total_failed", "Total number of runners that failed", c.TotalFailed.Load())
		writeGauge(w, "scalerunner_used_gpus", "Number of GPUs currently in use", c.UsedGPUs.Load())
		writeGauge(w, "scalerunner_available_gpus", "Number of GPUs available", c.AvailableGPUs.Load())
		writeCounter(w, "scalerunner_poll_errors_total", "Total GitHub API poll errors", c.PollErrors.Load())
		writeCounter(w, "scalerunner_launch_errors_total", "Total runner launch errors", c.LaunchErrors.Load())
		writeCounter(w, "scalerunner_token_errors_total", "Total registration token errors", c.TokenErrors.Load())

		c.mu.Lock()
		for repo, count := range c.repoJobs {
			writeGaugeLabeled(w, "scalerunner_repo_queued_jobs", "Queued jobs per repo", "repo", repo, count)
		}
		c.mu.Unlock()
	})
}

func writeGauge(w http.ResponseWriter, name, help string, value int64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, value)
}

func writeCounter(w http.ResponseWriter, name, help string, value int64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, value)
}

func writeGaugeLabeled(w http.ResponseWriter, name, help, labelKey, labelVal string, value int64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s{%s=%q} %d\n",
		name, help, name, name, labelKey, labelVal, value)
}
