package platform

import (
	"runtime"
	"testing"
)

func TestDistributeWorkersSingleSocket(t *testing.T) {
	// numaNodes <= 1 should use simple round-robin.
	cpus := DistributeWorkers(4, 8, 1)
	if len(cpus) != 4 {
		t.Fatalf("expected 4 cpus, got %d", len(cpus))
	}
	for i, cpu := range cpus {
		if cpu != i%8 {
			t.Errorf("worker %d: expected cpu %d, got %d", i, i%8, cpu)
		}
	}
}

func TestDistributeWorkersMoreThanCPUs(t *testing.T) {
	cpus := DistributeWorkers(6, 4, 1)
	expected := []int{0, 1, 2, 3, 0, 1}
	for i, cpu := range cpus {
		if cpu != expected[i] {
			t.Errorf("worker %d: expected cpu %d, got %d", i, expected[i], cpu)
		}
	}
}

func TestDistributeWorkersMultiSocket(t *testing.T) {
	// With numaNodes > 1 but no sysfs available (non-Linux or missing sysfs),
	// should fall back to round-robin.
	cpus := DistributeWorkers(4, 96, 2)
	if len(cpus) != 4 {
		t.Fatalf("expected 4 cpus, got %d", len(cpus))
	}
	// On non-Linux or when sysfs is unavailable, falls back to round-robin.
	// On Linux with sysfs, would interleave across nodes.
}

func TestParseCPUList(t *testing.T) {
	tests := []struct {
		input    string
		expected []int
	}{
		{"0-3", []int{0, 1, 2, 3}},
		{"0-3,8-11", []int{0, 1, 2, 3, 8, 9, 10, 11}},
		{"0,2,4,6", []int{0, 2, 4, 6}},
		{"0-1,4-5", []int{0, 1, 4, 5}},
		{"", nil},
		{"0", []int{0}},
	}
	for _, tt := range tests {
		got := parseCPUList(tt.input)
		if len(got) != len(tt.expected) {
			t.Errorf("parseCPUList(%q): got %v, want %v", tt.input, got, tt.expected)
			continue
		}
		for i := range got {
			if got[i] != tt.expected[i] {
				t.Errorf("parseCPUList(%q)[%d]: got %d, want %d", tt.input, i, got[i], tt.expected[i])
			}
		}
	}
}

func TestPinToCPU(t *testing.T) {
	// On non-linux this is a no-op. On linux it may fail without root but shouldn't panic.
	//
	// The pin belongs to the OS thread, not the goroutine (celeris#905): hold
	// the thread for the whole test, put its mask back, and never unlock it.
	// The test goroutine exits locked, so the runtime ends its thread even if
	// Restore failed, and no other test in this binary runs on a thread left
	// pinned to CPU 0.
	runtime.LockOSThread()
	prev, err := SaveThreadAffinity()
	if err != nil {
		t.Fatalf("SaveThreadAffinity: %v", err)
	}
	defer func() {
		if err := prev.Restore(); err != nil {
			t.Errorf("Restore: %v", err)
		}
	}()
	_ = PinToCPU(0)
}
