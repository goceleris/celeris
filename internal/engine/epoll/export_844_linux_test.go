//go:build linux

package epoll

// Exports for the external (package epoll_test) celeris#844 arms that run the
// adaptive engine over this one. Only the test build of this package has them.

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

// TransplantActive844 reports whether any loop is draining to a transplant target.
func (e *Engine) TransplantActive844() bool {
	for _, l := range e.loops {
		if l.transplant.Load() != nil {
			return true
		}
	}
	return false
}

// TierName844 is "n/a" on epoll.
func (e *Engine) TierName844() string { return "n/a" }
