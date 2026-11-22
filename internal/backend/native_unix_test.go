//go:build !windows

package backend

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emon5122/scalerunnergh/internal/config"
)

// fakeRunner builds a runner release whose run.sh writes what it saw to
// $SRG_TEST_OUT-like files inside its own dir, then follows the script body.
func fakeRunner(t *testing.T, body string) config.NativeBackendConfig {
	t.Helper()
	root := t.TempDir()
	archive := filepath.Join(root, "runner.tar.gz")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	script := "#!/bin/sh\n" + body
	tw.WriteHeader(&tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "run.sh", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(script))})
	tw.Write([]byte(script))
	tw.WriteHeader(&tar.Header{Name: "bin/Runner.Listener", Typeflag: tar.TypeReg, Mode: 0o755, Size: 4})
	tw.Write([]byte("fake"))
	tw.Close()
	gz.Close()
	f.Close()
	b, _ := os.ReadFile(archive)
	sum := sha256.Sum256(b)
	return config.NativeBackendConfig{Root: filepath.Join(root, "srg"), RunnerVersion: "9.9.9",
		RunnerSHA256: hex.EncodeToString(sum[:]), RunnerURL: "file://" + archive}
}

func TestNativeRunsJobInCleanDir(t *testing.T) {
	t.Setenv("SRG_LEAK", "1")
	t.Setenv("DEPLOY_TOKEN", "leak")
	out := filepath.Join(t.TempDir(), "seen")
	cfg := fakeRunner(t, `echo "jit=$ACTIONS_RUNNER_INPUT_JITCONFIG leak=${SRG_LEAK}${DEPLOY_TOKEN} pwd=$(pwd)" > "`+out+`"
mkdir -p _work && echo dirty > _work/state
echo hello-from-runner
exit 3
`)
	n := NewNative(cfg, nil)
	ctx := context.Background()
	h, err := n.Start(ctx, RunnerSpec{Name: "srg-h-s-0001", JITConfig: "JIT123", Limits: config.Limits{Nice: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(h.ID) != filepath.Join(cfg.Root, "jobs") {
		t.Fatalf("job dir %s not under jobs/", h.ID)
	}
	code, err := n.Wait(ctx, h)
	if err != nil || code != 3 {
		t.Fatalf("wait: %d %v", code, err)
	}
	seen, _ := os.ReadFile(out)
	if want := "jit=JIT123 leak= pwd=" + h.ID; strings.TrimSpace(string(seen)) != want {
		t.Fatalf("runner saw %q, want %q", seen, want)
	}
	if _, err := os.Stat(filepath.Join(h.ID, "_work", "state")); err != nil {
		t.Fatal("job state should exist before Stop")
	}
	if err := n.Stop(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.ID); !os.IsNotExist(err) {
		t.Fatal("job dir must be removed after the job")
	}
	if err := n.Stop(ctx, h); err != nil {
		t.Fatalf("second Stop must be a no-op: %v", err)
	}
	logb, _ := os.ReadFile(filepath.Join(cfg.Root, "logs", "srg-h-s-0001.log"))
	if !strings.Contains(string(logb), "hello-from-runner") || strings.Contains(string(logb), "JIT123") {
		t.Fatalf("log: %q", logb)
	}
	// The shared release is untouched and a second job starts clean.
	if _, err := os.Stat(filepath.Join(cfg.Root, "dist", "9.9.9", "_work")); !os.IsNotExist(err) {
		t.Fatal("job state leaked into the shared release")
	}
	h2, err := n.Start(ctx, RunnerSpec{Name: "srg-h-s-0002", JITConfig: "J2"})
	if err != nil {
		t.Fatal(err)
	}
	n.Wait(ctx, h2)
	n.Stop(ctx, h2)
}

func TestNativeStopKillsProcessTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cfg := fakeRunner(t, `sleep 300 &
echo $! > "`+pidFile+`"
wait
`)
	n := NewNative(cfg, nil)
	ctx := context.Background()
	h, err := n.Start(ctx, RunnerSpec{Name: "srg-kill", JITConfig: "J"})
	if err != nil {
		t.Fatal(err)
	}
	var childPid string
	for i := 0; i < 100 && childPid == ""; i++ {
		time.Sleep(20 * time.Millisecond)
		b, _ := os.ReadFile(pidFile)
		childPid = strings.TrimSpace(string(b))
	}
	if childPid == "" {
		t.Fatal("child did not start")
	}
	start := time.Now()
	if err := n.Stop(ctx, h); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatalf("Stop took %v", time.Since(start))
	}
	for i := 0; i < 100; i++ {
		if _, err := os.Stat("/proc/" + childPid); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child %s survived Stop", childPid)
}

func TestNativeCleanupRemovesLeftovers(t *testing.T) {
	cfg := fakeRunner(t, "exit 0\n")
	stale := filepath.Join(cfg.Root, "jobs", "srg-old")
	os.MkdirAll(filepath.Join(stale, "_work"), 0o755)
	os.WriteFile(filepath.Join(stale, ".srg-pid"), []byte("1"), 0o600) // pid 1 must never be killed
	n := NewNative(cfg, nil)
	if err := n.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale job dir must be removed")
	}
	if err := n.Stop(context.Background(), Handle{ID: "/etc"}); err == nil {
		t.Fatal("Stop must refuse a dir outside jobs/")
	}
}

func TestNativeCleanupRemovesReadOnlySubtree(t *testing.T) {
	cfg := fakeRunner(t, "exit 0\n")
	stale := filepath.Join(cfg.Root, "jobs", "srg-readonly")
	readonly := filepath.Join(stale, "go", "pkg", "mod")
	if err := os.MkdirAll(readonly, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readonly, "cache.zip"), []byte("cache"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readonly, 0o555); err != nil {
		t.Fatal(err)
	}
	n := NewNative(cfg, nil)
	if err := n.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("read-only job tree should be removed, stat error: %v", err)
	}
}

