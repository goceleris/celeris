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

// WriteStrategy controls whether writes are issued immediately or batched.
type WriteStrategy uint8

// Write strategy constants.
const (
	WriteImmediate WriteStrategy = iota
	WriteBatched
)

func (w WriteStrategy) String() string {
	switch w {
	case WriteImmediate:
		return "immediate"
	case WriteBatched:
		return "batched"
	default:
		return "unknown"
	}
}

// ObjectiveParams holds the resolved I/O and networking parameters for an objective profile.
type ObjectiveParams struct {
	CQBatch      int
	EpollTimeout time.Duration
	SOBusyPoll   time.Duration
	BufferSize   int
	SQERingScale int
	Write        WriteStrategy
	SQPollIdle   time.Duration
	TCPNoDelay   bool
	TCPQuickAck  bool
}

// ResolveObjective returns the concrete I/O parameters for the given objective profile.
func ResolveObjective(profile ObjectiveProfile) ObjectiveParams {
	switch profile {
	case LatencyOptimized:
		return ObjectiveParams{
			CQBatch:      32,
			EpollTimeout: 0,
			SOBusyPoll:   100 * time.Microsecond,
			BufferSize:   16384,
			SQERingScale: 1,
			Write:        WriteImmediate,
			SQPollIdle:   5000 * time.Millisecond,
			TCPNoDelay:   true,
			TCPQuickAck:  true,
		}
	case ThroughputOptimized:
		return ObjectiveParams{
			CQBatch:      256,
			EpollTimeout: 10 * time.Millisecond,
			SOBusyPoll:   0,
			BufferSize:   65536,
			SQERingScale: 2,
			Write:        WriteBatched,
			SQPollIdle:   1000 * time.Millisecond,
			TCPNoDelay:   true,
			TCPQuickAck:  false,
		}
	default:
		return ObjectiveParams{
			CQBatch:      128,
			EpollTimeout: 1 * time.Millisecond,
			SOBusyPoll:   50 * time.Microsecond,
			BufferSize:   65536,
			SQERingScale: 1,
			Write:        WriteBatched,
			SQPollIdle:   2000 * time.Millisecond,
			TCPNoDelay:   true,
			TCPQuickAck:  true,
		}
	}
}
