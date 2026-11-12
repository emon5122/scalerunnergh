package backend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emon5122/scalerunnergh/internal/config"
	"github.com/emon5122/scalerunnergh/internal/runnerdist"
)

// keepLogs is how many per-runner log files the native backend keeps.
const keepLogs = 50

// Native runs each runner as a host process in its own fresh directory
// (<root>/jobs/<runner>), a copy of the pinned actions/runner release. The
// directory is removed after the job, so nothing carries over between jobs.
type Native struct {
	cfg    config.NativeBackendConfig
	logger *slog.Logger

	distOnce sync.Once
	distDir  string
	distErr  error
	runAs    *runAsAccount // native.run_as (Windows), set by Prepare

	mu    sync.Mutex
	procs map[string]*nativeProc // job dir -> process
}

type nativeProc struct {
	cmd  *exec.Cmd
	done chan struct{}
	code int
	err  error
	log  *os.File
	tree procTree // the runner and everything it started
}

// NewNative creates the native backend.
func NewNative(cfg config.NativeBackendConfig, logger *slog.Logger) *Native {
	if logger == nil {
		logger = slog.Default()
	}
	return &Native{cfg: cfg, logger: logger, procs: map[string]*nativeProc{}}
}

// Name implements Backend.
func (n *Native) Name() string { return config.BackendNative }

func (n *Native) jobsDir() string { return filepath.Join(n.cfg.Root, "jobs") }
func (n *Native) logsDir() string { return filepath.Join(n.cfg.Root, "logs") }

// Prepare downloads and verifies the pinned runner release (once).
func (n *Native) Prepare(ctx context.Context) error {
	n.distOnce.Do(func() {
		n.distDir, n.distErr = runnerdist.Ensure(ctx, n.cfg.Root, n.cfg.RunnerVersion, n.cfg.RunnerSHA256, n.cfg.RunnerURL)
		if n.distErr != nil || n.cfg.RunAs == "" {
			return
		}
		if n.runAs, n.distErr = openRunAs(n.cfg.RunAs); n.distErr != nil {
			return
		}
		if n.distErr = os.MkdirAll(n.jobsDir(), 0o755); n.distErr == nil {
			n.distErr = n.runAs.grantJobsDir(n.jobsDir())
		}
		if n.distErr == nil {
			n.logger.Info("native jobs run as", "account", n.cfg.RunAs)
		}
	})
	return n.distErr
}

