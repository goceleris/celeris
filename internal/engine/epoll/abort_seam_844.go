//go:build linux

package epoll

import "sync/atomic"

// Fault-injection seams for the async dispatch goroutine's panic windows
// (celeris#844). All are nil in production and set only by tests, which
// cannot reach any window otherwise: each holds a lock while it runs only
// engine code, so the fault has to be injected at the exact point.
//
// Each is read through an atomic pointer so a test arming it after the engine
// started is not a data race, and costs one load on a path that runs once per
// Detach (detachWindowHook, detachPreFlagHook) or once per park while a
// transplant drain is active (parkWindowHook), never on the per-request path.
var (
	// detachWindowHook runs in OnDetach after asyncDetachUnlocked is set and
	// before the dispatch goroutine's detachMu is released: the window item 2
	// of celeris#844 describes.
	detachWindowHook atomic.Pointer[func()]

	// detachPreFlagHook runs in OnDetach right after the guarded writeFn is
	// installed and before asyncDetachUnlocked is set: the span in which a
	// write through the adapter already takes detachMu, which the dispatch
	// goroutine still holds (item 2 of celeris#844, the shape a handler that
	// recovers the panic, as the router does, then answers in).
	detachPreFlagHook atomic.Pointer[func()]

	// detachPostInstallHook runs in OnDetach of a conn that is not on the
	// promoted async path, once everything the hook installs is in place (the
	// guarded writeFn, the adapter's, RawWriteFn, pause/resume): the point a
	// panic must be undone from, because the conn goes on as an ordinary
	// keep-alive conn (#889 item 34).
	detachPostInstallHook atomic.Pointer[func()]

	// parkWindowHook runs in askTransplant, which askAtPark calls from the
	// park loop of serveAsync holding cs.asyncInMu: the engine's own code
	// under that lock (item 1 of celeris#844).
	parkWindowHook atomic.Pointer[func()]
)
