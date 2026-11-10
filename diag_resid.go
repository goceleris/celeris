package celeris

import "github.com/goceleris/celeris/internal/diag/resid"

// DiagResidSnapshot returns the cumulative server-residence histograms of
// celeris#757, summed over the epoll engine's loops. Its Line method formats
// the `[res]` line that the probatorium sampler prints beside `[sched]` each
// second. Everything is zero unless CELERIS_DIAG_RESID is set in the process
// environment.
//
// THROWAWAY DIAGNOSTIC of the diag/757-residence-stamps branch: not API, never
// merged, not part of any release.
func DiagResidSnapshot() resid.Snap { return resid.Snapshot() }
