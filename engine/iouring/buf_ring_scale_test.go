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
		{name: "default target falls through to 20", workers: 4, targetConns: 0, want: 256}, // 2*4*20=160, nextPow2=256
		{name: "explicit 20", workers: 4, targetConns: 20, want: 256},
		{name: "8 workers * 20 target = 320 → 512", workers: 8, targetConns: 20, want: 512},
		{name: "16 workers * 20 = 640 → 1024", workers: 16, targetConns: 20, want: 1024},
		{name: "32 workers * 20 = 1280 → 2048", workers: 32, targetConns: 20, want: 2048},
		{name: "below floor", workers: 1, targetConns: 1, want: bufRingCountMin},
		{name: "capped at max", workers: 1024, targetConns: 1024, want: bufRingCountMax},
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
