package overload

// Stage represents the current overload degradation level.
type Stage uint8

// Overload degradation stages, ordered by severity.
const (
	Normal       Stage = iota // normal operation
	Expand                    // expand worker pool
	Reap                      // reap idle connections
	Reorder                   // switch to LIFO scheduling
	Backpressure              // delay accepts, limit concurrency
	Reject                    // reject new connections
)

func (s Stage) String() string {
	switch s {
	case Normal:
		return "normal"
	case Expand:
		return "expand"
	case Reap:
		return "reap"
	case Reorder:
		return "reorder"
	case Backpressure:
		return "backpressure"
	case Reject:
		return "reject"
	default:
		return "unknown"
	}
}
