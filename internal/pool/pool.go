// Package pool is the host-wide runner slot pool shared by every scale set on
// a host. It replaces the old pc-09 mkdir lock: a scale set may start a runner
// only while it holds a slot, heavy scale sets also need a heavy slot, and a
// gate (load, memory) or a pause (kill switch) can stop new starts at once.
package pool

import (
	"sync"
)

// Gate reports whether the host may start another runner right now.
type Gate func() (ok bool, reason string)

// Slot is one reserved runner place. Release it exactly once (extra calls are no-ops).
type Slot struct {
	heavy    bool
	pool     *Pool
	released bool
}

// Heavy reports whether the slot also holds a heavy place.
func (s *Slot) Heavy() bool { return s.heavy }

// Release returns the slot to the pool.
func (s *Slot) Release() {
	if s == nil {
		return
	}
	s.pool.release(s)
}

// Pool tracks host capacity.
type Pool struct {
	mu        sync.Mutex
	max       int
	maxHeavy  int // 0 = heavy runners only limited by max
	used      int
	usedHeavy int
	paused    bool
	gate      Gate
	subs      []chan struct{}
}

// New creates a pool with max slots; maxHeavy (0 = unlimited) caps heavy slots.
func New(max, maxHeavy int, gate Gate) *Pool {
	return &Pool{max: max, maxHeavy: maxHeavy, gate: gate}
}

// TryAcquire reserves a slot or returns ok=false with the reason.
func (p *Pool) TryAcquire(heavy bool) (*Slot, bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if free, reason := p.freeLocked(heavy); free <= 0 {
		return nil, false, reason
	}
	if p.gate != nil {
		if ok, reason := p.gate(); !ok {
			return nil, false, reason
		}
	}
	p.used++
	if heavy {
		p.usedHeavy++
	}
	return &Slot{heavy: heavy, pool: p}, true, ""
}

// Limit returns the fixed number of runners of this kind the host allows,
// whatever is in use or paused right now.
func (p *Pool) Limit(heavy bool) int {
	if heavy && p.maxHeavy > 0 && p.maxHeavy < p.max {
		return p.maxHeavy
	}
	return p.max
}

// Free returns how many more runners of this kind could start now, ignoring
// the gate (the gate is checked at start time).
func (p *Pool) Free(heavy bool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, _ := p.freeLocked(heavy)
	return n
}

func (p *Pool) freeLocked(heavy bool) (int, string) {
	if p.paused {
		return 0, "paused (kill switch)"
	}
	free := p.max - p.used
	if free <= 0 {
		return 0, "host at max_runners"
	}
	if heavy && p.maxHeavy > 0 {
		if hf := p.maxHeavy - p.usedHeavy; hf < free {
			free = hf
			if free <= 0 {
				return 0, "host at max_heavy"
			}
		}
	}
	return free, ""
}

func (p *Pool) release(s *Slot) {
	p.mu.Lock()
	if s.released {
		p.mu.Unlock()
		return
	}
	s.released = true
	p.used--
	if s.heavy {
		p.usedHeavy--
	}
	p.mu.Unlock()
	p.Notify()
}

// SetPaused pauses or resumes new starts. Resuming wakes subscribers.
func (p *Pool) SetPaused(paused bool) {
	p.mu.Lock()
	changed := p.paused != paused
	p.paused = paused
	p.mu.Unlock()
	if changed {
		p.Notify()
	}
}

// Paused reports whether the pool is paused.
func (p *Pool) Paused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}

// Used returns used and usedHeavy slots.
func (p *Pool) Used() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.used, p.usedHeavy
}

// Subscribe returns a channel that receives a signal whenever capacity may
// have changed (slot released, pause toggled, Notify called).
func (p *Pool) Subscribe() <-chan struct{} {
	ch := make(chan struct{}, 1)
	p.mu.Lock()
	p.subs = append(p.subs, ch)
	p.mu.Unlock()
	return ch
}

// Notify wakes every subscriber without blocking.
func (p *Pool) Notify() {
	p.mu.Lock()
	subs := append([]chan struct{}(nil), p.subs...)
	p.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
