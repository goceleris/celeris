//go:build linux

package iouring

import "sync/atomic"

// mainThreadSeam890 is a test seam (celeris#890 item 42): when set, Listen
// offers the first worker's run to it instead of starting a goroutine for it.
// The seam returns true when it took the run and will call it, once, on the
// process's main thread. A worker runs on the main thread in only some runs
// when nothing sends it there (8 of 20 first cycles on a laptop, none in a CI
// run), and the restore of the thread's CPU mask on the way out (celeris#905,
// Worker.run) matters for that thread alone: every other worker's thread
// exits. A test that must exercise the restore starts a worker there on
// purpose. An atomic pointer: a test sets it while workers of an earlier
// engine of the package may still be winding down. One load per Listen.
var mainThreadSeam890 atomic.Pointer[func(run func()) bool]

// runOnMainThread offers run to mainThreadSeam890 and reports whether the seam
// took it.
func runOnMainThread(run func()) bool {
	if f := mainThreadSeam890.Load(); f != nil {
		return (*f)(run)
	}
	return false
}
