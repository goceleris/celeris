//go:build linux

package iouring

import "testing"

// Exports for the external (package iouring_test) celeris#844 arms that run
// the adaptive engine over this one. Only the test build of this package has
// them.

// SetDetachWindowHook844 arms (or, with nil, clears) the fault hook in OnDetach's window.
func SetDetachWindowHook844(f func()) {
	if f == nil {
		detachWindowHook.Store(nil)
		return
	}
	detachWindowHook.Store(&f)
}

// SetParkWindowHook844 arms (or, with nil, clears) the fault hook under asyncInMu in the park loop.
func SetParkWindowHook844(f func()) {
	if f == nil {
		parkWindowHook.Store(nil)
		return
	}
	parkWindowHook.Store(&f)
}

// TransplantActive844 reports whether any worker is draining to a transplant target.
func (e *Engine) TransplantActive844() bool {
	e.mu.Lock()
	ws := e.workers
	e.mu.Unlock()
	for _, w := range ws {
		if w.transplant.Load() != nil {
			return true
		}
	}
	return false
}

// TierName844 names the recv tier the workers run: "high" with a provided
// buffer ring, "base" without.
func (e *Engine) TierName844() string { return tier844(e) }

// RetryRingENOMEM844 runs start, retrying one that failed only on ring ENOMEM (see retryRingENOMEM).
func RetryRingENOMEM844(start func() error) error {
	_, _, err := retryRingENOMEM(start)
	return err
}

// SkipUnlessIOUringUnusable844 skips only if io_uring cannot be used here at all
// (or RLIMIT_MEMLOCK is below the floor the iouring tests hold themselves to),
// and fails otherwise (see skipUnlessIOUringUnusable).
func SkipUnlessIOUringUnusable844(t *testing.T, err error) {
	t.Helper()
	skipUnlessIOUringUnusable(t, err)
}
