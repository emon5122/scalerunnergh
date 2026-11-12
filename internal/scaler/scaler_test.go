package scaler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/emon5122/scalerunnergh/internal/backend"
	"github.com/emon5122/scalerunnergh/internal/config"
	"github.com/emon5122/scalerunnergh/internal/metrics"
	"github.com/emon5122/scalerunnergh/internal/pool"
)

// fakeBackend keeps runners "running" until finish() or Stop(). Like the real
// backends, Wait returns once the runner is stopped, whichever comes first.
type fakeBackend struct {
	mu       sync.Mutex
	exit     map[string]chan int // never deleted, buffered: Stop before Wait still ends Wait
	running  map[string]bool
	started  []backend.RunnerSpec
	stopped  map[string]int
	startErr error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{exit: map[string]chan int{}, running: map[string]bool{}, stopped: map[string]int{}}
}

func (f *fakeBackend) Name() string { return "fake" }

func (f *fakeBackend) Start(_ context.Context, spec backend.RunnerSpec) (backend.Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return backend.Handle{}, f.startErr
	}
	f.exit[spec.Name] = make(chan int, 1)
	f.running[spec.Name] = true
	f.started = append(f.started, spec)
	return backend.Handle{ID: "id-" + spec.Name, Name: spec.Name}, nil
}

func (f *fakeBackend) Wait(ctx context.Context, h backend.Handle) (int, error) {
	f.mu.Lock()
	ch := f.exit[h.Name]
	f.mu.Unlock()
	select {
	case code := <-ch:
		f.mu.Lock()
		delete(f.running, h.Name)
		f.mu.Unlock()
		return code, nil
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

func (f *fakeBackend) Stop(_ context.Context, h backend.Handle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped[h.Name]++
	delete(f.running, h.Name)
	select {
	case f.exit[h.Name] <- 137:
	default:
	}
	return nil
}

func (f *fakeBackend) Cleanup(context.Context) error { return nil }

func (f *fakeBackend) finish(name string, code int) {
	f.mu.Lock()
	ch := f.exit[name]
	f.mu.Unlock()
	ch <- code
}

func (f *fakeBackend) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for n := range f.running {
		out = append(out, n)
	}
	return out
}

func (f *fakeBackend) stops(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped[name]
}

type fakeJIT struct {
	mu      sync.Mutex
	next    int
	removed []int64
	err     error
}

func (j *fakeJIT) GenerateJitRunnerConfig(_ context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, id int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return nil, j.err
	}
	j.next++
	return &scaleset.RunnerScaleSetJitRunnerConfig{Runner: &scaleset.RunnerReference{ID: j.next, Name: s.Name}, EncodedJITConfig: "jit-" + s.Name}, nil
}

func (j *fakeJIT) RemoveRunner(_ context.Context, id int64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.removed = append(j.removed, id)
	return nil
}

type fakeGuard struct{ deny map[string]string }

func (g fakeGuard) AllowJob(_ context.Context, owner, repo, event string, _ int64) (bool, string) {
	if r, ok := g.deny[repo]; ok {
		return false, r
	}
	if r, ok := g.deny[event]; ok {
		return false, r
	}
	return true, ""
}

type fakeAcquirer struct{ got [][]int64 }

func (a *fakeAcquirer) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	a.got = append(a.got, ids)
	return ids, nil
}

type rig struct {
	s   *Scaler
	b   *fakeBackend
	jit *fakeJIT
	p   *pool.Pool
	m   *metrics.Collector
	acq *fakeAcquirer
	cap []int
	mu  sync.Mutex
}

