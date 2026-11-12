// Package app wires one host together: backends, the shared slot pool, the
// kill switch, and one listener + scaler per runner scale set.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/emon5122/scalerunnergh/internal/backend"
	"github.com/emon5122/scalerunnergh/internal/config"
	"github.com/emon5122/scalerunnergh/internal/ghapi"
	"github.com/emon5122/scalerunnergh/internal/guard"
	"github.com/emon5122/scalerunnergh/internal/hostgate"
	"github.com/emon5122/scalerunnergh/internal/metrics"
	"github.com/emon5122/scalerunnergh/internal/pool"
	"github.com/emon5122/scalerunnergh/internal/scaler"
)

// App runs every scale set of one host.
type App struct {
	Config  *config.Config
	Version string
	Logger  *slog.Logger
	Metrics *metrics.Collector
	Pool    *pool.Pool

	mu      sync.Mutex
	scalers []*scaler.Scaler
}

func (a *App) scalerList() []*scaler.Scaler {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*scaler.Scaler(nil), a.scalers...)
}

// New creates the app and its slot pool.
func New(cfg *config.Config, version string, logger *slog.Logger, met *metrics.Collector) *App {
	gate := hostgate.LoadGate{MaxLoadPerCore: cfg.Host.MaxLoadPerCore, MinMemAvailableMB: cfg.Host.MinMemAvailableMB}
	return &App{
		Config:  cfg,
		Version: version,
		Logger:  logger,
		Metrics: met,
		Pool:    pool.New(cfg.Host.MaxRunners, cfg.Host.MaxHeavy, gate.Check),
	}
}

// Run blocks until ctx is cancelled, then stops every runner.
func (a *App) Run(ctx context.Context) error {
	cfg := a.Config
	backends := map[string]backend.Backend{}
	if cfg.UsesBackend(config.BackendDocker) {
		backends[config.BackendDocker] = backend.NewDocker(cfg.Docker, cfg.Host.Name)
	}
	if cfg.UsesBackend(config.BackendNative) {
		n := backend.NewNative(cfg.Native, a.Logger)
		a.Logger.Info("preparing pinned runner release", "version", cfg.Native.RunnerVersion)
		if err := n.Prepare(ctx); err != nil {
			return err
		}
		backends[config.BackendNative] = n
	}
	// Crash safety: nothing of an earlier process survives.
	for name, b := range backends {
		if err := b.Cleanup(ctx); err != nil {
			return fmt.Errorf("cleanup %s backend: %w", name, err)
		}
	}
	a.Metrics.Set(metrics.HostSlotsMax, "", float64(cfg.Host.MaxRunners))

	ks := hostgate.KillSwitch{Path: cfg.Host.KillSwitchFile, Logger: a.Logger}
	// Evaluate the kill switch before any listener starts.
	a.Pool.SetPaused(ks.Active())

	// Listeners outlive ctx during the drain, so job-started messages still arrive.
	runCtx, stopListeners := context.WithCancel(context.WithoutCancel(ctx))
	defer stopListeners()
	var wg sync.WaitGroup
	for _, ss := range cfg.ScaleSets {
		svc, err := a.newService(ss, backends[ss.Backend])
		if err != nil {
			stopListeners()
			wg.Wait()
			return fmt.Errorf("scale set %s: %w", ss.Name, err)
		}
		a.mu.Lock()
		a.scalers = append(a.scalers, svc.scaler)
		a.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.run(runCtx)
		}()
	}

	ks.OnChange = func(active bool) {
		if ctx.Err() != nil {
			return // shutting down: the pool stays paused
		}
		a.Pool.SetPaused(active)
		paused := 0.0
		if active {
			paused = 1
			if cfg.Host.KillSwitchStopsRunning {
				for _, s := range a.scalerList() {
					s.StopAll(context.Background(), "kill switch")
				}
			}
		}
		a.Metrics.Set(metrics.HostPaused, "", paused)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		ks.Run(runCtx)
	}()

	<-ctx.Done()
	a.Pool.SetPaused(true) // no new runners from here on
	for _, s := range a.scalerList() {
		s.SetDraining() // and no new jobs acquired
	}
	a.drain(cfg.Host.DrainTimeout.Duration)
	a.Logger.Info("shutting down: stopping runners")
	stopListeners()
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, s := range a.scalerList() {
		s.Close(stopCtx)
	}
	wg.Wait()
	return nil
}

