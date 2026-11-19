//go:build !windows

package backend

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/emon5122/scalerunnergh/internal/config"
)

func TestNativeSweepJobDirs(t *testing.T) {
	root := t.TempDir()
	n := NewNative(config.NativeBackendConfig{Root: root}, nil)
	jobs := n.jobsDir()
	old := time.Now().Add(-3 * time.Hour)
	mk := func(name string, mod time.Time) string {
		dir := filepath.Join(jobs, name)
		if err := os.MkdirAll(filepath.Join(dir, "_work", "repo"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dir, mod, mod); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	orphan := mk("orphan", old)
	young := mk("young", time.Now())
	live := mk("live", old)
	starting := mk("starting", old)
	n.procs[live] = &nativeProc{done: make(chan struct{})}
	n.claimed[starting] = true
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(jobs, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobs, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	dry, err := n.SweepJobDirs(time.Hour, true)
	if err != nil || !slices.Equal(dry.Removed, []string{orphan}) {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatal("dry run removed the orphan")
	}

	res, err := n.SweepJobDirs(time.Hour, false)
	if err != nil || !slices.Equal(res.Removed, []string{orphan}) || len(res.Leftovers) != 0 {
		t.Fatalf("sweep: %+v %v", res, err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan job dir not removed")
	}
	for _, keep := range []string{young, live, starting, outside, filepath.Join(jobs, "link"), filepath.Join(jobs, "file")} {
		if _, err := os.Lstat(keep); err != nil {
			t.Errorf("%s must stay: %v", keep, err)
		}
	}
	if n.claimed[orphan] || !n.claimed[starting] {
		t.Fatalf("claims after the sweep: %v", n.claimed)
	}
	if res, err := NewNative(config.NativeBackendConfig{Root: t.TempDir()}, nil).SweepJobDirs(time.Hour, false); err != nil || len(res.Removed) != 0 {
		t.Fatalf("no jobs dir yet: %+v %v", res, err)
	}
}

func TestNativeStartRefusesClaimedDir(t *testing.T) {
	cfg := fakeRunner(t, "exit 0\n")
	n := NewNative(cfg, nil)
	if err := n.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	n.claimed[filepath.Join(n.jobsDir(), "r1")] = true // the sweep holds it
	if _, err := n.Start(t.Context(), RunnerSpec{Name: "r1", JITConfig: "x"}); err == nil {
		t.Fatal("start into a claimed dir must fail")
	}
	h, err := n.Start(t.Context(), RunnerSpec{Name: "r2", JITConfig: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if n.claimed[h.ID] {
		t.Fatal("start must release its claim once the runner is live")
	}
	if _, err := n.Wait(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	if err := n.Stop(t.Context(), h); err != nil || n.claimed[h.ID] {
		t.Fatalf("stop: %v, claimed %v", err, n.claimed)
	}
	if _, err := os.Stat(h.ID); !os.IsNotExist(err) {
		t.Fatal("job dir (and its _work) must be gone after the job")
	}
}