func newRig(p *pool.Pool, ss config.ScaleSetConfig, g JobGuard) *rig {
	r := &rig{b: newFakeBackend(), jit: &fakeJIT{}, p: p, m: metrics.New(), acq: &fakeAcquirer{}}
	if ss.Name == "" {
		ss.Name = "set"
	}
	if g == nil {
		g = fakeGuard{}
	}
	r.s = New(Options{Host: "h", ScaleSet: ss, ScaleSetID: 5, Backend: r.b, Pool: p, Guard: g, JIT: r.jit, Metrics: r.m,
		IdleTimeout: 30 * time.Millisecond, MaxJobDuration: time.Hour,
		OnCapacity: func(n int) { r.mu.Lock(); r.cap = append(r.cap, n); r.mu.Unlock() }})
	r.s.SetAcquirer(r.acq)
	return r
}

func (r *rig) lastCap() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cap) == 0 {
		return -1
	}
	return r.cap[len(r.cap)-1]
}

func stats(n int) *scaleset.RunnerScaleSetMessage {
	return &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: n}}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", what)
}

func alive(s *Scaler) int { a, _ := s.Count(); return a }

func TestScalesToAssignedJobsWithinMax(t *testing.T) {
	r := newRig(pool.New(10, 0, nil), config.ScaleSetConfig{MaxRunners: 3, Image: "img", DinD: true, Limits: config.Limits{CPUs: 2}}, nil)
	ctx := context.Background()
	r.s.Scale(ctx, stats(2))
	if alive(r.s) != 2 {
		t.Fatalf("want 2 runners, have %d", alive(r.s))
	}
	r.s.Scale(ctx, stats(7))
	if alive(r.s) != 3 {
		t.Fatalf("want max 3 runners, have %d", alive(r.s))
	}
	spec := r.b.started[0]
	if !strings.HasPrefix(spec.Name, "srg-h-set-") || spec.JITConfig != "jit-"+spec.Name || spec.Image != "img" || !spec.DinD || spec.Limits.CPUs != 2 {
		t.Fatalf("spec: %+v", spec)
	}
	if used, _ := r.p.Used(); used != 3 {
		t.Fatalf("pool used %d", used)
	}
	if r.lastCap() != 3 {
		t.Fatalf("capacity reported %d", r.lastCap())
	}

	// A runner finishing its job frees the slot, is cleaned up, keeps its registration removal off.
	name := r.b.names()[0]
	r.s.Scale(ctx, &scaleset.RunnerScaleSetMessage{
		JobStartedMessages: []*scaleset.JobStarted{{RunnerName: name}},
		Statistics:         &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2},
	})
	r.b.finish(name, 0)
	eventually(t, "runner removed", func() bool { return alive(r.s) == 2 })
	eventually(t, "slot released", func() bool { u, _ := r.p.Used(); return u == 2 })
	if r.b.stops(name) != 1 {
		t.Fatalf("backend cleanup calls: %d", r.b.stops(name))
	}
	if len(r.jit.removed) != 0 {
		t.Fatalf("a runner that ran a job is not unregistered by us: %v", r.jit.removed)
	}
	eventually(t, "completion counted", func() bool { return r.m.Get(metrics.RunnersCompleted, "set") == 1 })
	if r.m.Get(metrics.RunnersStarted, "set") != 3 {
		t.Fatal("metrics wrong")
	}
	r.s.Close(ctx)
}

func TestHostPoolSharedAcrossScaleSets(t *testing.T) {
	p := pool.New(2, 1, nil)
	a := newRig(p, config.ScaleSetConfig{Name: "a", MaxRunners: 2, Heavy: true}, nil)
	b := newRig(p, config.ScaleSetConfig{Name: "b", MaxRunners: 2, Heavy: true}, nil)
	ctx := context.Background()
	a.s.Scale(ctx, stats(2))
	b.s.Scale(ctx, stats(2))
	if alive(a.s) != 1 || alive(b.s) != 0 {
		t.Fatalf("one heavy job at a time: a=%d b=%d", alive(a.s), alive(b.s))
	}
	if a.lastCap() != 1 || b.lastCap() != 0 {
		t.Fatalf("capacity: a=%d b=%d", a.lastCap(), b.lastCap())
	}
	if b.m.Get(metrics.CapacityBlocked, "b") != 1 {
		t.Fatal("b should report blocked")
	}
	// a finishes; b's waiting job starts on the next reconcile (pool notification).
	a.s.Scale(ctx, stats(0))
	a.b.finish(a.b.names()[0], 0)
	eventually(t, "a's slot freed", func() bool { u, _ := p.Used(); return u == 0 })
	b.s.Reconcile(ctx)
	if alive(b.s) != 1 {
		t.Fatalf("b should run now, have %d", alive(b.s))
	}
	a.s.Close(ctx)
	b.s.Close(ctx)
}

