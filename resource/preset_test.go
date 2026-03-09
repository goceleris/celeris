package resource

import "testing"

func TestResolvePresetGreedy(t *testing.T) {
	cpus := []int{1, 4, 8, 16, 32}
	for _, n := range cpus {
		res := resolvePreset(Greedy, n)
		if res.Workers != n {
			t.Errorf("Greedy(cpu=%d): Workers = %d, want %d", n, res.Workers, n)
		}
		if res.SQERingSize != 32768 {
			t.Errorf("Greedy(cpu=%d): SQERingSize = %d, want 32768", n, res.SQERingSize)
		}
		if res.BufferPool != 65536 {
			t.Errorf("Greedy(cpu=%d): BufferPool = %d, want 65536", n, res.BufferPool)
		}
		if res.BufferSize != 65536 {
			t.Errorf("Greedy(cpu=%d): BufferSize = %d, want 65536", n, res.BufferSize)
		}
		if res.MaxEvents != 8192 {
			t.Errorf("Greedy(cpu=%d): MaxEvents = %d, want 8192", n, res.MaxEvents)
		}
		if res.MaxConns != 65536 {
			t.Errorf("Greedy(cpu=%d): MaxConns = %d, want 65536", n, res.MaxConns)
		}
		if res.SocketRecv != 262144 {
			t.Errorf("Greedy(cpu=%d): SocketRecv = %d, want 262144", n, res.SocketRecv)
		}
		if res.SocketSend != 262144 {
			t.Errorf("Greedy(cpu=%d): SocketSend = %d, want 262144", n, res.SocketSend)
		}
	}
}

func TestResolvePresetBalanced(t *testing.T) {
	tests := []struct {
		numCPU     int
		sqeRing    int
		bufferPool int
		maxEvents  int
		maxConns   int
	}{
		{1, 2048, 512, 1024, 512},
		{4, 8192, 2048, 4096, 2048},
		{8, 16384, 4096, 8192, 4096},
		{16, 32768, 8192, 8192, 8192},
		{32, 32768, 16384, 8192, 16384},
	}
	for _, tt := range tests {
		res := resolvePreset(Balanced, tt.numCPU)
		if res.Workers != tt.numCPU {
			t.Errorf("Balanced(cpu=%d): Workers = %d, want %d", tt.numCPU, res.Workers, tt.numCPU)
		}
		if res.SQERingSize != tt.sqeRing {
			t.Errorf("Balanced(cpu=%d): SQERingSize = %d, want %d", tt.numCPU, res.SQERingSize, tt.sqeRing)
		}
		if res.BufferPool != tt.bufferPool {
			t.Errorf("Balanced(cpu=%d): BufferPool = %d, want %d", tt.numCPU, res.BufferPool, tt.bufferPool)
		}
		if res.BufferSize != 65536 {
			t.Errorf("Balanced(cpu=%d): BufferSize = %d, want 65536", tt.numCPU, res.BufferSize)
		}
		if res.MaxEvents != tt.maxEvents {
			t.Errorf("Balanced(cpu=%d): MaxEvents = %d, want %d", tt.numCPU, res.MaxEvents, tt.maxEvents)
		}
		if res.MaxConns != tt.maxConns {
			t.Errorf("Balanced(cpu=%d): MaxConns = %d, want %d", tt.numCPU, res.MaxConns, tt.maxConns)
		}
		if res.SocketRecv != 262144 {
			t.Errorf("Balanced(cpu=%d): SocketRecv = %d, want 262144", tt.numCPU, res.SocketRecv)
		}
		if res.SocketSend != 262144 {
			t.Errorf("Balanced(cpu=%d): SocketSend = %d, want 262144", tt.numCPU, res.SocketSend)
		}
	}
}

func TestResolvePresetMinimal(t *testing.T) {
	tests := []struct {
		numCPU  int
		workers int
	}{
		{1, MinWorkers},
		{4, 2},
		{8, 4},
		{16, 8},
		{32, 16},
	}
	for _, tt := range tests {
		res := resolvePreset(Minimal, tt.numCPU)
		if res.Workers != tt.workers {
			t.Errorf("Minimal(cpu=%d): Workers = %d, want %d", tt.numCPU, res.Workers, tt.workers)
		}
		if res.SQERingSize != 2048 {
			t.Errorf("Minimal(cpu=%d): SQERingSize = %d, want 2048", tt.numCPU, res.SQERingSize)
		}
		if res.BufferPool != 1024 {
			t.Errorf("Minimal(cpu=%d): BufferPool = %d, want 1024", tt.numCPU, res.BufferPool)
		}
		if res.BufferSize != 16384 {
			t.Errorf("Minimal(cpu=%d): BufferSize = %d, want 16384", tt.numCPU, res.BufferSize)
		}
		if res.MaxEvents != 1024 {
			t.Errorf("Minimal(cpu=%d): MaxEvents = %d, want 1024", tt.numCPU, res.MaxEvents)
		}
		if res.MaxConns != 1024 {
			t.Errorf("Minimal(cpu=%d): MaxConns = %d, want 1024", tt.numCPU, res.MaxConns)
		}
		if res.SocketRecv != 65536 {
			t.Errorf("Minimal(cpu=%d): SocketRecv = %d, want 65536", tt.numCPU, res.SocketRecv)
		}
		if res.SocketSend != 65536 {
			t.Errorf("Minimal(cpu=%d): SocketSend = %d, want 65536", tt.numCPU, res.SocketSend)
		}
	}
}

func TestResolvePresetZeroCPU(t *testing.T) {
	res := resolvePreset(Balanced, 0)
	if res.Workers < 1 {
		t.Errorf("numCPU=0: Workers = %d, want >= 1", res.Workers)
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

func TestResourcePresetString(t *testing.T) {
	tests := []struct {
		preset ResourcePreset
		want   string
	}{
		{Greedy, "greedy"},
		{Balanced, "balanced"},
		{Minimal, "minimal"},
		{ResourcePreset(255), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.preset.String(); got != tt.want {
			t.Errorf("ResourcePreset(%d).String() = %q, want %q", tt.preset, got, tt.want)
		}
	}
}
