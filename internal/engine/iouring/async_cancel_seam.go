//go:build linux

package iouring

// AsyncCancelAnswer is an answer the async-cancel-flags probe can give: what
// the kernel said to the probe's cancel, or that it said nothing. The
// package's own name for it is asyncCancelProbe; this is the spelling a test of
// another package forces (ForceAsyncCancelProbe).
type AsyncCancelAnswer = asyncCancelProbe

// The answers ForceAsyncCancelProbe can give. Rejected is the one every kernel
// before Linux 5.19 gives, and the one that makes New refuse io_uring
// (requireAsyncCancelFlags, celeris#682).
const (
	AsyncCancelNoAnswer   = asyncCancelNoAnswer
	AsyncCancelAccepted   = asyncCancelAccepted
	AsyncCancelRejected   = asyncCancelRejected
	AsyncCancelUnexpected = asyncCancelUnexpected
)

// ForceAsyncCancelProbe makes the async-cancel-flags probe answer answer, with
// reason, to every New until the returned restore function runs, whatever the
// kernel is. It exists so that a test outside this package can reach the
// refusal of New that only a kernel before 5.19, or one that rejects the
// IORING_ASYNC_CANCEL flags, gives (celeris#886 item 3): the adaptive engine
// falling back to epoll when the io_uring start engine is refused, its lazy
// standby build backing off, and Server.Start failing for Engine: IOUring. CI
// runs on a kernel that accepts the flags, so without it none of that is
// reached. Not for production.
//
// The seam is replaced and the cached answer forgotten under asyncCancelMu,
// the lock probeAsyncCancelFlagsCached holds while it reads the seam, so the
// swap is not a data race with a New running on another goroutine (an engine
// the caller does not own, say). restore puts the probe back and forgets the
// cached answer again, so the next New asks the kernel. The answer applies to
// the process: a test that forces it must not run in parallel with another
// that builds an io_uring engine, and must call restore (t.Cleanup).
func ForceAsyncCancelProbe(answer AsyncCancelAnswer, reason string) (restore func()) {
	asyncCancelMu.Lock()
	saved := runAsyncCancelProbe
	runAsyncCancelProbe = func() (asyncCancelProbe, string) { return answer, reason }
	forgetAsyncCancelAnswerLocked()
	asyncCancelMu.Unlock()
	return func() {
		asyncCancelMu.Lock()
		runAsyncCancelProbe = saved
		forgetAsyncCancelAnswerLocked()
		asyncCancelMu.Unlock()
	}
}

// forgetAsyncCancelAnswerLocked clears the cache; asyncCancelMu is held.
func forgetAsyncCancelAnswerLocked() {
	asyncCancelKnown = false
	cachedAsyncCancelRes, cachedAsyncCancelReas = asyncCancelNoAnswer, ""
}
