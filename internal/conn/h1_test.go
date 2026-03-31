package conn

import "testing"

func TestH1StateMaxBodySize(t *testing.T) {
	s := NewH1State()

	// Zero value = unlimited (0 passes through, limit > 0 guard disables enforcement)
	if got := s.maxBodySize(); got != 0 {
		t.Fatalf("expected 0 (unlimited), got %d", got)
	}

	// Custom value
	s.MaxRequestBodySize = 50 << 20
	if got := s.maxBodySize(); got != 50<<20 {
		t.Fatalf("expected 50MB, got %d", got)
	}
}
