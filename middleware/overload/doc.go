// Package overload is a 5-stage CPU degradation ladder driven by
// [observe.Collector].
//
// A background polling goroutine samples
// collector.Snapshot().CPUUtilization every [Config.PollInterval]
// and transitions the stage atomically. Hot-path handlers read the
// stage via a single atomic load — no locks, no map lookups — so
// the Normal path adds only a few nanoseconds of overhead.
//
// Stages (thresholds configurable, hysteresis applied to downward
// transitions):
//
//	Normal       — pass through unchanged
//	Expand       — signal best-effort worker widening; pass through
//	Reap         — opt-in runtime.GC() then pass through
//	Reorder      — low-priority requests return 503; others pass
//	Backpressure — low-priority requests return BackpressureStatus (503);
//	               others sleep BackpressureDelay then pass; exempt pass through
//	Reject       — all non-exempt requests return 503 + Retry-After
//
// Priority is application-defined via [Config.PriorityFunc]. Without it,
// Reorder passes everything and Backpressure delays everything.
package overload
