// Package overload implements a 5-stage degradation ladder for backpressure and load shedding.
//
// The overload manager monitors CPU utilization and progressively escalates
// through stages: Normal → Expand → Reap → Reorder → Backpressure → Reject.
// Each stage applies engine hooks to shed load. De-escalation requires sustained
// improvement and respects per-stage cooldown timers.
package overload
