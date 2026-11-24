package startwatch

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func newAt(t *testing.T, now *time.Time) *Watch {
	w := New(t.TempDir())
	w.now = func() time.Time { return *now }
	return w
}

func TestSpikeAfterUncleanStarts(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	w := newAt(t, &now)
	for i := 0; i < 5; i++ {
		if err := w.Record(); err != nil {
			t.Fatal(err)
		}
		_ = w.Exit(errors.New("cleanup native backend: permission denied"))
		now = now.Add(10 * time.Second)
	}
	sp, err := w.Evaluate(time.Hour, 5)
	if err != nil || sp.Spike || sp.Starts != 5 {
		t.Fatalf("5 starts are not a spike: %+v %v", sp, err)
	}
	if err := w.Record(); err != nil {
		t.Fatal(err)
	}
	sp, _ = w.Evaluate(time.Hour, 5)
	if !sp.Spike || !sp.AlertDue || sp.Starts != 6 || sp.LastError != "cleanup native backend: permission denied" {
		t.Fatalf("6th start: %+v", sp)
	}
	if err := w.MarkAlerted(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	_ = w.Record()
	if sp, _ = w.Evaluate(time.Hour, 5); !sp.Spike || sp.AlertDue {
		t.Fatalf("one alert per window: %+v", sp)
	}
	now = now.Add(61 * time.Minute)
	_ = w.Record()
	if sp, _ = w.Evaluate(time.Hour, 5); sp.Spike || sp.Starts != 1 {
		t.Fatalf("old starts leave the window: %+v", sp)
	}
}

func TestCleanExitClearsStarts(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	w := newAt(t, &now)
	for i := 0; i < 20; i++ {
		_ = w.Record()
		if err := w.Exit(nil); err != nil {
			t.Fatal(err)
		}
	}
	_ = w.Record()
	if sp, _ := w.Evaluate(time.Hour, 5); sp.Spike || sp.Starts != 1 {
		t.Fatalf("planned restarts are no spike: %+v", sp)
	}
}

func TestBrokenFileIsReset(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	w := newAt(t, &now)
	if err := os.WriteFile(w.Path(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.Record(); err == nil {
		t.Fatal("a broken file is reported")
	}
	if sp, err := w.Evaluate(time.Hour, 5); err != nil || sp.Starts != 1 {
		t.Fatalf("after reset: %+v %v", sp, err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(w.Path()), "*.tmp-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestAlertEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	out, err := Alert(t.Context(), []string{"sh", "-c", `echo "$SRG_ALERT $SRG_HOST $SRG_STARTS $SRG_WINDOW $SRG_LAST_ERROR"`}, "pc09",
		Spike{Starts: 7, Window: time.Hour, LastError: "boom"})
	if err != nil || out != "restart_spike pc09 7 1h0m0s boom\n" {
		t.Fatalf("alert: %q %v", out, err)
	}
	if _, err := Alert(t.Context(), []string{"sh", "-c", "exit 3"}, "h", Spike{}); err == nil {
		t.Fatal("a failing command is an error")
	}
}
