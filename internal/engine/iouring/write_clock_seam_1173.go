//go:build linux

package iouring

import "sync/atomic"

// h2WriteDrainSeam is a test seam (celeris#1173): when set, drainH2Writes calls
// it, on the worker's thread, for each HTTP/2 conn with frames queued by a
// handler's goroutine, before it stages them for a SEND. A test reads the
// worker's clock (w.cachedNow) there to see how old the write clock's stamp
// would be (flushSend stamps lastActivity with it). An atomic.Pointer, as
// testHookCheckTimeouts is, because workers of an earlier engine may still read
// it when a later test sets it.
var h2WriteDrainSeam atomic.Pointer[func(w *Worker, fd int)]
