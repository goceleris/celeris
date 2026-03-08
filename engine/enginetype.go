package engine

// EngineType identifies which I/O engine implementation is in use.
type EngineType uint8 //nolint:revive // user-approved name

// I/O engine implementation types.
const (
	IOUring  EngineType = iota // io_uring-based engine
	Epoll                      // epoll-based engine
	Adaptive                   // runtime-adaptive engine selection
	Std                        // net/http stdlib engine
)

func (t EngineType) String() string {
	switch t {
	case IOUring:
		return "io_uring"
	case Epoll:
		return "epoll"
	case Adaptive:
		return "adaptive"
	case Std:
		return "std"
	default:
		return "unknown"
	}
}
