package engine

// EngineType identifies which I/O engine implementation is in use.
type EngineType uint8

const (
	IOUring  EngineType = iota
	Epoll
	Adaptive
	Std
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
