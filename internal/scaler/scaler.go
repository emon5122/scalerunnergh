// Package scaler turns GitHub's runner scale set messages into runners.
//
// One Scaler serves one scale set. GitHub matches jobs to the scale set and
// reports how many are assigned; the scaler keeps that many ephemeral
// runners alive, each holding a slot of the host pool, and never more than
// the host can take. Jobs the guard refuses are never acquired.
package scaler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/actions/scaleset"

	"github.com/emon5122/scalerunnergh/internal/backend"
	"github.com/emon5122/scalerunnergh/internal/config"
	"github.com/emon5122/scalerunnergh/internal/metrics"
	"github.com/emon5122/scalerunnergh/internal/pool"
)

// JITClient creates and removes JIT runners of a scale set.
type JITClient interface {
	GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	RemoveRunner(ctx context.Context, runnerID int64) error
}

// JobAcquirer acquires available jobs for the message session.
type JobAcquirer interface {
	AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error)
}

// JobGuard decides whether a job may run on this host.
type JobGuard interface {
	AllowJob(ctx context.Context, owner, repo, event string, runID int64) (bool, string)
}

// Options configure a Scaler.
type Options struct {
	Host     string
	ScaleSet config.ScaleSetConfig
	// ScaleSetID is the id GitHub gave the scale set (see SetScaleSetID).
	ScaleSetID int
	Backend    backend.Backend
	Pool       *pool.Pool
	Guard      JobGuard
	JIT        JITClient
	Metrics    *metrics.Collector
	Logger     *slog.Logger
	// IdleTimeout and MaxJobDuration come from the host config.
	IdleTimeout    time.Duration
	MaxJobDuration time.Duration
	// OnCapacity is told the capacity to report to GitHub whenever it changes.
	OnCapacity func(maxRunners int)
}

type runnerState struct {
	name     string
	runnerID int64
	handle   backend.Handle
	slot     *pool.Slot
	busy     bool
	started  time.Time
	stopping bool
}

// Scaler implements listener.Scaler for one scale set.
type Scaler struct {
	o Options

	mu           sync.Mutex
	acquirer     JobAcquirer
	runners      map[string]*runnerState
	starting     int
	assigned     int
	surplusSince time.Time
	closed       bool
	wg           sync.WaitGroup
}

// New creates a scaler.
func New(o Options) *Scaler {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Metrics == nil {
		o.Metrics = metrics.New()
	}
	o.Logger = o.Logger.With("scale_set", o.ScaleSet.Name)
	return &Scaler{o: o, runners: map[string]*runnerState{}}
}

// SetAcquirer sets the message session used to acquire jobs (it changes when
// the listener reconnects).
func (s *Scaler) SetAcquirer(a JobAcquirer) {
	s.mu.Lock()
	s.acquirer = a
	s.mu.Unlock()
}

// SetScaleSetID sets the scale set id once it is registered at GitHub.
func (s *Scaler) SetScaleSetID(id int) {
	s.mu.Lock()
	s.o.ScaleSetID = id
	s.mu.Unlock()
}

// Scale handles one message from GitHub (listener.Scaler).
func (s *Scaler) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if msg == nil {
		s.Reconcile(ctx)
		return nil
	}
	if len(msg.JobAvailableMessages) > 0 {
		ids := make([]int64, 0, len(msg.JobAvailableMessages))
		for _, j := range msg.JobAvailableMessages {
			if ok, reason := s.o.Guard.AllowJob(ctx, j.OwnerName, j.RepositoryName, j.EventName, j.WorkflowRunID); !ok {
				s.o.Metrics.Add(metrics.JobsRefused, s.o.ScaleSet.Name, 1)
				s.o.Logger.Warn("job refused, not acquired", "reason", reason,
					"repo", j.OwnerName+"/"+j.RepositoryName, "event", j.EventName, "run_id", j.WorkflowRunID)
				continue
			}
			ids = append(ids, j.RunnerRequestID)
		}
		s.mu.Lock()
		acq := s.acquirer
		s.mu.Unlock()
		if len(ids) > 0 && acq != nil {
			if _, err := acq.AcquireJobs(ctx, ids); err != nil {
				return fmt.Errorf("acquire jobs: %w", err)
			}
		}
	}
	for _, j := range msg.JobStartedMessages {
		s.jobStarted(ctx, j)
	}
	for _, j := range msg.JobCompletedMessages {
		s.o.Logger.Info("job completed", "runner", j.RunnerName, "result", j.Result,
			"repo", j.OwnerName+"/"+j.RepositoryName, "job", j.JobDisplayName)
	}
	if msg.Statistics != nil {
		s.mu.Lock()
		s.assigned = msg.Statistics.TotalAssignedJobs
		s.mu.Unlock()
		s.o.Metrics.Set(metrics.JobsAssigned, s.o.ScaleSet.Name, float64(msg.Statistics.TotalAssignedJobs))
	}
	s.Reconcile(ctx)
	return nil
}

