//go:build !linux

package cpumon

import (
	"sync"
	"time"
)

// RuntimeMon is the non-Linux fallback monitor.
//
// There is no portable system-wide CPU-utilization counter outside of Linux,
// and the only adaptive engine that consumes this signal is Linux-only. Rather
// than report a misleading per-process user-CPU proxy (which is NOT system
// utilization and would feed a bogus io_uring bias on a path that never runs),
// it reports zero utilization — i.e. "no CPU bias on non-Linux". The struct
// retains a mutex and closed flag so it satisfies the same concurrency and
// lifecycle contract as ProcStat.
type RuntimeMon struct {
	mu     sync.Mutex
	closed bool
}

// NewRuntimeMon creates a runtime-based CPU monitor for non-Linux platforms.
func NewRuntimeMon() *RuntimeMon {
	return &RuntimeMon{}
}

// Sample returns zero utilization on non-Linux platforms (no system-wide CPU
// signal available), making "no CPU bias off Linux" explicit.
func (m *RuntimeMon) Sample() (CPUSample, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return CPUSample{}, ErrClosed
	}
	return CPUSample{
		Utilization: 0,
		Timestamp:   time.Now(),
	}, nil
}

// Close marks the monitor closed. Idempotent; holds no OS resources.
func (m *RuntimeMon) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}
