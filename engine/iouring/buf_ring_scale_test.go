//go:build linux

package iouring

import (
	"os"
	"testing"

	"github.com/goceleris/celeris/resource"
)

func TestResolveBufRingCountDefaults(t *testing.T) {
	tests := []struct {
		name        string
		workers     int
		targetConns int
		want        int
	}{
		// Formula: 2 * connsPerWorker, rounded up to a power of 2,
		// clamped to [bufRingCountMin=1024, bufRingCountMax]. The ring is
		// PER-WORKER, so the result MUST NOT depend on Workers (celeris#322
		// follow-up — the prior 2*Workers*target over-sized every worker's
		// ring by the worker count).
		{name: "default target falls through to 20; 2*20=40→64→floor 1024", workers: 4, targetConns: 0, want: 1024},
		{name: "explicit 20; same as above", workers: 4, targetConns: 20, want: 1024},
		{name: "more workers must not change result; 2*20→floor 1024", workers: 64, targetConns: 20, want: 1024},
		{name: "target 600 → 2*600=1200 → 2048", workers: 4, targetConns: 600, want: 2048},
		{name: "below floor", workers: 1, targetConns: 1, want: bufRingCountMin},
		{name: "capped at max", workers: 1, targetConns: 1 << 20, want: bufRingCountMax},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := resource.ResolvedResources{Workers: tt.workers, BufferSize: 8192}
			got := resolveBufRingCount(res, tt.targetConns)
			if got != tt.want {
				t.Errorf("resolveBufRingCount(workers=%d, target=%d) = %d, want %d",
					tt.workers, tt.targetConns, got, tt.want)
			}
		})
	}
}

// TestResolveBufRingCountIndependentOfWorkers is the regression guard for
// celeris#322 follow-up: the provided-buffer ring is created once PER WORKER,
// so its size must depend only on the per-worker conn target, never on the
// engine-wide Workers count. Before the fix, 2*Workers*target made a 64-worker
// box allocate a 64× larger ring on every worker.
func TestResolveBufRingCountIndependentOfWorkers(t *testing.T) {
	const target = 20
	got1 := resolveBufRingCount(resource.ResolvedResources{Workers: 1, BufferSize: 8192}, target)
	got64 := resolveBufRingCount(resource.ResolvedResources{Workers: 64, BufferSize: 8192}, target)
	if got1 != got64 {
		t.Fatalf("ring size depends on Workers: workers=1→%d, workers=64→%d (must be equal)", got1, got64)
	}
}

func TestResolveBufRingCountEnvOverride(t *testing.T) {
	t.Setenv(envPbufCount, "4096")
	res := resource.ResolvedResources{Workers: 4, BufferSize: 8192}
	got := resolveBufRingCount(res, 20)
	if got != 4096 {
		t.Errorf("env override: got %d, want 4096", got)
	}
}

func TestResolveBufRingCountEnvOverrideRoundsUp(t *testing.T) {
	t.Setenv(envPbufCount, "3000") // not a power of 2
	res := resource.ResolvedResources{Workers: 4, BufferSize: 8192}
	got := resolveBufRingCount(res, 20)
	if got != 4096 {
		t.Errorf("non-pow2 env: got %d, want 4096", got)
	}
}

func TestResolveBufRingCountEnvOverrideBelowFloor(t *testing.T) {
	t.Setenv(envPbufCount, "256") // below the 1024 floor
	res := resource.ResolvedResources{Workers: 4, BufferSize: 8192}
	got := resolveBufRingCount(res, 20)
	if got != bufRingCountMin {
		t.Errorf("below-floor env: got %d, want %d (clamped to floor)", got, bufRingCountMin)
	}
}

func TestResolveBufRingCountEnvOverrideInvalid(t *testing.T) {
	t.Setenv(envPbufCount, "not-a-number")
	res := resource.ResolvedResources{Workers: 4, BufferSize: 8192}
	// Invalid env value falls through to auto-scaling.
	got := resolveBufRingCount(res, 20)
	if got == 0 {
		t.Errorf("invalid env should not return 0")
	}
}

func TestResolveBufRingCountIsPowerOf2(t *testing.T) {
	// Sanity: every value out of resolveBufRingCount must be a power of 2
	// (kernel requirement of RegisterPbufRing).
	for workers := 1; workers <= 64; workers *= 2 {
		for target := 1; target <= 100; target *= 5 {
			res := resource.ResolvedResources{Workers: workers, BufferSize: 8192}
			got := resolveBufRingCount(res, target)
			if got&(got-1) != 0 {
				t.Errorf("non-pow2: workers=%d target=%d got=%d", workers, target, got)
			}
			if got < bufRingCountMin {
				t.Errorf("below floor: workers=%d target=%d got=%d", workers, target, got)
			}
			if got > bufRingCountMax {
				t.Errorf("above max: workers=%d target=%d got=%d", workers, target, got)
			}
		}
	}
}

func TestResolveBufRingCountEnvZeroRevertsToAuto(t *testing.T) {
	// 0 or empty env reverts to auto-scaling.
	t.Setenv(envPbufCount, "")
	if v := os.Getenv(envPbufCount); v != "" {
		t.Fatalf("setup: env var not cleared")
	}
	res := resource.ResolvedResources{Workers: 4, BufferSize: 8192}
	got := resolveBufRingCount(res, 20)
	if got == 0 {
		t.Errorf("zero env should fall through to auto-scaling, got 0")
	}
}
