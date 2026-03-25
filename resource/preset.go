package resource

import "runtime"

// Resource limit constants for validation and clamping.
const (
	MinWorkers    = 2
	MaxSQERing    = 65536
	MinBufferSize = 4096
	MaxBufferSize = 262144
)

func resolveDefaults() ResolvedResources {
	numCPU := runtime.GOMAXPROCS(0)
	if numCPU < 1 {
		numCPU = 1
	}
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
