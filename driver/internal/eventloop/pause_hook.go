package eventloop

import (
	"sync/atomic"
	"time"
)

// Pause is a test instrument that models a descheduled worker: one call in
// every Every of [DispatchPause] sleeps for Delay. A worker can be preempted,
// or stopped by a GC, between taking the request a reply belongs to and writing
// the reply into it; the scheduler can always do that, and the event loop
// documents that onRecv can run after the conn's close (UnregisterConn), so a
// driver that hands a failed request back for reuse has to survive the late
// write. A driver's test installs a Pause with [SetDispatchPause] and checks
// that no caller ever gets another caller's reply, with the window made wide
// enough (a few hundred microseconds) for the race detector's slowdown not to
// be needed to land in it.
//
// It is for tests only. Production code never installs one, and then every
// [DispatchPause] is a function call (it is not inlined) and one atomic load of a nil pointer,
// per read of a worker and per reply line of a driver.
type Pause struct {
	every uint64
	delay time.Duration
	calls atomic.Uint64
	hits  atomic.Uint64
}

// NewPause returns a Pause that sleeps for delay on one call in every every
// (every <= 1 means every call).
func NewPause(every int, delay time.Duration) *Pause {
	if every < 1 {
		every = 1
	}
	return &Pause{every: uint64(every), delay: delay}
}

// Hits is the number of calls that slept.
func (p *Pause) Hits() uint64 { return p.hits.Load() }

// Calls is the number of [DispatchPause] calls it has seen.
func (p *Pause) Calls() uint64 { return p.calls.Load() }

var dispatchPause atomic.Pointer[Pause]

// SetDispatchPause installs p (nil removes it) and returns the function that
// puts back what was installed before. Install it before the traffic of the
// test starts, and restore it when the test is done (t.Cleanup).
func SetDispatchPause(p *Pause) (restore func()) {
	prev := dispatchPause.Swap(p)
	return func() { dispatchPause.Store(prev) }
}

// DispatchPause is the injection point. The event loop's workers call it
// between reading a chunk and handing it to onRecv, and a driver calls it in
// its dispatch, after it has taken the request a reply belongs to and before it
// writes the reply into it. With no Pause installed it is a function call and one atomic load of a nil
// pointer (it is not inlined).
func DispatchPause() {
	p := dispatchPause.Load()
	if p == nil {
		return
	}
	if p.calls.Add(1)%p.every != 0 {
		return
	}
	p.hits.Add(1)
	time.Sleep(p.delay)
}
