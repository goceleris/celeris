//go:build !race

package celeris

import "time"

// bindDeadline is the per-cell wall budget for celeris.Server's listener to
// publish a non-nil Addr() after the cell-orchestrator hands off the
// pre-bound listener. 5 s is comfortable on baseline (non-race) builds:
// even msr1 ARM with 12 workers + dual adaptive engines binds in <1 s
// without race instrumentation.
const bindDeadline = 5 * time.Second
