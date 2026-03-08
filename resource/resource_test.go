package resource

import "testing"

func TestResolveUserOverrides(t *testing.T) {
	r := Resources{
		Preset:  Balanced,
		Workers: 8,
	}
	res := r.Resolve(4)
	if res.Workers != 8 {
		t.Errorf("Workers override: got %d, want 8", res.Workers)
	}
}

func TestResolveAllOverrides(t *testing.T) {
	r := Resources{
		Preset:      Minimal,
		Workers:     16,
		SQERingSize: 4096,
		BufferPool:  2048,
		BufferSize:  32768,
		MaxEvents:   4096,
		MaxConns:    8192,
		SocketRecv:  131072,
		SocketSend:  131072,
	}
	res := r.Resolve(4)
	if res.Workers != 16 {
		t.Errorf("Workers = %d, want 16", res.Workers)
	}
	if res.SQERingSize != 4096 {
		t.Errorf("SQERingSize = %d, want 4096", res.SQERingSize)
	}
	if res.BufferPool != 2048 {
		t.Errorf("BufferPool = %d, want 2048", res.BufferPool)
	}
	if res.BufferSize != 32768 {
		t.Errorf("BufferSize = %d, want 32768", res.BufferSize)
	}
	if res.MaxEvents != 4096 {
		t.Errorf("MaxEvents = %d, want 4096", res.MaxEvents)
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

func TestResolveSQERingPowerOf2(t *testing.T) {
	r := Resources{
		Preset:      Balanced,
		SQERingSize: 1000,
	}
	res := r.Resolve(4)
	if res.SQERingSize != 1024 {
		t.Errorf("SQERingSize = %d, want 1024 (rounded from 1000)", res.SQERingSize)
	}
}

func TestResolveSQERingAlreadyPowerOf2(t *testing.T) {
	r := Resources{
		Preset:      Balanced,
		SQERingSize: 2048,
	}
	res := r.Resolve(4)
	if res.SQERingSize != 2048 {
		t.Errorf("SQERingSize = %d, want 2048", res.SQERingSize)
	}
}

func TestResolveSQERingHardCap(t *testing.T) {
	r := Resources{
		Preset:      Balanced,
		SQERingSize: 100000,
	}
	res := r.Resolve(4)
	if res.SQERingSize != MaxSQERing {
		t.Errorf("SQERingSize = %d, want %d (capped)", res.SQERingSize, MaxSQERing)
	}
}

func TestResolveWorkersMinCap(t *testing.T) {
	r := Resources{
		Preset: Balanced,
	}
	res := r.Resolve(1)
	if res.Workers < MinWorkers {
		t.Errorf("Workers = %d, want >= %d", res.Workers, MinWorkers)
	}
}

func TestResolveBufferSizeMinCap(t *testing.T) {
	r := Resources{
		Preset:     Balanced,
		BufferSize: 1024,
	}
	res := r.Resolve(4)
	if res.BufferSize != MinBufferSize {
		t.Errorf("BufferSize = %d, want %d (clamped to min)", res.BufferSize, MinBufferSize)
	}
}

func TestResolveBufferSizeMaxCap(t *testing.T) {
	r := Resources{
		Preset:     Balanced,
		BufferSize: 1 << 20,
	}
	res := r.Resolve(4)
	if res.BufferSize != MaxBufferSize {
		t.Errorf("BufferSize = %d, want %d (clamped to max)", res.BufferSize, MaxBufferSize)
	}
}

func TestResolveZeroOverridesUsePreset(t *testing.T) {
	r := Resources{Preset: Greedy}
	res := r.Resolve(8)
	preset := resolvePreset(Greedy, 8)

	if res.BufferPool != preset.BufferPool {
		t.Errorf("BufferPool = %d, want preset %d", res.BufferPool, preset.BufferPool)
	}
	if res.MaxEvents != preset.MaxEvents {
		t.Errorf("MaxEvents = %d, want preset %d", res.MaxEvents, preset.MaxEvents)
	}
	if res.MaxConns != preset.MaxConns {
		t.Errorf("MaxConns = %d, want preset %d", res.MaxConns, preset.MaxConns)
	}
	if res.SocketRecv != preset.SocketRecv {
		t.Errorf("SocketRecv = %d, want preset %d", res.SocketRecv, preset.SocketRecv)
	}
	if res.SocketSend != preset.SocketSend {
		t.Errorf("SocketSend = %d, want preset %d", res.SocketSend, preset.SocketSend)
	}
}
