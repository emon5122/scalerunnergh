package hostgate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeProc(t *testing.T, load, meminfo string) string {
	t.Helper()
	dir := t.TempDir()
	if load != "" {
		os.WriteFile(filepath.Join(dir, "loadavg"), []byte(load), 0o644)
	}
	if meminfo != "" {
		os.WriteFile(filepath.Join(dir, "meminfo"), []byte(meminfo), 0o644)
	}
	return dir
}

func TestLoadGate(t *testing.T) {
	mem := "MemTotal: 8000000 kB\nMemAvailable:    2048000 kB\n"
	cases := []struct {
		name   string
		g      LoadGate
		ok     bool
		reason string
	}{
		{"off", LoadGate{}, true, ""},
		{"load ok", LoadGate{MaxLoadPerCore: 1, Cores: 4, ProcRoot: writeProc(t, "3.50 1 1 1/1 1\n", mem)}, true, ""},
		{"load high", LoadGate{MaxLoadPerCore: 1, Cores: 4, ProcRoot: writeProc(t, "4.50 1 1 1/1 1\n", mem)}, false, "load1"},
		{"mem ok", LoadGate{MinMemAvailableMB: 1500, ProcRoot: writeProc(t, "0 0 0", mem)}, true, ""},
		{"mem low", LoadGate{MinMemAvailableMB: 3000, ProcRoot: writeProc(t, "0 0 0", mem)}, false, "MemAvailable"},
		{"garbage fails closed", LoadGate{MaxLoadPerCore: 1, ProcRoot: writeProc(t, "x y z", mem)}, false, "load gate"},
		{"no proc allows", LoadGate{MaxLoadPerCore: 1, MinMemAvailableMB: 1, ProcRoot: t.TempDir()}, true, ""},
	}
	for _, c := range cases {
		ok, reason := c.g.Check()
		if ok != c.ok || !strings.Contains(reason, c.reason) {
			t.Errorf("%s: got ok=%v reason=%q", c.name, ok, reason)
		}
	}
}

func TestKillSwitchFailsClosed(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plain-file")
	os.WriteFile(file, nil, 0o644)
	// A path below a regular file cannot be checked (ENOTDIR): treated as on.
	if !(KillSwitch{Path: filepath.Join(file, "STOP")}).Active() {
		t.Fatal("an unreadable kill switch must count as on")
	}
	if (KillSwitch{Path: filepath.Join(t.TempDir(), "STOP")}).Active() || (KillSwitch{}).Active() {
		t.Fatal("a missing file or no path means off")
	}
}

func TestKillSwitch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "STOP")
	var mu sync.Mutex
	var events []bool
	ks := KillSwitch{Path: path, Interval: 10 * time.Millisecond, OnChange: func(a bool) {
		mu.Lock()
		events = append(events, a)
		mu.Unlock()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ks.Run(ctx); close(done) }()
	wait := func(n int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			l := len(events)
			mu.Unlock()
			if l >= n {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %d events, have %v", n, events)
	}
	wait(1)
	os.WriteFile(path, nil, 0o644)
	wait(2)
	os.Remove(path)
	wait(3)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 || events[0] || !events[1] || events[2] {
		t.Fatalf("events: %v", events)
	}
}
