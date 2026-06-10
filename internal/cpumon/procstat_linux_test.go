//go:build linux

package cpumon

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// newProcStatFromFile builds a ProcStat backed by an arbitrary file so tests
// can drive the parser/delta logic without depending on the host /proc/stat.
func newProcStatFromFile(t *testing.T, path string) *ProcStat {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return &ProcStat{file: f}
}

func writeStat(t *testing.T, path, line string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatalf("write stat: %v", err)
	}
}

// TestProcStatDecreasingIdleClampsToZero feeds a decreasing idle counter (as a
// CPU hotplug reset or garbled read would produce) and asserts the unguarded
// uint64 subtraction no longer underflows to a nonsense utilization.
func TestProcStatDecreasingIdleClampsToZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stat")

	// Prime: idle=1000, total=2000.
	writeStat(t, path, "cpu  500 0 500 1000 0 0 0 0\n")
	p := newProcStatFromFile(t, path)
	if _, err := p.Sample(); err != nil {
		t.Fatalf("prime Sample: %v", err)
	}

	// Now idle DECREASES (counter regression). Total also regresses.
	writeStat(t, path, "cpu  100 0 100 200 0 0 0 0\n")
	s, err := p.Sample()
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if s.Utilization != 0 {
		t.Errorf("decreasing counters: util = %v, want 0 (no underflow)", s.Utilization)
	}
}

// TestProcStatUtilizationInRange sanity-checks a normal forward delta and the
// [0,1] clamp.
func TestProcStatUtilizationInRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stat")

	// Prime: idle=1000, total=2000.
	writeStat(t, path, "cpu  500 0 500 1000 0 0 0 0\n")
	p := newProcStatFromFile(t, path)
	if _, err := p.Sample(); err != nil {
		t.Fatalf("prime Sample: %v", err)
	}

	// Forward: idle=1100 (+100), total=2400 (+400) → util = 1 - 100/400 = 0.75.
	writeStat(t, path, "cpu  700 0 600 1100 0 0 0 0\n")
	s, err := p.Sample()
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if s.Utilization < 0 || s.Utilization > 1 {
		t.Fatalf("util %v out of [0,1]", s.Utilization)
	}
	if d := s.Utilization - 0.75; d > 1e-9 || d < -1e-9 {
		t.Errorf("util = %v, want 0.75", s.Utilization)
	}
}

// TestProcStatCloseIdempotentAndSentinel verifies Close is idempotent and that
// Sample after Close returns ErrClosed instead of touching a (possibly reused)
// fd.
func TestProcStatCloseIdempotentAndSentinel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stat")
	writeStat(t, path, "cpu  500 0 500 1000 0 0 0 0\n")
	p := newProcStatFromFile(t, path)

	if err := p.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close (must be idempotent): %v", err)
	}
	if _, err := p.Sample(); err != ErrClosed {
		t.Fatalf("Sample after Close: err = %v, want ErrClosed", err)
	}
}

// TestProcStatConcurrentSample spins two goroutines hammering Sample on a single
// ProcStat — the production wiring (live sampler + observe collector share one
// instance). Run under -race to catch unsynchronized access to the shared file
// handle and delta accumulators.
func TestProcStatConcurrentSample(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stat")
	writeStat(t, path, "cpu  500 0 500 1000 0 0 0 0\n")
	p := newProcStatFromFile(t, path)
	if _, err := p.Sample(); err != nil {
		t.Fatalf("prime Sample: %v", err)
	}

	var wg sync.WaitGroup
	for g := range 2 {
		wg.Add(1)
		go func(_ int) {
			defer wg.Done()
			for range 500 {
				if _, err := p.Sample(); err != nil {
					t.Errorf("concurrent Sample: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
