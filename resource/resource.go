package resource

// Resources allows user overrides of default values.
// Zero values mean "use default".
type Resources struct {
	Workers     int
	BufferSize  int
	SocketRecv  int
	SocketSend  int
	MaxConns    int
}

// ResolvedResources contains the final computed values after applying defaults and overrides.
type ResolvedResources struct {
	Workers     int
	SQERingSize int
	BufferPool  int
	BufferSize  int
	MaxEvents   int
	MaxConns    int
	SocketRecv  int
	SocketSend  int
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
