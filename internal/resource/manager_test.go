package resource

import (
	"fmt"
	"sync"
	"testing"
)

func TestAllocateAndRelease(t *testing.T) {
	m := NewManager(3, 4, 8, 16384)

	if err := m.Allocate("r1", 1, 2, 4096); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.ActiveCount() != 1 {
		t.Errorf("expected 1 active, got %d", m.ActiveCount())
	}
	if m.AvailableGPUs() != 3 {
		t.Errorf("expected 3 available GPUs, got %d", m.AvailableGPUs())
	}

	if err := m.Allocate("r2", 2, 4, 8192); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.ActiveCount() != 2 {
		t.Errorf("expected 2 active, got %d", m.ActiveCount())
	}

	// Release first
	if err := m.Release("r1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.ActiveCount() != 1 {
		t.Errorf("expected 1 active after release, got %d", m.ActiveCount())
	}
	if m.AvailableGPUs() != 2 {
		t.Errorf("expected 2 available GPUs, got %d", m.AvailableGPUs())
	}
}

func TestAllocate_MaxRunners(t *testing.T) {
	m := NewManager(2, 0, 0, 0)

	if err := m.Allocate("r1", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Allocate("r2", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	err := m.Allocate("r3", 0, 0, 0)
	if err != ErrNoCapacity {
		t.Fatalf("expected ErrNoCapacity, got %v", err)
	}
}

func TestAllocate_GPULimit(t *testing.T) {
	m := NewManager(10, 2, 0, 0)

	if err := m.Allocate("r1", 2, 0, 0); err != nil {
		t.Fatal(err)
	}
	err := m.Allocate("r2", 1, 0, 0)
	if err != ErrNoCapacity {
		t.Fatalf("expected ErrNoCapacity for GPU overcommit, got %v", err)
	}
}

func TestAllocate_CPULimit(t *testing.T) {
	m := NewManager(10, 0, 4, 0)

	if err := m.Allocate("r1", 0, 3, 0); err != nil {
		t.Fatal(err)
	}
	err := m.Allocate("r2", 0, 2, 0)
	if err != ErrNoCapacity {
		t.Fatalf("expected ErrNoCapacity for CPU overcommit, got %v", err)
	}
}

func TestAllocate_MemLimit(t *testing.T) {
	m := NewManager(10, 0, 0, 8192)

	if err := m.Allocate("r1", 0, 0, 5000); err != nil {
		t.Fatal(err)
	}
	err := m.Allocate("r2", 0, 0, 4000)
	if err != ErrNoCapacity {
		t.Fatalf("expected ErrNoCapacity for mem overcommit, got %v", err)
	}
}

func TestAllocate_DuplicateID(t *testing.T) {
	m := NewManager(5, 0, 0, 0)

	if err := m.Allocate("r1", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	err := m.Allocate("r1", 0, 0, 0)
	if err == nil {
		t.Fatal("expected error for duplicate ID")
	}
}

func TestRelease_NotFound(t *testing.T) {
	m := NewManager(5, 0, 0, 0)

	err := m.Release("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown ID")
	}
}

func TestCanAllocate(t *testing.T) {
	m := NewManager(2, 2, 0, 0)

	if !m.CanAllocate(1, 0, 0) {
		t.Error("expected CanAllocate to return true")
	}
	m.Allocate("r1", 1, 0, 0)
	m.Allocate("r2", 1, 0, 0)

	if m.CanAllocate(0, 0, 0) {
		t.Error("expected CanAllocate false when max runners reached")
	}
}

func TestSnapshot(t *testing.T) {
	m := NewManager(5, 4, 8, 16384)
	m.Allocate("r1", 1, 2, 4096)
	m.Allocate("r2", 2, 3, 8192)

	snap := m.Snapshot()
	if snap.ActiveRunners != 2 {
		t.Errorf("expected 2 active, got %d", snap.ActiveRunners)
	}
	if snap.UsedGPUs != 3 {
		t.Errorf("expected 3 used GPUs, got %d", snap.UsedGPUs)
	}
	if snap.UsedCPUs != 5 {
		t.Errorf("expected 5 used CPUs, got %d", snap.UsedCPUs)
	}
	if snap.UsedMemMB != 12288 {
		t.Errorf("expected 12288 used mem, got %d", snap.UsedMemMB)
	}
	if len(snap.Allocations) != 2 {
		t.Errorf("expected 2 allocations, got %d", len(snap.Allocations))
	}
}

func TestConcurrentAccess(t *testing.T) {
	m := NewManager(100, 0, 0, 0)
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("r%d", i)
			if err := m.Allocate(id, 0, 0, 0); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if m.ActiveCount() != 50 {
		t.Errorf("expected 50 active, got %d", m.ActiveCount())
	}

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("r%d", i)
			if err := m.Release(id); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if m.ActiveCount() != 0 {
		t.Errorf("expected 0 active after release all, got %d", m.ActiveCount())
	}
}

func TestAvailableGPUs_Unlimited(t *testing.T) {
	m := NewManager(5, 0, 0, 0) // 0 means unlimited
	if m.AvailableGPUs() != 0 {
		t.Errorf("expected 0 for unlimited GPUs, got %d", m.AvailableGPUs())
	}
}
