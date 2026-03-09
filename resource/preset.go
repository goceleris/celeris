package resource

// Resource limit constants for validation and clamping.
const (
	MinWorkers    = 1
	MaxSQERing    = 65536
	MinBufferSize = 4096
	MaxBufferSize = 262144
)

func resolvePreset(preset ResourcePreset, numCPU int) ResolvedResources {
	if numCPU < 1 {
		numCPU = 1
	}
	switch preset {
	case Greedy:
		return ResolvedResources{
			Workers:     numCPU,
			SQERingSize: 32768,
			BufferPool:  65536,
			BufferSize:  65536,
			MaxEvents:   8192,
			MaxConns:    65536,
			SocketRecv:  262144,
			SocketSend:  262144,
		}
	case Minimal:
		workers := numCPU / 2
		if workers < MinWorkers {
			workers = MinWorkers
		}
		return ResolvedResources{
			Workers:     workers,
			SQERingSize: 2048,
			BufferPool:  1024,
			BufferSize:  16384,
			MaxEvents:   1024,
			MaxConns:    1024,
			SocketRecv:  65536,
			SocketSend:  65536,
		}
	default:
		return ResolvedResources{
			Workers:     numCPU,
			SQERingSize: min(2048*numCPU, 32768),
			BufferPool:  min(512*numCPU, 65536),
			BufferSize:  65536,
			MaxEvents:   min(1024*numCPU, 8192),
			MaxConns:    min(512*numCPU, 65536),
			SocketRecv:  262144,
			SocketSend:  262144,
		}
	}
}

func clamp(v, minVal, maxVal int) int {
	if minVal > 0 && v < minVal {
		v = minVal
	}
	if maxVal > 0 && v > maxVal {
		v = maxVal
	}
	return v
}

func nextPowerOf2(v int) int {
	if v <= 0 {
		return v
	}
	v--
	v |= v >> 1
	v |= v >> 2
	v |= v >> 4
	v |= v >> 8
	v |= v >> 16
	v |= v >> 32
	v++
	return v
}
