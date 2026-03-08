package resource

// ResourcePreset selects a predefined resource allocation profile.
type ResourcePreset uint8 //nolint:revive // ResourcePreset is clearer than Preset for cross-package use

// Resource preset constants.
const (
	Greedy ResourcePreset = iota
	Balanced
	Minimal
)

func (p ResourcePreset) String() string {
	switch p {
	case Greedy:
		return "greedy"
	case Balanced:
		return "balanced"
	case Minimal:
		return "minimal"
	default:
		return "unknown"
	}
}

// Resources allows user overrides of preset values.
// Zero values mean "use preset default".
type Resources struct {
	Preset      ResourcePreset
	Workers     int
	SQERingSize int
	BufferPool  int
	BufferSize  int
	MaxEvents   int
	MaxConns    int
	SocketRecv  int
	SocketSend  int
}

// ResolvedResources contains the final computed values after applying presets and overrides.
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

// Resolve applies preset defaults, user overrides, and hard caps.
func (r Resources) Resolve(numCPU int) ResolvedResources {
	res := resolvePreset(r.Preset, numCPU)

	if r.Workers > 0 {
		res.Workers = r.Workers
	}
	if r.SQERingSize > 0 {
		res.SQERingSize = r.SQERingSize
	}
	if r.BufferPool > 0 {
		res.BufferPool = r.BufferPool
	}
	if r.BufferSize > 0 {
		res.BufferSize = r.BufferSize
	}
	if r.MaxEvents > 0 {
		res.MaxEvents = r.MaxEvents
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
