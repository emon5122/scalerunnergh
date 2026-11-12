package pool

import (
	"sync"
	"testing"
)

func TestPoolLimits(t *testing.T) {
	p := New(3, 1, nil)
	h1, ok, _ := p.TryAcquire(true)
	if !ok {
		t.Fatal("first heavy must fit")
	}
	if _, ok, reason := p.TryAcquire(true); ok || reason != "host at max_heavy" {
		t.Fatalf("second heavy must be refused, got ok=%v reason=%q", ok, reason)
	}
	if p.Limit(true) != 1 || p.Limit(false) != 3 || New(4, 0, nil).Limit(true) != 4 {
		t.Fatal("Limit must be the fixed host limit")
	}
	if p.Free(true) != 0 || p.Free(false) != 2 {
		t.Fatalf("free: heavy %d light %d", p.Free(true), p.Free(false))
	}
	l1, _, _ := p.TryAcquire(false)
	l2, _, _ := p.TryAcquire(false)
	if _, ok, reason := p.TryAcquire(false); ok || reason != "host at max_runners" {
		t.Fatalf("pool must be full, got %v %q", ok, reason)
	}
	h1.Release()
	h1.Release() // double release is a no-op
	if used, heavy := p.Used(); used != 2 || heavy != 0 {
		t.Fatalf("used %d heavy %d", used, heavy)
	}
	l1.Release()
	l2.Release()
	if used, _ := p.Used(); used != 0 {
		t.Fatalf("used %d", used)
	}
}

func TestPoolGateAndPause(t *testing.T) {
	open := false
	p := New(2, 0, func() (bool, string) { return open, "load high" })
	if _, ok, reason := p.TryAcquire(false); ok || reason != "load high" {
		t.Fatalf("gate must block: %v %q", ok, reason)
	}
	if used, _ := p.Used(); used != 0 {
		t.Fatal("a blocked acquire must not leak a slot")
	}
	open = true
	sub := p.Subscribe()
	p.SetPaused(true)
	<-sub
	if _, ok, _ := p.TryAcquire(false); ok || p.Free(false) != 0 || !p.Paused() {
		t.Fatal("paused pool must not hand out slots")
	}
	p.SetPaused(false)
	<-sub
	s, ok, _ := p.TryAcquire(false)
	if !ok {
		t.Fatal("resumed pool must hand out slots")
	}
	s.Release()
	<-sub // release notifies
}

func TestPoolConcurrent(t *testing.T) {
	p := New(4, 2, nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	peak, peakHeavy := 0, 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(heavy bool) {
			defer wg.Done()
			s, ok, _ := p.TryAcquire(heavy)
			if !ok {
				return
			}
			u, h := p.Used()
			mu.Lock()
			peak, peakHeavy = max(peak, u), max(peakHeavy, h)
			mu.Unlock()
			s.Release()
		}(i%2 == 0)
	}
	wg.Wait()
	if peak > 4 || peakHeavy > 2 {
		t.Fatalf("limits exceeded: peak %d heavy %d", peak, peakHeavy)
	}
	if u, h := p.Used(); u != 0 || h != 0 {
		t.Fatalf("leak: %d %d", u, h)
	}
}
