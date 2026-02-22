// Package resource provides a thread-safe resource manager that tracks
// available GPUs, CPUs, memory, and runner slots on the host.
package resource

import (
	"errors"
	"fmt"
	"sync"
)

// ErrNoCapacity is returned when there are not enough resources to fulfill
// an allocation request.
var ErrNoCapacity = errors.New("resource: insufficient capacity")

// Allocation represents the resources reserved for a single runner.
type Allocation struct {
	ID    string // unique identifier (usually container ID)
	GPUs  int
	CPUs  int
	MemMB int
}

// Manager tracks resource usage and enforces limits.
type Manager struct {
	mu sync.Mutex

	maxRunners int
	maxGPUs    int
	maxCPUs    int
	maxMemMB   int

	activeRunners int
	usedGPUs      int
	usedCPUs      int
	usedMemMB     int

	allocations map[string]Allocation
}

// NewManager creates a resource manager with the given limits.
// A limit of 0 means unlimited.
func NewManager(maxRunners, maxGPUs, maxCPUs, maxMemMB int) *Manager {
	return &Manager{
		maxRunners:  maxRunners,
		maxGPUs:     maxGPUs,
		maxCPUs:     maxCPUs,
		maxMemMB:    maxMemMB,
		allocations: make(map[string]Allocation),
	}
}

// CanAllocate checks whether the given resource request can be satisfied
// without actually reserving anything.
func (m *Manager) CanAllocate(gpus, cpus, memMB int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.canAllocateLocked(gpus, cpus, memMB)
}

func (m *Manager) canAllocateLocked(gpus, cpus, memMB int) bool {
	if m.maxRunners > 0 && m.activeRunners >= m.maxRunners {
		return false
	}
	if m.maxGPUs > 0 && m.usedGPUs+gpus > m.maxGPUs {
		return false
	}
	if m.maxCPUs > 0 && m.usedCPUs+cpus > m.maxCPUs {
		return false
	}
	if m.maxMemMB > 0 && m.usedMemMB+memMB > m.maxMemMB {
		return false
	}
	return true
}

// Allocate reserves resources for a runner. If capacity is insufficient,
// ErrNoCapacity is returned.
func (m *Manager) Allocate(id string, gpus, cpus, memMB int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.allocations[id]; exists {
		return fmt.Errorf("resource: allocation %q already exists", id)
	}

	if !m.canAllocateLocked(gpus, cpus, memMB) {
		return ErrNoCapacity
	}

	alloc := Allocation{
		ID:    id,
		GPUs:  gpus,
		CPUs:  cpus,
		MemMB: memMB,
	}
	m.allocations[id] = alloc
	m.activeRunners++
	m.usedGPUs += gpus
	m.usedCPUs += cpus
	m.usedMemMB += memMB
	return nil
}

// Release frees resources held by the given allocation ID.
func (m *Manager) Release(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	alloc, exists := m.allocations[id]
	if !exists {
		return fmt.Errorf("resource: allocation %q not found", id)
	}

	m.activeRunners--
	m.usedGPUs -= alloc.GPUs
	m.usedCPUs -= alloc.CPUs
	m.usedMemMB -= alloc.MemMB
	delete(m.allocations, id)
	return nil
}

// Snapshot returns a point-in-time view of resource usage.
type Snapshot struct {
	MaxRunners    int
	ActiveRunners int
	MaxGPUs       int
	UsedGPUs      int
	MaxCPUs       int
	UsedCPUs      int
	MaxMemMB      int
	UsedMemMB     int
	Allocations   []Allocation
}

// Snapshot returns the current resource state.
func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	allocs := make([]Allocation, 0, len(m.allocations))
	for _, a := range m.allocations {
		allocs = append(allocs, a)
	}
	return Snapshot{
		MaxRunners:    m.maxRunners,
		ActiveRunners: m.activeRunners,
		MaxGPUs:       m.maxGPUs,
		UsedGPUs:      m.usedGPUs,
		MaxCPUs:       m.maxCPUs,
		UsedCPUs:      m.usedCPUs,
		MaxMemMB:      m.maxMemMB,
		UsedMemMB:     m.usedMemMB,
		Allocations:   allocs,
	}
}

// ActiveCount returns the number of currently active runners.
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeRunners
}

// AvailableGPUs returns the number of free GPU slots.
func (m *Manager) AvailableGPUs() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.maxGPUs <= 0 {
		return 0
	}
	return m.maxGPUs - m.usedGPUs
}
