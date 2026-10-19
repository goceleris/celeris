//go:build linux

package epoll

import "sync/atomic"

// drainPollHook is a test seam: nil in production and set only by tests, which
// cannot otherwise tell where in a shutdown the send drain is (celeris#885
// items 12 and 13). When set, drainSends runs it on the loop thread each time
// it is about to wait for a socket to take more of a queued response: after
// sendDrainWait has said the drain may go on, before the poll. So it runs
// only for a drain that has something to wait for, once per wait.
//
// It is read through an atomic pointer, so a test arming it after the engine
// started is not a data race, and it costs one load per wait of a shutdown's
// send drain, nothing on a request's path.
var drainPollHook atomic.Pointer[func()]

// SetDrainPollHook makes every epoll loop of this process run f at each wait
// of its shutdown send drain (see drainPollHook), until the returned function
// is called, which puts back what was set before, but only if this call's hook
// is still the one installed: a restore that runs after a later caller
// replaced the hook leaves that caller's hook alone. f runs on the loop
// thread with no lock held, and must not block. Tests only: the hook is
// process-wide, so a test that sets it runs no other engine's shutdown
// meanwhile.
func SetDrainPollHook(f func()) (restore func()) {
	var p *func()
	if f != nil {
		p = &f
	}
	prev := drainPollHook.Swap(p)
	return func() { drainPollHook.CompareAndSwap(p, prev) }
}
