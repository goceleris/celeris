//go:build linux

package iouring

import "sync/atomic"

// Fault-injection seams for the async dispatch goroutine's two panic windows
// (celeris#844). Both are nil in production and set only by tests, which
// cannot reach either window otherwise: each holds a lock while it runs only
// engine code, so the fault has to be injected at the exact point.
//
// Each is read through an atomic pointer so a test arming it after the engine
// started is not a data race, and costs one load on a path that runs once per
// Detach (detachWindowHook) or once per park while a transplant drain is
// active (parkWindowHook), never on the per-request path.
var (
	// detachWindowHook runs in OnDetach after asyncDetachUnlocked is set and
	// before the dispatch goroutine's detachMu is released: the window item 2
	// of celeris#844 describes.
	detachWindowHook atomic.Pointer[func()]

	// parkWindowHook runs in asyncTransplantEligible, which the park loop of
	// serveAsync calls holding cs.asyncInMu: the engine's own code under that
	// lock (item 1 of celeris#844 on the tier where canRevertToInline never
	// reaches application code).
	parkWindowHook atomic.Pointer[func()]
)
