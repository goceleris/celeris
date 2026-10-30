// Package drainclock is the clock of the native engines' shutdown deadlines:
// the send drain, the wait for HTTP/2 pool handlers, the gate's pacing and the
// bounded waits for the kernel's completions (celeris#1176).
//
// Those deadlines were taken and read as time.Now().UnixNano(), which is the
// wall clock: a step of it (an NTP step, a VM clock sync, a suspend and resume)
// during a Shutdown moved every deadline with it. Forward, a drain with time
// left to run was found over and its responses were cut; backward, one that was
// over lasted as long as the step.
package drainclock

import (
	"sync/atomic"
	"time"
)

// epoch anchors the clock. time.Now carries a monotonic reading, and time.Since
// between two readings that both carry one uses the monotonic clock alone
// (package time, "Monotonic Clocks").
var epoch = time.Now()

// Reading is one reading of both clocks, which is what a test replaces to step
// one of them without the other (SetSeam). The wall clock is not a clock of
// any deadline here; it is in the pair so that a test can say what moved.
type Reading struct {
	Wall int64 // time.Now().UnixNano()
	Mono int64 // nanoseconds on the monotonic clock since the package was initialised; never 0
}

// seam is a test seam: nil in production and set only by tests, which cannot
// step the process's wall clock (the machine's clock is shared). It is one
// atomic load per read, taken by a shutdown's deadlines and by the gate's
// pacing, never by a request's path.
var seam atomic.Pointer[func() Reading]

func read() Reading {
	if f := seam.Load(); f != nil {
		return (*f)()
	}
	t := time.Now()
	return Reading{Wall: t.UnixNano(), Mono: int64(t.Sub(epoch)) + 1}
}

// Now is the clock every shutdown deadline is taken and read on: nanoseconds on
// the monotonic clock. A stamp of it is never 0, so 0 can still mean "not
// set". It is not comparable with a wall-clock reading (cachedNow, a conn's
// lastActivity): the two are on different scales, and a comparison of them is
// wrong by the size of the process's age.
func Now() int64 { return read().Wall }

// At is d on the Now scale, for a deadline taken from a context. A deadline of
// context.WithTimeout carries a monotonic reading, which time.Until uses; one
// with none (context.WithDeadline of a wall-clock time) is an instant of the
// wall clock, and is read against it as it is now.
func At(d time.Time) int64 { return d.UnixNano() }

// SetSeam makes Now read f instead of the clocks, until the returned function
// is called, which puts back what was set before, but only if this call's f is
// still the one installed. Tests only; the seam is process-wide, so a test that
// sets it runs no other engine's shutdown meanwhile.
func SetSeam(f func() Reading) (restore func()) {
	var p *func() Reading
	if f != nil {
		p = &f
	}
	prev := seam.Swap(p)
	return func() { seam.CompareAndSwap(p, prev) }
}
