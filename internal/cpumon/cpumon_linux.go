//go:build linux

package cpumon

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// ProcStat reads CPU utilization from /proc/stat.
//
// A single ProcStat is sampled by more than one goroutine in an adaptive setup
// (the live sampler and the observe collector share one instance), so all
// mutable state — the shared file handle, the delta accumulators, and the
// closed flag — is guarded by mu.
type ProcStat struct {
	mu        sync.Mutex
	file      *os.File
	prevIdle  uint64
	prevTotal uint64
	closed    bool
}

// NewProcStat creates a /proc/stat-based CPU monitor.
func NewProcStat() (*ProcStat, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return nil, fmt.Errorf("open /proc/stat: %w", err)
	}
	p := &ProcStat{file: f}
	// Take an initial reading to prime the deltas.
	if _, err := p.Sample(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return p, nil
}

// Sample reads /proc/stat and computes CPU utilization since the last call.
func (p *ProcStat) Sample() (CPUSample, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return CPUSample{}, ErrClosed
	}

	if _, err := p.file.Seek(0, 0); err != nil {
		return CPUSample{}, fmt.Errorf("seek /proc/stat: %w", err)
	}

	var buf [512]byte
	n, err := p.file.Read(buf[:])
	if err != nil {
		return CPUSample{}, fmt.Errorf("read /proc/stat: %w", err)
	}

	// Parse first line: "cpu  user nice system idle iowait irq softirq steal ..."
	line := buf[:n]
	i := 0
	// Skip "cpu" prefix and whitespace.
	for i < len(line) && line[i] != ' ' {
		i++
	}
	for i < len(line) && line[i] == ' ' {
		i++
	}

	var fields [8]uint64
	for f := range 8 {
		val := uint64(0)
		for i < len(line) && line[i] >= '0' && line[i] <= '9' {
			val = val*10 + uint64(line[i]-'0')
			i++
		}
		fields[f] = val
		for i < len(line) && line[i] == ' ' {
			i++
		}
		if i < len(line) && line[i] == '\n' {
			break
		}
	}

	// fields: user, nice, system, idle, iowait, irq, softirq, steal
	idle := fields[3] + fields[4] // idle + iowait
	var total uint64
	for _, v := range fields {
		total += v
	}

	// /proc/stat counters are monotonic in normal operation, but a CPU hotplug
	// reset or a short/garbled read can make a counter decrease. An unguarded
	// uint64 subtraction would underflow to ~2^64 and yield nonsense
	// utilization, so only take a delta when the new value did not regress.
	var deltaIdle, deltaTotal uint64
	if idle >= p.prevIdle {
		deltaIdle = idle - p.prevIdle
	}
	if total >= p.prevTotal {
		deltaTotal = total - p.prevTotal
	}
	p.prevIdle = idle
	p.prevTotal = total

	util := 0.0
	if deltaTotal > 0 && deltaIdle <= deltaTotal {
		util = 1.0 - float64(deltaIdle)/float64(deltaTotal)
	}
	// Clamp into [0,1] to absorb any residual measurement skew.
	if util < 0 {
		util = 0
	} else if util > 1 {
		util = 1
	}

	return CPUSample{
		Utilization: util,
		Timestamp:   time.Now(),
	}, nil
}

// Close releases the file handle. It is idempotent and safe to call
// concurrently with Sample; once closed, Sample returns ErrClosed instead of
// touching a file descriptor that may have been reused.
func (p *ProcStat) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return p.file.Close()
}
