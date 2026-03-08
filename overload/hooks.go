package overload

import "time"

// EngineHooks is the interface engines implement to respond to overload actions.
// The overload package does not import any engine package.
type EngineHooks interface {
	ExpandWorkers(maxWorkers int) int
	ReapIdleConnections(idleThreshold time.Duration) int
	SetSchedulingMode(lifo bool)
	SetAcceptPaused(paused bool)
	SetAcceptDelay(d time.Duration)
	SetMaxConcurrent(n int)
	ShrinkWorkers(baseWorkers int, idleThreshold time.Duration) int
	ActiveConnections() int64
	WorkerCount() int
}
