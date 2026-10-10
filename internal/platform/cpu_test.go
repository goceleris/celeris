package platform

import (
	"runtime"
	"testing"
)

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