func TestGuardRefusedJobsAreNotAcquired(t *testing.T) {
	r := newRig(pool.New(5, 0, nil), config.ScaleSetConfig{MaxRunners: 5}, fakeGuard{deny: map[string]string{"public-repo": "public repository"}})
	msg := &scaleset.RunnerScaleSetMessage{JobAvailableMessages: []*scaleset.JobAvailable{
		{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 1, OwnerName: "o", RepositoryName: "app", EventName: "push"}},
		{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 2, OwnerName: "o", RepositoryName: "public-repo", EventName: "push"}},
	}}
	if err := r.s.Scale(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if len(r.acq.got) != 1 || fmt.Sprint(r.acq.got[0]) != "[1]" {
		t.Fatalf("acquired: %v", r.acq.got)
	}
	if r.m.Get(metrics.JobsRefused, "set") != 1 {
		t.Fatal("refusal not counted")
	}
}

func TestRunnerStoppedWhenStartedJobFailsGuard(t *testing.T) {
	r := newRig(pool.New(5, 0, nil), config.ScaleSetConfig{MaxRunners: 5}, fakeGuard{deny: map[string]string{"pull_request": "pull request from a fork"}})
	ctx := context.Background()
	r.s.Scale(ctx, stats(1))
	name := r.b.names()[0]
	r.s.Scale(ctx, &scaleset.RunnerScaleSetMessage{
		JobStartedMessages: []*scaleset.JobStarted{{RunnerName: name, JobMessageBase: scaleset.JobMessageBase{OwnerName: "o", RepositoryName: "app", EventName: "pull_request"}}},
		Statistics:         &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 0},
	})
	eventually(t, "runner stopped", func() bool { return alive(r.s) == 0 })
	eventually(t, "forced stop counted as failed", func() bool { return r.m.Get(metrics.RunnersFailed, "set") == 1 })
	r.s.Close(ctx)
}

func TestPausedPoolStartsNothingThenResumes(t *testing.T) {
	p := pool.New(3, 0, nil)
	r := newRig(p, config.ScaleSetConfig{MaxRunners: 3}, nil)
	ctx := context.Background()
	r.s.Scale(ctx, stats(1))
	p.SetPaused(true)
	r.s.Scale(ctx, stats(3))
	if alive(r.s) != 1 || r.lastCap() != 1 {
		t.Fatalf("paused: runners=%d capacity=%d", alive(r.s), r.lastCap())
	}
	r.s.StopAll(ctx, "kill switch")
	eventually(t, "runners stopped", func() bool { return alive(r.s) == 0 })
	if alive(r.s) != 0 {
		t.Fatal("nothing may restart while paused")
	}
	p.SetPaused(false)
	r.s.Reconcile(ctx)
	if alive(r.s) != 3 {
		t.Fatalf("resumed: %d", alive(r.s))
	}
	r.s.Close(ctx)
}

func TestIdleSurplusRunnersAreRemovedAndUnregistered(t *testing.T) {
	r := newRig(pool.New(5, 0, nil), config.ScaleSetConfig{MaxRunners: 5, MinRunners: 1}, nil)
	ctx := context.Background()
	r.s.Scale(ctx, stats(2)) // min 1 + 2 assigned
	if alive(r.s) != 3 {
		t.Fatalf("want 3, have %d", alive(r.s))
	}
	busy := r.b.names()[0]
	r.s.Scale(ctx, &scaleset.RunnerScaleSetMessage{
		JobStartedMessages: []*scaleset.JobStarted{{RunnerName: busy}},
		Statistics:         &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}, // the other job was cancelled
	})
	r.s.Reconcile(ctx) // starts the surplus clock
	time.Sleep(50 * time.Millisecond)
	r.s.Reconcile(ctx)
	eventually(t, "surplus idle runner removed", func() bool { return alive(r.s) == 2 })
	if r.b.stops(busy) != 0 {
		t.Fatal("the busy runner must never be trimmed")
	}
	eventually(t, "idle runner unregistered", func() bool { r.jit.mu.Lock(); defer r.jit.mu.Unlock(); return len(r.jit.removed) == 1 })
	r.s.Close(ctx)
}