// jobStarted marks the runner busy and re-checks the job: a job the guard
// would refuse must not keep running here.
func (s *Scaler) jobStarted(ctx context.Context, j *scaleset.JobStarted) {
	s.mu.Lock()
	r := s.runners[j.RunnerName]
	if r != nil {
		r.busy = true
	}
	s.mu.Unlock()
	s.o.Logger.Info("job started", "runner", j.RunnerName,
		"repo", j.OwnerName+"/"+j.RepositoryName, "job", j.JobDisplayName, "event", j.EventName)
	if r == nil {
		return
	}
	if ok, reason := s.o.Guard.AllowJob(ctx, j.OwnerName, j.RepositoryName, j.EventName, j.WorkflowRunID); !ok {
		s.o.Metrics.Add(metrics.JobsRefused, s.o.ScaleSet.Name, 1)
		s.o.Logger.Error("stopping runner: job refused by guard", "reason", reason, "runner", j.RunnerName,
			"repo", j.OwnerName+"/"+j.RepositoryName, "event", j.EventName)
		s.stopRunner(ctx, j.RunnerName, "guard")
	}
}

// Reconcile starts runners up to the demand and the host capacity, removes
// surplus idle runners and reports capacity. It is safe to call at any time.
func (s *Scaler) Reconcile(ctx context.Context) {
	ss := s.o.ScaleSet
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		count := len(s.runners) + s.starting
		desired := min(ss.MaxRunners, ss.MinRunners+s.assigned)
		if count >= desired {
			s.mu.Unlock()
			break
		}
		slot, ok, reason := s.o.Pool.TryAcquire(ss.Heavy)
		if !ok {
			s.mu.Unlock()
			s.o.Metrics.Set(metrics.CapacityBlocked, ss.Name, 1)
			s.o.Logger.Debug("waiting for capacity", "reason", reason, "desired", desired, "have", count)
			s.report()
			return
		}
		s.starting++
		s.mu.Unlock()

		r, err := s.start(ctx, slot)
		s.mu.Lock()
		s.starting--
		if err == nil {
			if s.closed {
				s.mu.Unlock()
				s.teardown(r, false)
				return
			}
			s.runners[r.name] = r
			s.wg.Add(1)
			go s.watch(r)
		}
		s.mu.Unlock()
		if err != nil {
			slot.Release()
			s.o.Metrics.Add(metrics.StartErrors, ss.Name, 1)
			s.o.Logger.Error("runner start failed", "error", err)
			s.report()
			return // retried on the next message or tick
		}
		s.o.Metrics.Add(metrics.RunnersStarted, ss.Name, 1)
	}
	s.o.Metrics.Set(metrics.CapacityBlocked, ss.Name, 0)
	s.trimIdle(ctx)
	s.report()
}

func (s *Scaler) start(ctx context.Context, slot *pool.Slot) (*runnerState, error) {
	name := RunnerName(s.o.Host, s.o.ScaleSet.Name)
	s.mu.Lock()
	scaleSetID := s.o.ScaleSetID
	s.mu.Unlock()
	if scaleSetID == 0 {
		return nil, fmt.Errorf("scale set not registered yet")
	}
	jit, err := s.o.JIT.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, scaleSetID)
	if err != nil {
		return nil, fmt.Errorf("jit config: %w", err)
	}
	var runnerID int64
	if jit.Runner != nil {
		runnerID = int64(jit.Runner.ID)
	}
	h, err := s.o.Backend.Start(ctx, backend.RunnerSpec{
		Name:      name,
		ScaleSet:  s.o.ScaleSet.Name,
		JITConfig: jit.EncodedJITConfig,
		Image:     s.o.ScaleSet.Image,
		DinD:      s.o.ScaleSet.DinD,
		Limits:    s.o.ScaleSet.Limits,
	})
	if err != nil {
		if runnerID != 0 {
			_ = s.o.JIT.RemoveRunner(context.WithoutCancel(ctx), runnerID)
		}
		return nil, fmt.Errorf("backend %s: %w", s.o.Backend.Name(), err)
	}
	s.o.Logger.Info("runner started", "runner", name, "backend", s.o.Backend.Name())
	return &runnerState{name: name, runnerID: runnerID, handle: h, slot: slot, started: time.Now()}, nil
}

