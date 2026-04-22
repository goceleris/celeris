package resource

import "runtime"

// Resource limit constants for validation and clamping.
const (
	// MinWorkers is the minimum allowed number of I/O workers.
	MinWorkers = 2
	// MaxSQERing is the maximum io_uring submission queue ring size.
	MaxSQERing = 65536
	// MinBufferSize is the minimum per-connection I/O buffer size in bytes.
	MinBufferSize = 4096
	// MaxBufferSize is the maximum per-connection I/O buffer size in bytes.
	MaxBufferSize = 262144
)

func resolveDefaults() ResolvedResources {
	numCPU := runtime.GOMAXPROCS(0)
	if numCPU < 1 {
		numCPU = 1
	}
	return ResolvedResources{
		Workers:     numCPU,
		SQERingSize: 8192,
		BufferPool:  65536,
		BufferSize:  8192,
		MaxEvents:   8192,
		MaxConns:    65536,
		// SocketRecv / SocketSend = 0 leaves SO_RCVBUF / SO_SNDBUF unset so
		// the kernel's TCP auto-tuning owns the buffer sizing (up to
		// net.ipv4.tcp_rmem / tcp_wmem maxima — typically 6 MiB / 4 MiB).
		// Forcing 256 KiB previously capped rwnd at min(256 KiB, rmem_max)
		// which throttled large body POSTs by ~10 % on hosts where
		// net.core.rmem_max < 256 KiB (the default on most distros).
		SocketRecv: 0,
		SocketSend: 0,
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
