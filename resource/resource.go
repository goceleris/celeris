package resource

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