// watch waits for the runner to exit (or its time limit) and cleans up.
func (s *Scaler) watch(r *runnerState) {
	defer s.wg.Done()
	ctx := context.Background()
	if s.o.MaxJobDuration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.o.MaxJobDuration)
		defer cancel()
	}
	code, err := s.o.Backend.Wait(ctx, r.handle)
	if ctx.Err() != nil {
		s.o.Logger.Error("runner exceeded max_job_duration, stopping", "runner", r.name)
	}
	s.mu.Lock()
	delete(s.runners, r.name)
	forced := r.stopping
	wasBusy := r.busy
	s.mu.Unlock()

	s.teardown(r, wasBusy)
	switch {
	case err == nil && code == 0 && !forced:
		s.o.Metrics.Add(metrics.RunnersCompleted, s.o.ScaleSet.Name, 1)
		s.o.Logger.Info("runner finished", "runner", r.name)
	default:
		s.o.Metrics.Add(metrics.RunnersFailed, s.o.ScaleSet.Name, 1)
		s.o.Logger.Warn("runner ended abnormally", "runner", r.name, "exit_code", code, "forced", forced, "error", err)
	}
	s.Reconcile(context.Background())
}

// teardown removes the runner's leftovers, its registration if it never ran
// a job, and returns its slot.
func (s *Scaler) teardown(r *runnerState, ranJob bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := s.o.Backend.Stop(ctx, r.handle); err != nil {
		s.o.Logger.Error("runner cleanup failed", "runner", r.name, "error", err)
	}
	if !ranJob && r.runnerID != 0 {
		if err := s.o.JIT.RemoveRunner(ctx, r.runnerID); err != nil {
			s.o.Logger.Debug("remove runner registration", "runner", r.name, "error", err)
		}
	}
	r.slot.Release()
}

// stopRunner force-stops one runner; watch() does the cleanup.
func (s *Scaler) stopRunner(ctx context.Context, name, why string) {
	s.mu.Lock()
	r := s.runners[name]
	if r == nil || r.stopping {
		s.mu.Unlock()
		return
	}
	r.stopping = true
	s.mu.Unlock()
	s.o.Logger.Warn("stopping runner", "runner", name, "why", why)
	if err := s.o.Backend.Stop(context.WithoutCancel(ctx), r.handle); err != nil {
		s.o.Logger.Error("stop runner", "runner", name, "error", err)
	}
}

// trimIdle stops idle runners nobody needs once the surplus lasted IdleTimeout.
func (s *Scaler) trimIdle(ctx context.Context) {
	ss := s.o.ScaleSet
	s.mu.Lock()
	surplus := len(s.runners) + s.starting - min(ss.MaxRunners, ss.MinRunners+s.assigned)
	if surplus <= 0 {
		s.surplusSince = time.Time{}
		s.mu.Unlock()
		return
	}
	if s.surplusSince.IsZero() {
		s.surplusSince = time.Now()
	}
	if time.Since(s.surplusSince) < s.o.IdleTimeout {
		s.mu.Unlock()
		return
	}
	var victims []string
	for name, r := range s.runners {
		if len(victims) == surplus {
			break
		}
		if !r.busy && !r.stopping && time.Since(r.started) >= s.o.IdleTimeout {
			victims = append(victims, name)
		}
	}
	s.surplusSince = time.Time{}
	s.mu.Unlock()
	for _, name := range victims {
		s.stopRunner(ctx, name, "idle surplus")
	}
}

// report publishes gauges and tells the listener the capacity GitHub may use:
// what we already run plus what the host could still start for this scale set.
func (s *Scaler) report() {
	ss := s.o.ScaleSet
	s.mu.Lock()
	count := len(s.runners) + s.starting
	busy := 0
	for _, r := range s.runners {
		if r.busy {
			busy++
		}
	}
	s.mu.Unlock()
	capacity := min(ss.MaxRunners, count+s.o.Pool.Free(ss.Heavy))
	s.o.Metrics.Set(metrics.RunnersActive, ss.Name, float64(count))
	s.o.Metrics.Set(metrics.RunnersBusy, ss.Name, float64(busy))
	if s.o.OnCapacity != nil {
		s.o.OnCapacity(capacity)
	}
}

// StopAll force-stops every runner (kill switch, shutdown).
func (s *Scaler) StopAll(ctx context.Context, why string) {
	s.mu.Lock()
	names := make([]string, 0, len(s.runners))
	for n := range s.runners {
		names = append(names, n)
	}
	s.mu.Unlock()
	for _, n := range names {
		s.stopRunner(ctx, n, why)
	}
}

// Close stops every runner and waits for their cleanup. The scaler starts
// nothing afterwards.
func (s *Scaler) Close(ctx context.Context) {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.StopAll(ctx, "shutdown")
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Count returns alive and busy runners.
func (s *Scaler) Count() (alive, busy int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runners {
		if r.busy {
			busy++
		}
	}
	return len(s.runners), busy
}

// RunnerName builds a unique runner name that fits GitHub's 64-character limit.
func RunnerName(host, scaleSet string) string {
	prefix := "srg-" + host + "-" + scaleSet
	if len(prefix) > 54 {
		prefix = prefix[:54]
	}
	return prefix + "-" + randHex(4)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
