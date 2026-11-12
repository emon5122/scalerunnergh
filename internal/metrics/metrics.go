// Package metrics exposes Prometheus-format metrics without external
// dependencies: a few counters and gauges, labelled by scale set.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Metric names.
const (
	RunnersStarted   = "scalerunner_runners_started_total"
	RunnersCompleted = "scalerunner_runners_completed_total"
	RunnersFailed    = "scalerunner_runners_failed_total"
	StartErrors      = "scalerunner_start_errors_total"
	JobsRefused      = "scalerunner_jobs_refused_total"
	ListenerErrors   = "scalerunner_listener_errors_total"
	RunnersActive    = "scalerunner_runners_active"
	RunnersBusy      = "scalerunner_runners_busy"
	JobsAssigned     = "scalerunner_jobs_assigned"
	CapacityBlocked  = "scalerunner_capacity_blocked"
	HostSlotsUsed    = "scalerunner_host_slots_used"
	HostSlotsMax     = "scalerunner_host_slots_max"
	HostPaused       = "scalerunner_host_paused"
)

var help = map[string]string{
	RunnersStarted:   "Runners started",
	RunnersCompleted: "Runners that exited with code 0",
	RunnersFailed:    "Runners that exited non-zero or were force-stopped",
	StartErrors:      "Runner start errors (JIT config or backend)",
	JobsRefused:      "Jobs refused by the guard (public repo, fork PR, not allowed)",
	ListenerErrors:   "Message session errors (the listener restarts)",
	RunnersActive:    "Runners currently alive",
	RunnersBusy:      "Runners currently running a job",
	JobsAssigned:     "Jobs GitHub has assigned to the scale set",
	CapacityBlocked:  "1 while jobs wait for a host slot, the load gate or the kill switch",
	HostSlotsUsed:    "Host slots in use",
	HostSlotsMax:     "Host slots (host.max_runners)",
	HostPaused:       "1 while the kill switch is on",
}

// Collector stores metric values.
type Collector struct {
	mu     sync.Mutex
	values map[string]map[string]float64 // name -> scale set ("" = host) -> value
}

// New creates a collector.
func New() *Collector {
	return &Collector{values: map[string]map[string]float64{}}
}

// Add increments a counter.
func (c *Collector) Add(name, scaleSet string, delta float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.series(name)[scaleSet] += delta
}

// Set sets a gauge.
func (c *Collector) Set(name, scaleSet string, v float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.series(name)[scaleSet] = v
}

// Get returns a value (0 when unset).
func (c *Collector) Get(name, scaleSet string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[name][scaleSet]
}

func (c *Collector) series(name string) map[string]float64 {
	s := c.values[name]
	if s == nil {
		s = map[string]float64{}
		c.values[name] = s
	}
	return s
}

// Handler serves the Prometheus text exposition format.
func (c *Collector) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		c.mu.Lock()
		defer c.mu.Unlock()
		names := make([]string, 0, len(c.values))
		for n := range c.values {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			typ := "gauge"
			if strings.HasSuffix(n, "_total") {
				typ = "counter"
			}
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", n, help[n], n, typ)
			sets := make([]string, 0, len(c.values[n]))
			for s := range c.values[n] {
				sets = append(sets, s)
			}
			sort.Strings(sets)
			for _, s := range sets {
				if s == "" {
					fmt.Fprintf(w, "%s %g\n", n, c.values[n][s])
				} else {
					fmt.Fprintf(w, "%s{scale_set=%q} %g\n", n, s, c.values[n][s])
				}
			}
		}
	})
}
