package conn

import "testing"

func TestH1StateMaxBodySize(t *testing.T) {
	s := NewH1State()

	// Default: 100 MB
	if got := s.maxBodySize(); got != defaultMaxRequestBodySize {
		t.Fatalf("expected default %d, got %d", defaultMaxRequestBodySize, got)
	}

	// Custom value
	s.MaxRequestBodySize = 50 << 20
	if got := s.maxBodySize(); got != 50<<20 {
		t.Fatalf("expected 50MB, got %d", got)
	}
}
