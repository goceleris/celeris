package engine

// EngineType identifies which I/O engine implementation is in use.
type EngineType uint8 //nolint:revive // user-approved name

// I/O engine implementation types. The zero value is engineDefault which
// resolves to Adaptive on Linux and Std elsewhere (see resource.Config.WithDefaults).
const (
	engineDefault EngineType = iota // resolved at config time
	IOUring                         // io_uring-based engine
	Epoll                           // epoll-based engine
	Adaptive                        // runtime-adaptive engine selection
	Std                             // net/http stdlib engine
)

// IsDefault reports whether the engine type is the unset zero value.
func (t EngineType) IsDefault() bool { return t == engineDefault }

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
