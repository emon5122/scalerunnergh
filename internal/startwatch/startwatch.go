// Package startwatch notices a crash loop: every start of the service is recorded in a small state file, a clean
// exit clears the record, and a start that finds more than max_starts unclean starts within the window is a restart
// spike. A service manager restarts a failing service forever (systemd Restart=always), and a process that dies
// within seconds of every start is never seen by a scraper of its own metrics, so the spike is detected at startup
// and reported from there: the log, gauges, /health and an optional alert command.
package startwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// FileName is the state file in the state dir.
const FileName = "starts.json"

// keepFor bounds the start history; it is longer than any sensible window.
const (
	keepFor   = 7 * 24 * time.Hour
	keepCount = 10000
)

// AlertTimeout bounds one alert command run, so a hung command cannot hold up every start of a loop.
const AlertTimeout = 10 * time.Second

type state struct {
	// Starts are the unix times of the starts since the last clean exit.
	Starts      []int64 `json:"starts"`
	LastError   string  `json:"last_error,omitempty"`
	LastErrorAt int64   `json:"last_error_at,omitempty"`
	LastAlertAt int64   `json:"last_alert_at,omitempty"`
}

// Watch is the start record of one service instance.
type Watch struct {
	path string
	now  func() time.Time
}

// New returns the watch on dir/starts.json.
func New(dir string) *Watch {
	return &Watch{path: filepath.Join(dir, FileName), now: time.Now}
}

// Path is the state file.
func (w *Watch) Path() string { return w.path }

// Spike is what Evaluate found.
type Spike struct {
	Starts    int           // unclean starts within the window, this one included
	Window    time.Duration // the window
	Spike     bool          // Starts > max_starts
	AlertDue  bool          // a spike, and no alert was sent within the window
	LastError string        // the last recorded exit error (may be from before the window)
}

// Record notes one start. Call it as early as possible, before anything that can fail.
func (w *Watch) Record() error {
	st, err := w.load()
	if err != nil {
		st = state{} // a broken file must not stop the watch
	}
	now := w.now()
	cutoff := now.Add(-keepFor).Unix()
	kept := st.Starts[:0]
	for _, t := range st.Starts {
		if t >= cutoff {
			kept = append(kept, t)
		}
	}
	st.Starts = append(kept, now.Unix())
	if len(st.Starts) > keepCount {
		st.Starts = st.Starts[len(st.Starts)-keepCount:]
	}
	if serr := w.save(st); serr != nil {
		return serr
	}
	return err
}

// Evaluate counts the recorded starts within window against maxStarts.
func (w *Watch) Evaluate(window time.Duration, maxStarts int) (Spike, error) {
	st, err := w.load()
	if err != nil {
		return Spike{}, err
	}
	now := w.now()
	sp := Spike{Window: window, LastError: st.LastError}
	cutoff := now.Add(-window).Unix()
	for _, t := range st.Starts {
		if t >= cutoff {
			sp.Starts++
		}
	}
	sp.Spike = sp.Starts > maxStarts
	sp.AlertDue = sp.Spike && (st.LastAlertAt == 0 || now.Unix()-st.LastAlertAt >= int64(window/time.Second))
	return sp, nil
}

// MarkAlerted notes that the alert for the current spike went out.
func (w *Watch) MarkAlerted() error {
	st, err := w.load()
	if err != nil {
		return err
	}
	st.LastAlertAt = w.now().Unix()
	return w.save(st)
}

// Exit records how the process ends. A clean exit (nil) clears the start history: planned restarts (deploys,
// service stops) never add up to a spike. An error is kept as the last error for the next start's report.
func (w *Watch) Exit(exitErr error) error {
	st, err := w.load()
	if err != nil {
		st = state{}
	}
	if exitErr == nil {
		st.Starts = nil
	} else {
		msg := exitErr.Error()
		if len(msg) > 2000 {
			msg = msg[:2000]
		}
		st.LastError, st.LastErrorAt = msg, w.now().Unix()
	}
	return w.save(st)
}

func (w *Watch) load() (state, error) {
	var st state
	b, err := os.ReadFile(w.path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return state{}, fmt.Errorf("startwatch: parse %s: %w", w.path, err)
	}
	return st, nil
}

// save writes the file atomically (temp file + rename), so a crash mid-write never leaves half a file.
func (w *Watch) save(st state) error {
	if err := os.MkdirAll(filepath.Dir(w.path), 0o750); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(w.path), FileName+".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), w.path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Alert runs command (argv, no shell) with the spike in its environment: SRG_ALERT=restart_spike, SRG_HOST,
// SRG_STARTS, SRG_WINDOW, SRG_LAST_ERROR. Its output is returned for the log.
func Alert(ctx context.Context, command []string, host string, sp Spike) (string, error) {
	if len(command) == 0 {
		return "", errors.New("startwatch: empty alert command")
	}
	ctx, cancel := context.WithTimeout(ctx, AlertTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Env = append(os.Environ(),
		"SRG_ALERT=restart_spike",
		"SRG_HOST="+host,
		"SRG_STARTS="+strconv.Itoa(sp.Starts),
		"SRG_WINDOW="+sp.Window.String(),
		"SRG_LAST_ERROR="+sp.LastError,
	)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if len(out) > 1000 {
		out = out[:1000]
	}
	return string(out), err
}
