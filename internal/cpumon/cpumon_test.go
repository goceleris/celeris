package cpumon

import (
	"testing"
)

func TestSyntheticMonitor(t *testing.T) {
	m := NewSynthetic(0.5)

	s, err := m.Sample()
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if s.Utilization != 0.5 {
		t.Errorf("got %f, want 0.5", s.Utilization)
	}

	m.Set(0.95)
	s, err = m.Sample()
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if s.Utilization != 0.95 {
		t.Errorf("got %f, want 0.95", s.Utilization)
	}
}

func TestSyntheticImplementsMonitor(_ *testing.T) {
	var _ Monitor = NewSynthetic(0)
}
