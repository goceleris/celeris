package platform

import "testing"

func TestDistributeWorkers(t *testing.T) {
	cpus := DistributeWorkers(4, 8, 2)
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

func TestPinToCPU(_ *testing.T) {
	// On non-linux this is a no-op. On linux it may fail without root but shouldn't panic.
	_ = PinToCPU(0)
}
