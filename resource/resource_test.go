package resource

import "testing"

func TestResolveUserOverrides(t *testing.T) {
	r := Resources{
		Workers: 8,
	}
	res := r.Resolve()
	if res.Workers != 8 {
		t.Errorf("Workers override: got %d, want 8", res.Workers)
	}
}

func TestResolveAllOverrides(t *testing.T) {
	r := Resources{
		Workers:    16,
		BufferSize: 32768,
		MaxConns:   8192,
		SocketRecv: 131072,
		SocketSend: 131072,
	}
	res := r.Resolve()
	if res.Workers != 16 {
		t.Errorf("Workers = %d, want 16", res.Workers)
	}
	if res.BufferSize != 32768 {
		t.Errorf("BufferSize = %d, want 32768", res.BufferSize)
	}
	if res.MaxConns != 8192 {
		t.Errorf("MaxConns = %d, want 8192", res.MaxConns)
	}
	if res.SocketRecv != 131072 {
		t.Errorf("SocketRecv = %d, want 131072", res.SocketRecv)
	}
	if res.SocketSend != 131072 {
		t.Errorf("SocketSend = %d, want 131072", res.SocketSend)
	}
}

func TestResolveDefaultValues(t *testing.T) {
	r := Resources{}
	res := r.Resolve()
	if res.SQERingSize != 8192 {
		t.Errorf("SQERingSize = %d, want 8192", res.SQERingSize)
	}
	if res.BufferPool != 65536 {
		t.Errorf("BufferPool = %d, want 65536", res.BufferPool)
	}
	if res.BufferSize != 8192 {
		t.Errorf("BufferSize = %d, want 8192", res.BufferSize)
	}
	if res.MaxEvents != 8192 {
		t.Errorf("MaxEvents = %d, want 8192", res.MaxEvents)
	}
	if res.MaxConns != 65536 {
		t.Errorf("MaxConns = %d, want 65536", res.MaxConns)
	}
	if res.SocketRecv != 0 {
		t.Errorf("SocketRecv = %d, want 0 (kernel auto-tune)", res.SocketRecv)
	}
	if res.SocketSend != 0 {
		t.Errorf("SocketSend = %d, want 0 (kernel auto-tune)", res.SocketSend)
	}
}

func TestResolveWorkersMinCap(t *testing.T) {
	r := Resources{}
	res := r.Resolve()
	if res.Workers < MinWorkers {
		t.Errorf("Workers = %d, want >= %d", res.Workers, MinWorkers)
	}
}

func TestResolveBufferSizeMinCap(t *testing.T) {
	r := Resources{
		BufferSize: 1024,
	}
	res := r.Resolve()
	if res.BufferSize != MinBufferSize {
		t.Errorf("BufferSize = %d, want %d (clamped to min)", res.BufferSize, MinBufferSize)
	}
}

func TestResolveBufferSizeMaxCap(t *testing.T) {
	r := Resources{
		BufferSize: 1 << 20,
	}
	res := r.Resolve()
	if res.BufferSize != MaxBufferSize {
		t.Errorf("BufferSize = %d, want %d (clamped to max)", res.BufferSize, MaxBufferSize)
	}
}

func TestClamp(t *testing.T) {
	tests := []struct {
		v, minVal, maxVal, want int
	}{
		{5, 2, 10, 5},
		{1, 2, 10, 2},
		{15, 2, 10, 10},
		{5, 0, 0, 5},
		{5, 2, 0, 5},
		{1, 2, 0, 2},
		{5, 0, 3, 3},
	}
	for _, tt := range tests {
		got := clamp(tt.v, tt.minVal, tt.maxVal)
		if got != tt.want {
			t.Errorf("clamp(%d, %d, %d) = %d, want %d", tt.v, tt.minVal, tt.maxVal, got, tt.want)
		}
	}
}

func TestNextPowerOf2(t *testing.T) {
	tests := []struct {
		input, want int
	}{
		{1000, 1024},
		{2048, 2048},
		{3, 4},
		{1, 1},
		{0, 0},
		{-1, -1},
		{5, 8},
		{16, 16},
		{17, 32},
		{4096, 4096},
		{4097, 8192},
		{32768, 32768},
	}
	for _, tt := range tests {
		got := nextPowerOf2(tt.input)
		if got != tt.want {
			t.Errorf("nextPowerOf2(%d) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestConfigMaxRequestBodySizeDefaults(t *testing.T) {
	// 0 → default 100 MB
	c := Config{}.WithDefaults()
	if c.MaxRequestBodySize != 100<<20 {
		t.Fatalf("expected 100MB default, got %d", c.MaxRequestBodySize)
	}

	// Positive value preserved
	c = Config{MaxRequestBodySize: 50 << 20}.WithDefaults()
	if c.MaxRequestBodySize != 50<<20 {
		t.Fatalf("expected 50MB, got %d", c.MaxRequestBodySize)
	}

	// -1 → unlimited (0)
	c = Config{MaxRequestBodySize: -1}.WithDefaults()
	if c.MaxRequestBodySize != 0 {
		t.Fatalf("expected 0 (unlimited), got %d", c.MaxRequestBodySize)
	}
}