// Start implements Backend.
func (n *Native) Start(ctx context.Context, spec RunnerSpec) (Handle, error) {
	if err := n.Prepare(ctx); err != nil {
		return Handle{}, err
	}
	name := config.SanitizeName(spec.Name)
	if name == "" {
		return Handle{}, errors.New("native: empty runner name")
	}
	dir := filepath.Join(n.jobsDir(), name)
	if _, err := os.Stat(dir); err == nil {
		return Handle{}, fmt.Errorf("native: job dir %s already exists", dir)
	}
	if err := os.MkdirAll(n.logsDir(), 0o755); err != nil {
		return Handle{}, err
	}
	if err := runnerdist.CopyTree(n.distDir, dir); err != nil {
		removeAll(dir)
		return Handle{}, fmt.Errorf("native: copy runner: %w", err)
	}
	logf, err := os.Create(filepath.Join(n.logsDir(), name+".log"))
	if err != nil {
		removeAll(dir)
		return Handle{}, err
	}

	cmd := runnerCommand(dir)
	cmd.Dir = dir
	cmd.Env = runnerEnv(os.Environ(), n.jobEnv(), spec.JITConfig)
	cmd.Stdout, cmd.Stderr = logf, logf
	setProcAttrs(cmd, spec.Limits.Nice, n.runAs)
	if err := cmd.Start(); err != nil {
		logf.Close()
		removeAll(dir)
		return Handle{}, fmt.Errorf("native: start runner: %w", err)
	}
	tree := afterStart(cmd, spec.Limits.Nice)
	_ = os.WriteFile(filepath.Join(dir, ".srg-pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)

	p := &nativeProc{cmd: cmd, done: make(chan struct{}), log: logf, tree: tree}
	go func() {
		err := cmd.Wait()
		var ee *exec.ExitError
		switch {
		case err == nil:
			p.code = 0
		case errors.As(err, &ee):
			p.code = ee.ExitCode()
		default:
			p.code, p.err = -1, err
		}
		logf.Close()
		close(p.done)
	}()
	n.mu.Lock()
	n.procs[dir] = p
	n.mu.Unlock()
	return Handle{ID: dir, Name: spec.Name}, nil
}

// Wait implements Backend.
func (n *Native) Wait(ctx context.Context, h Handle) (int, error) {
	n.mu.Lock()
	p := n.procs[h.ID]
	n.mu.Unlock()
	if p == nil {
		return -1, fmt.Errorf("native: unknown runner %s", h.Name)
	}
	select {
	case <-p.done:
		return p.code, p.err
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

// Stop implements Backend: kill the process tree, then delete the job dir.
func (n *Native) Stop(ctx context.Context, h Handle) error {
	n.mu.Lock()
	p := n.procs[h.ID]
	delete(n.procs, h.ID)
	n.mu.Unlock()
	if p != nil {
		select {
		case <-p.done:
		default:
			p.tree.kill(false)
			select {
			case <-p.done:
			case <-time.After(10 * time.Second):
				p.tree.kill(true)
				select {
				case <-p.done:
				case <-time.After(10 * time.Second):
				}
			}
		}
		// Children that outlived the runner, then release the handle.
		p.tree.killOrphans()
		p.tree.close()
	}
	if !n.ownsDir(h.ID) {
		return fmt.Errorf("native: refusing to remove %q (outside %s)", h.ID, n.jobsDir())
	}
	err := removeAll(h.ID)
	n.pruneLogs()
	return err
}

// ownsDir reports whether dir is a direct child of the jobs dir.
func (n *Native) ownsDir(dir string) bool {
	return dir != "" && filepath.Dir(filepath.Clean(dir)) == filepath.Clean(n.jobsDir())
}

// Cleanup removes job dirs (and their processes) left by an earlier process.
func (n *Native) Cleanup(ctx context.Context) error {
	entries, err := os.ReadDir(n.jobsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var firstErr error
	for _, e := range entries {
		dir := filepath.Join(n.jobsDir(), e.Name())
		n.mu.Lock()
		_, live := n.procs[dir]
		n.mu.Unlock()
		if live {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(dir, ".srg-pid")); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 && killLeftover(pid, dir) {
				n.logger.Warn("killed leftover runner process", "pid", pid, "dir", dir)
			}
		}
		if err := removeAll(dir); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (n *Native) pruneLogs() {
	entries, err := os.ReadDir(n.logsDir())
	if err != nil || len(entries) <= keepLogs {
		return
	}
	type lf struct {
		path string
		mod  time.Time
	}
	var files []lf
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() {
			files = append(files, lf{filepath.Join(n.logsDir(), e.Name()), info.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	for _, f := range files[min(keepLogs, len(files)):] {
		os.Remove(f.path)
	}
}

// removeAll retries, because Windows keeps files locked briefly after a kill.
func removeAll(dir string) error {
	var err error
	for i := 0; i < 10; i++ {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return err
}

// jobEnv is native.env on top of the run_as account's profile variables.
func (n *Native) jobEnv() map[string]string {
	if n.runAs == nil {
		return n.cfg.Env
	}
	m := n.runAs.env()
	for k, v := range n.cfg.Env {
		m[k] = v
	}
	return m
}

// runnerEnv is the filtered orchestrator environment, then the configured
// native.env (it wins over inherited values), then the JIT config last.
func runnerEnv(base []string, extra map[string]string, jit string) []string {
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := FilterEnv(base)
	out := make([]string, 0, len(env)+len(keys)+1)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := extra[k]; !ok {
			out = append(out, kv)
		}
	}
	for _, k := range keys {
		out = append(out, k+"="+extra[k])
	}
	return append(out, "ACTIONS_RUNNER_INPUT_JITCONFIG="+jit)
}

// FilterEnv drops credentials and orchestrator settings from the environment
// handed to a native runner.
func FilterEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		up := strings.ToUpper(key)
		if strings.HasPrefix(up, "SRG_") || strings.HasPrefix(up, "GITHUB_") || strings.HasPrefix(up, "ACTIONS_") ||
			strings.Contains(up, "TOKEN") || strings.Contains(up, "SECRET") || strings.Contains(up, "PASSWORD") ||
			strings.Contains(up, "CREDENTIAL") || strings.HasSuffix(up, "_KEY") || strings.HasSuffix(up, "_PAT") || up == "PAT" {
			continue
		}
		out = append(out, kv)
	}
	return out
}