// drainSettle is how long idle runners get to pick up an already assigned job
// before the drain treats them as unused.
var drainSettle = 20 * time.Second

// drain waits for running jobs to finish, up to timeout. Runners that stay
// idle are not waited for.
func (a *App) drain(timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	start := time.Now()
	a.Logger.Info("draining: waiting for running jobs", "timeout", timeout)
	for time.Since(start) < timeout {
		alive, busy := 0, 0
		for _, s := range a.scalerList() {
			al, b := s.Count()
			alive, busy = alive+al, busy+b
		}
		if alive == 0 || (busy == 0 && time.Since(start) >= drainSettle) {
			a.Logger.Info("drain complete", "after", time.Since(start).Round(time.Second))
			return
		}
		time.Sleep(time.Second)
	}
	a.Logger.Warn("drain timeout: stopping jobs that are still running")
}

// Status is served at /status.
type Status struct {
	Host      string           `json:"host"`
	Paused    bool             `json:"paused"`
	SlotsUsed int              `json:"slots_used"`
	SlotsMax  int              `json:"slots_max"`
	HeavyUsed int              `json:"heavy_used"`
	ScaleSets []ScaleSetStatus `json:"scale_sets"`
}

// ScaleSetStatus is one scale set in Status.
type ScaleSetStatus struct {
	Name    string `json:"name"`
	Runners int    `json:"runners"`
	Busy    int    `json:"busy"`
}

// Status returns the current host state.
func (a *App) Status() Status {
	used, heavy := a.Pool.Used()
	st := Status{Host: a.Config.Host.Name, Paused: a.Pool.Paused(), SlotsUsed: used, SlotsMax: a.Config.Host.MaxRunners, HeavyUsed: heavy}
	for i, s := range a.scalerList() {
		alive, busy := s.Count()
		st.ScaleSets = append(st.ScaleSets, ScaleSetStatus{Name: a.Config.ScaleSets[i].Name, Runners: alive, Busy: busy})
	}
	return st
}

type service struct {
	app    *App
	ss     config.ScaleSetConfig
	owner  string
	repo   string
	client *scaleset.Client
	guard  *guard.Guard
	scaler *scaler.Scaler
	logger *slog.Logger

	mu       sync.Mutex
	listener *listener.Listener
	capacity int
}

func (a *App) newService(ss config.ScaleSetConfig, b backend.Backend) (*service, error) {
	owner, repo, err := config.ParseGitHubURL(ss.URL)
	if err != nil {
		return nil, err
	}
	auth := a.Config.EffectiveAuth(ss)
	info := scaleset.SystemInfo{System: "scalerunnergh", Version: a.Version, Subsystem: "listener"}
	var client *scaleset.Client
	var tokens ghapi.TokenSource
	if auth.App != nil {
		key, err := config.ReadSecretFile(auth.App.PrivateKeyFile)
		if err != nil {
			return nil, err
		}
		client, err = scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: ss.URL,
			GitHubAppAuth:   scaleset.GitHubAppAuth{ClientID: auth.App.ClientID, InstallationID: auth.App.InstallationID, PrivateKey: key},
			SystemInfo:      info,
		})
		if err != nil {
			return nil, err
		}
		tokens = &ghapi.AppToken{ClientID: auth.App.ClientID, InstallationID: auth.App.InstallationID, PrivateKeyPEM: key}
	} else {
		pat, err := config.ReadSecretFile(auth.PATFile)
		if err != nil {
			return nil, err
		}
		client, err = scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL: ss.URL, PersonalAccessToken: pat, SystemInfo: info,
		})
		if err != nil {
			return nil, err
		}
		tokens = ghapi.StaticToken(pat)
	}
	svc := &service{
		app: a, ss: ss, owner: owner, repo: repo, client: client,
		guard:  guard.New(&ghapi.Client{Tokens: tokens}, owner, ss.AllowRepos),
		logger: a.Logger.With("scale_set", ss.Name),
	}
	svc.scaler = scaler.New(scaler.Options{
		Host:           a.Config.Host.Name,
		ScaleSet:       ss,
		Backend:        b,
		Pool:           a.Pool,
		Guard:          svc.guard,
		JIT:            client,
		Metrics:        a.Metrics,
		Logger:         a.Logger,
		IdleTimeout:    a.Config.Host.IdleTimeout.Duration,
		MaxJobDuration: a.Config.Host.MaxJobDuration.Duration,
		OnCapacity:     svc.setCapacity,
	})
	return svc, nil
}

