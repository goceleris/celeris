package engine

import "testing"

func TestEngineTypeString(t *testing.T) {
	tests := []struct {
		et   EngineType
		want string
	}{
		{IOUring, "io_uring"},
		{Epoll, "epoll"},
		{Adaptive, "adaptive"},
		{Std, "std"},
		{EngineType(255), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.et.String(); got != tt.want {
			t.Errorf("EngineType(%d).String() = %q, want %q", tt.et, got, tt.want)
		}
	}
}