func TestNativeCleanupFailureDoesNotStopStartup(t *testing.T) {
	cfg := fakeRunner(t, "exit 0\n")
	stale := filepath.Join(cfg.Root, "jobs", "srg-stuck")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	n := NewNative(cfg, nil)
	n.removeDir = func(string) error { return errors.New("simulated removal failure") }
	if err := n.Cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup failure must be non-fatal: %v", err)
	}
	entries, err := os.ReadDir(n.jobsDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "quarantine-srg-stuck-") {
		t.Fatalf("stuck dir should be quarantined, entries: %v", entries)
	}
	h, err := n.Start(context.Background(), RunnerSpec{Name: "srg-starts", JITConfig: "J"})
	if err != nil {
		t.Fatalf("startup after cleanup failure: %v", err)
	}
	if _, err := n.Wait(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if err := n.Stop(context.Background(), h); err != nil {
		t.Fatal(err)
	}
}

func TestNativeCleanupDoesNotRequarantineAndPrunesOldQuarantine(t *testing.T) {
	cfg := fakeRunner(t, "exit 0\n")
	jobs := filepath.Join(cfg.Root, "jobs")
	stuck := filepath.Join(jobs, "quarantine-srg-stuck-1")
	old := filepath.Join(jobs, "quarantine-srg-old-2")
	young := filepath.Join(jobs, "quarantine-srg-young-3")
	for _, dir := range []string{stuck, old, young} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(stuck, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	var attempts []string
	n := NewNative(cfg, nil)
	n.removeDir = func(dir string) error {
		attempts = append(attempts, dir)
		if dir == stuck {
			return errors.New("simulated removal failure")
		}
		return os.RemoveAll(dir)
	}
	if err := n.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stuck); err != nil {
		t.Fatalf("stuck quarantine dir should keep its name: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old removable quarantine dir should be pruned, stat error: %v", err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Fatalf("young quarantine dir should be left alone: %v", err)
	}
	if len(attempts) != 2 || attempts[0] != old || attempts[1] != stuck {
		t.Fatalf("unexpected removal attempts: %v", attempts)
	}
}

func TestNativeEnvFromConfig(t *testing.T) {
	t.Setenv("ACTIONS_RUNNER_HOOK_JOB_STARTED", "/from/orchestrator/env")
	out := filepath.Join(t.TempDir(), "seen")
	cfg := fakeRunner(t, `echo "hook=$ACTIONS_RUNNER_HOOK_JOB_STARTED java=$JAVA_HOME jit=$ACTIONS_RUNNER_INPUT_JITCONFIG" > "`+out+`"
`)
	cfg.Env = map[string]string{
		"ACTIONS_RUNNER_HOOK_JOB_STARTED": "/opt/hooks/start.sh",
		"JAVA_HOME":                       "/usr/lib/jvm/17",
	}
	n := NewNative(cfg, nil)
	ctx := context.Background()
	h, err := n.Start(ctx, RunnerSpec{Name: "srg-h-s-env1", JITConfig: "JITX"})
	if err != nil {
		t.Fatal(err)
	}
	n.Wait(ctx, h)
	n.Stop(ctx, h)
	seen, _ := os.ReadFile(out)
	if want := "hook=/opt/hooks/start.sh java=/usr/lib/jvm/17 jit=JITX"; strings.TrimSpace(string(seen)) != want {
		t.Fatalf("runner saw %q, want %q", seen, want)
	}
}