func (s *service) setCapacity(n int) {
	s.mu.Lock()
	s.capacity = n
	if s.listener != nil {
		s.listener.SetMaxRunners(n)
	}
	s.mu.Unlock()
	used, _ := s.app.Pool.Used()
	s.app.Metrics.Set(metrics.HostSlotsUsed, "", float64(used))
}

// run keeps the scale set registered and its message session alive.
func (s *service) run(ctx context.Context) {
	// Reconcile on pool changes and on a timer (blocked starts, idle trim).
	var bg sync.WaitGroup
	defer bg.Wait() // run() returns only after a reconcile in flight has finished
	bg.Add(1)
	go func() {
		defer bg.Done()
		sub := s.app.Pool.Subscribe()
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sub:
			case <-t.C:
			}
			s.scaler.Reconcile(ctx)
		}
	}()

	const minBackoff = 5 * time.Second
	backoff := minBackoff
	for ctx.Err() == nil {
		began := time.Now()
		err := s.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(began) > 2*time.Minute {
			backoff = minBackoff // the session worked for a while: start over
		}
		s.app.Metrics.Add(metrics.ListenerErrors, s.ss.Name, 1)
		s.logger.Error("listener stopped, retrying", "error", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 2*time.Minute)
	}
}

// session registers the scale set (after the guard approves its target),
// opens a message session and listens until an error or ctx ends.
func (s *service) session(ctx context.Context) error {
	if err := s.guard.CheckTarget(ctx, s.owner, s.repo, s.ss.RunnerGroup); err != nil {
		return err
	}
	id, err := s.ensureScaleSet(ctx)
	if err != nil {
		return err
	}
	s.scaler.SetScaleSetID(id)
	sess, err := s.client.MessageSessionClient(ctx, id, s.app.Config.Host.Name)
	if err != nil {
		return fmt.Errorf("message session: %w", err)
	}
	defer sess.Close(context.WithoutCancel(ctx))
	s.scaler.SetAcquirer(sess)

	s.mu.Lock()
	l, err := listener.New(sess, listener.Config{ScaleSetID: id, MaxRunners: s.capacity})
	if err == nil {
		s.listener = l
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.logger.Info("listening", "scale_set_id", id, "backend", s.ss.Backend)
	err = l.Run(ctx, s.scaler)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (s *service) ensureScaleSet(ctx context.Context) (int, error) {
	group, err := s.client.GetRunnerGroupByName(ctx, s.ss.RunnerGroup)
	if err != nil {
		return 0, fmt.Errorf("runner group %q: %w", s.ss.RunnerGroup, err)
	}
	want := []scaleset.Label{{Name: s.ss.Name}}
	for _, l := range s.ss.Labels {
		if l != s.ss.Name {
			want = append(want, scaleset.Label{Name: l})
		}
	}
	existing, err := s.client.GetRunnerScaleSet(ctx, group.ID, s.ss.Name)
	if err != nil {
		return 0, fmt.Errorf("get scale set: %w", err)
	}
	desired := &scaleset.RunnerScaleSet{
		Name:          s.ss.Name,
		RunnerGroupID: group.ID,
		Labels:        want,
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	}
	if existing == nil {
		created, err := s.client.CreateRunnerScaleSet(ctx, desired)
		if err != nil {
			return 0, fmt.Errorf("create scale set: %w", err)
		}
		s.logger.Info("scale set created", "id", created.ID, "group", group.Name)
		return created.ID, nil
	}
	if !slices.Equal(labelNames(existing.Labels), labelNames(want)) {
		if _, err := s.client.UpdateRunnerScaleSet(ctx, existing.ID, desired); err != nil {
			return 0, fmt.Errorf("update scale set labels: %w", err)
		}
		s.logger.Info("scale set labels updated", "id", existing.ID)
	}
	return existing.ID, nil
}

func labelNames(ls []scaleset.Label) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Name)
	}
	slices.Sort(out)
	return out
}
