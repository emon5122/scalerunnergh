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
const (
	keepLogs         = 50
	quarantinePrefix = "quarantine-"
	quarantineMaxAge = 7 * 24 * time.Hour
)

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
	// claimed job dirs are being started into, stopped or swept by one actor (gc.go claim); guarded by mu.
	claimed map[string]bool
	// removeDir is the filesystem remover used by cleanup and GC. Kept on the
	// backend so removal failures can be handled without making startup fatal.
	removeDir func(string) error
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
	return &Native{cfg: cfg, logger: logger, procs: map[string]*nativeProc{}, claimed: map[string]bool{}}
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
	// The gc sweep must never take a dir mid-copy: hold it until procs owns it.
	if !n.claim(dir) {
		return Handle{}, fmt.Errorf("native: job dir %s is in use", dir)
	}
	defer n.release(dir)
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
	// Hold the dir while it is torn down, so the gc sweep does not race the removal. When the sweep holds it
	// already, it is removing it anyway.
	owned := !n.claimed[h.ID]
	n.claimed[h.ID] = true
	n.mu.Unlock()
	if owned {
		defer n.release(h.ID)
	}
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
		n.logger.Warn("native: could not read jobs dir during startup cleanup", "dir", n.jobsDir(), "error", err)
		return nil
	}
	for _, e := range entries {
		dir := filepath.Join(n.jobsDir(), e.Name())
		if isQuarantineDir(dir) {
			info, err := e.Info()
			if err != nil || quarantineAge(dir, info.ModTime(), time.Now()) <= quarantineMaxAge {
				continue // young quarantine dirs are left alone so startup stays fast
			}
			if err := n.removeQuarantineDir(dir); err != nil {
				n.logger.Warn("native: old quarantine dir could not be removed", "dir", dir, "error", err)
			}
			continue
		}
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
		if err := n.removeJobDir(dir); err != nil {
			quarantined, renameErr := quarantineJobDir(dir)
			if renameErr != nil {
				n.logger.Warn("native: leftover job dir could not be removed or quarantined", "dir", dir, "error", err, "quarantine_error", renameErr)
			} else {
				n.logger.Warn("native: leftover job dir quarantined", "dir", dir, "quarantine", quarantined, "error", err)
			}
		}
	}
	return nil
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

// removeAll removes a job dir, retrying because Windows keeps files locked
// briefly after a kill.
func removeAll(dir string) error { return removeAllTries(dir, 10) }

// removeAllTries tries plain os.RemoveAll first (the common case, one walk).
// Only after a failure does it make directories owner-writable: Go's RemoveAll
// cannot remove entries from a read-only directory, as in a Go module cache.
func removeAllTries(dir string, tries int) error {
	var err error
	for i := 0; i < tries; i++ {
		if i > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		// Still try RemoveAll when the chmod walk fails: some entries may have changed since.
		chmodErr := makeTreeWritable(dir)
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		err = errors.Join(err, chmodErr)
	}
	return err
}

// makeTreeWritable adds owner read/write/execute to directories before walking
// them, so even mode-000 or read-only descendants can be traversed and cleared.
func makeTreeWritable(root string) error {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return err
	}
	mode := info.Mode().Perm() | 0o700
	if err := os.Chmod(root, mode); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	var walkErr error
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if entry.IsDir() {
			walkErr = errors.Join(walkErr, makeTreeWritable(path))
		}
	}
	return walkErr
}

func (n *Native) removeJobDir(dir string) error {
	if n.removeDir != nil {
		return n.removeDir(dir)
	}
	return removeAll(dir)
}

// removeQuarantineDir makes one attempt: a quarantine dir already failed the
// retries, and a permanently stuck one must not cost seconds on every sweep and
// every startup.
func (n *Native) removeQuarantineDir(dir string) error {
	if n.removeDir != nil {
		return n.removeDir(dir)
	}
	return removeAllTries(dir, 1)
}

// quarantineJobDir keeps failed removals under jobs/ so startup can continue
// and a later cleanup or GC sweep can retry them. The name carries the
// quarantine time (rename does not touch the dir's mtime).
func quarantineJobDir(dir string) (string, error) {
	name := filepath.Base(dir)
	quarantine := filepath.Join(filepath.Dir(dir), quarantinePrefix+name+"-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.Rename(dir, quarantine); err != nil {
		return "", err
	}
	return quarantine, nil
}

func isQuarantineDir(dir string) bool {
	return strings.HasPrefix(filepath.Base(dir), quarantinePrefix)
}

// minQuarantineStamp: a name suffix below this (2020-01-01 in Unix nanoseconds)
// is not a quarantine stamp, e.g. a short number in a runner name.
const minQuarantineStamp = 1577836800 * int64(time.Second)

// quarantineAge is how long ago dir was quarantined: from the nanosecond stamp
// quarantineJobDir puts in the name, else (no plausible stamp) from mtime.
func quarantineAge(dir string, mod time.Time, now time.Time) time.Duration {
	base := filepath.Base(dir)
	if i := strings.LastIndexByte(base, '-'); i >= 0 {
		if ns, err := strconv.ParseInt(base[i+1:], 10, 64); err == nil && ns >= minQuarantineStamp && ns <= now.UnixNano() {
			return now.Sub(time.Unix(0, ns))
		}
	}
	return now.Sub(mod)
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
