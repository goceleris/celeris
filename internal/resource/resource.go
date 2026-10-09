package resource

// WorkloadHint is an OPTIONAL operator declaration of the expected steady-state
// concurrency, consumed only by the adaptive engine's start-engine decision.
//
// Because established connections cannot migrate between epoll and io_uring,
// the START engine decides keep-alive throughput; and the expected concurrency
// is unknowable at Listen() time (no connections exist yet). This hint is the
// ONLY input that can make the adaptive engine START on io_uring. Absent it,
// the engine defaults to epoll (every server ramps from zero connections — the
// low-concurrency regime where epoll wins on both throughput and tail latency)
// and lets the runtime switch route NEW connections up to io_uring under load.
type WorkloadHint int

const (
	// WorkloadUnspecified (zero value) leaves the start-engine choice to the
	// default policy: start epoll, promote new conns to io_uring under load.
	WorkloadUnspecified WorkloadHint = iota
	// WorkloadLowConcurrency explicitly declares thin/latency-sensitive traffic
	// — start (and stay) on epoll.
	WorkloadLowConcurrency
	// WorkloadHighConcurrency explicitly declares many h1 keep-alive
	// connections per worker — start on io_uring (when kernel + memlock allow).
	WorkloadHighConcurrency
)

// Resources allows user overrides of default resource values.
// Zero values mean "use engine default".
type Resources struct {
	// Workers is the number of I/O worker goroutines (0 = GOMAXPROCS).
	Workers int
	// BufferSize is the per-connection I/O buffer size in bytes (0 = engine default).
	BufferSize int
	// SocketRecv is the SO_RCVBUF size for accepted connections (0 = OS default).
	SocketRecv int
	// SocketSend is the SO_SNDBUF size for accepted connections (0 = OS default).
	SocketSend int
	// MaxConns is the max simultaneous connections per worker (0 = unlimited).
	MaxConns int
	// WorkloadHint is an OPTIONAL operator concurrency declaration; the only
	// input that can make the adaptive engine START on io_uring (see WorkloadHint).
	WorkloadHint WorkloadHint
	// MemoryLimitBytes is an OPTIONAL soft heap ceiling (runtime/debug
	// SetMemoryLimit). 0 = unset: celeris does not touch the process GC, so
	// embedders keep full control. When >0, the server applies it at start —
	// the GC then collects before the heap balloons during a connection-ramp
	// burst (with the default GOGC=100 the live-heap-doubling ramp spike is
	// the dominant peak-RSS contributor), trading a few extra GC cycles
	// during the ramp for a lower high-water mark; steady-state RSS sits far
	// below the limit so steady throughput is unaffected. NOTE: SetMemoryLimit
	// is PROCESS-GLOBAL, so this is opt-in only — set it just when celeris
	// owns the process (e.g. a dedicated server binary). See DeriveMemoryLimit.
	MemoryLimitBytes int64
}

// DeriveMemoryLimit returns a generous soft heap ceiling for `workers` I/O
// workers: max(256 MiB, workers*32 MiB). It is sized HIGH on purpose — the
// goal is to clip the connection-ramp balloon, not to run the heap tight
// (a too-tight limit GC-thrashes under load). Operators that want the
// peak-RSS reduction pass this into Resources.MemoryLimitBytes; it is never
// applied implicitly.
func DeriveMemoryLimit(workers int) int64 {
	const floor = 256 << 20 // 256 MiB
	per := int64(workers) * (32 << 20)
	if per < floor {
		return floor
	}
	return per
}

// ResolvedResources contains the final computed values after applying defaults,
// user overrides, and hard caps. Used by engine implementations at startup.
type ResolvedResources struct {
	// Workers is the resolved number of I/O worker goroutines.
	Workers int
	// SQERingSize is the io_uring submission queue size (power of 2).
	SQERingSize int
	// BufferPool is the number of pre-allocated I/O buffers.
	BufferPool int
	// BufferSize is the resolved per-connection I/O buffer size in bytes.
	BufferSize int
	// MaxEvents is the max events returned per epoll_wait call.
	MaxEvents int
	// MaxConns is the resolved max connections per worker.
	MaxConns int
	// SocketRecv is the resolved SO_RCVBUF size.
	SocketRecv int
	// SocketSend is the resolved SO_SNDBUF size.
	SocketSend int
}

// Resolve applies hardcoded defaults, user overrides, and hard caps.
func (r Resources) Resolve() ResolvedResources {
	res := resolveDefaults()

	if r.Workers > 0 {
		res.Workers = r.Workers
	}
	if r.BufferSize > 0 {
		res.BufferSize = r.BufferSize
	}
	if r.MaxConns > 0 {
		res.MaxConns = r.MaxConns
	}
	if r.SocketRecv > 0 {
		res.SocketRecv = r.SocketRecv
	}
	if r.SocketSend > 0 {
		res.SocketSend = r.SocketSend
	}

	res.Workers = clamp(res.Workers, MinWorkers, 0)
	res.SQERingSize = clamp(nextPowerOf2(res.SQERingSize), 0, MaxSQERing)
	res.BufferSize = clamp(res.BufferSize, MinBufferSize, MaxBufferSize)

	return res
}
