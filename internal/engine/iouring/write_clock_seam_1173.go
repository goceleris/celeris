//go:build linux

package iouring

// h2WriteDrainSeam is a test seam (celeris#1173): when set, drainH2Writes calls
// it, on the worker's thread, for each HTTP/2 conn with frames queued by a
// handler's goroutine, before it stages them for a SEND. A test reads the
// worker's clock (w.cachedNow) there to see how old the write clock's stamp
// would be (flushSend stamps lastActivity with it).
var h2WriteDrainSeam func(w *Worker, fd int)