func TestStartErrorsReleaseSlots(t *testing.T) {
	p := pool.New(2, 0, nil)
	r := newRig(p, config.ScaleSetConfig{MaxRunners: 2}, nil)
	ctx := context.Background()
	r.jit.err = errors.New("jit down")
	r.s.Scale(ctx, stats(2))
	if u, _ := p.Used(); u != 0 || alive(r.s) != 0 {
		t.Fatalf("slot leaked on JIT error: used=%d", u)
	}
	r.jit.err = nil
	r.b.startErr = errors.New("docker down")
	r.s.Scale(ctx, stats(2))
	if u, _ := p.Used(); u != 0 {
		t.Fatalf("slot leaked on backend error: used=%d", u)
	}
	if len(r.jit.removed) != 1 {
		t.Fatalf("the unused JIT runner must be unregistered: %v", r.jit.removed)
	}
	if r.m.Get(metrics.StartErrors, "set") != 2 {
		t.Fatal("start errors not counted")
	}
	r.b.startErr = nil
	r.s.Reconcile(ctx)
	if alive(r.s) != 2 {
		t.Fatalf("should recover, have %d", alive(r.s))
	}
	r.s.Close(ctx)
}

func TestMaxJobDurationStopsRunner(t *testing.T) {
	r := newRig(pool.New(1, 0, nil), config.ScaleSetConfig{MaxRunners: 1}, nil)
	r.s.o.MaxJobDuration = 40 * time.Millisecond
	ctx := context.Background()
	r.s.Scale(ctx, stats(1))
	name := r.b.names()[0]
	r.s.Scale(ctx, stats(0))
	eventually(t, "overlong runner stopped", func() bool { return r.b.stops(name) > 0 })
	eventually(t, "slot released", func() bool { u, _ := r.p.Used(); return u == 0 })
	r.s.Close(ctx)
}

func TestCloseStopsEverythingAndStartsNothing(t *testing.T) {
	p := pool.New(3, 0, nil)
	r := newRig(p, config.ScaleSetConfig{MaxRunners: 3}, nil)
	ctx := context.Background()
	r.s.Scale(ctx, stats(3))
	r.s.Close(ctx)
	if alive(r.s) != 0 || len(r.b.names()) != 0 {
		t.Fatal("runners survived Close")
	}
	if u, _ := p.Used(); u != 0 {
		t.Fatalf("slots leaked: %d", u)
	}
	r.s.Scale(ctx, stats(3))
	if alive(r.s) != 0 {
		t.Fatal("a closed scaler must not start runners")
	}
}

func TestNoStartBeforeScaleSetRegistered(t *testing.T) {
	r := newRig(pool.New(1, 0, nil), config.ScaleSetConfig{MaxRunners: 1}, nil)
	r.s.SetScaleSetID(0)
	r.s.Scale(context.Background(), stats(1))
	if u, _ := r.p.Used(); alive(r.s) != 0 || u != 0 {
		t.Fatal("must not start without a scale set id")
	}
}

func TestRunnerNameFitsLimit(t *testing.T) {
	n := RunnerName(strings.Repeat("h", 40), strings.Repeat("s", 45))
	if len(n) > 63 {
		t.Fatalf("name too long: %d", len(n))
	}
	if RunnerName("h", "s") == RunnerName("h", "s") {
		t.Fatal("names must be unique")
	}
}
