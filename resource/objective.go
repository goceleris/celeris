package resource

import "time"

// ObjectiveProfile selects a tuning profile that controls I/O and networking parameters.
type ObjectiveProfile uint8

// Objective profile constants for server tuning.
const (
	BalancedObjective ObjectiveProfile = iota
	LatencyOptimized
	ThroughputOptimized
)

func (o ObjectiveProfile) String() string {
	switch o {
	case LatencyOptimized:
		return "latency"
	case ThroughputOptimized:
		return "throughput"
	case BalancedObjective:
		return "balanced"
	default:
		return "unknown"
	}
}

// ObjectiveParams holds the resolved I/O and networking parameters for an objective profile.
type ObjectiveParams struct {
	EpollTimeout time.Duration
	SOBusyPoll   time.Duration
	BufferSize   int
	SQERingScale int
	SQPollIdle   time.Duration
	TCPNoDelay   bool
	TCPQuickAck  bool
}

// ResolveObjective returns the concrete I/O parameters for the given objective profile.
func ResolveObjective(profile ObjectiveProfile) ObjectiveParams {
	switch profile {
	case LatencyOptimized:
		return ObjectiveParams{
			EpollTimeout: 0,
			SOBusyPoll:   100 * time.Microsecond,
			BufferSize:   16384,
			SQERingScale: 1,
			SQPollIdle:   5000 * time.Millisecond,
			TCPNoDelay:   true,
			TCPQuickAck:  true,
		}
	case ThroughputOptimized:
		return ObjectiveParams{
			EpollTimeout: 10 * time.Millisecond,
			SOBusyPoll:   0,
			BufferSize:   65536,
			SQERingScale: 2,
			SQPollIdle:   1000 * time.Millisecond,
			TCPNoDelay:   true,
			TCPQuickAck:  false,
		}
	default:
		return ObjectiveParams{
			EpollTimeout: 1 * time.Millisecond,
			SOBusyPoll:   50 * time.Microsecond,
			BufferSize:   65536,
			SQERingScale: 1,
			SQPollIdle:   2000 * time.Millisecond,
			TCPNoDelay:   true,
			TCPQuickAck:  true,
		}
	}
}
