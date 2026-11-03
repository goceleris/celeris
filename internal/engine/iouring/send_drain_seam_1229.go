//go:build linux

package iouring

import "sync/atomic"

// sendDrainSeam is a test seam (celeris#1229): when set, the send drain calls it
// on every pass, on the worker's thread, before it takes its snapshot of the
// requests in the kernel (once, on its first pass) and asks whether it is done
// (sendDrainDone). A test blocks the worker in it, with a request written to a
// connection the worker has accepted: a head that is in the kernel when the
// worker sees the shutdown, whatever the machine's speed. Several workers call
// it, so a test keeps its state atomic. Nil in production, where the shutdown
// path costs one load of this variable and no other path any. An
// atomic.Pointer, as ringWaitSeam is, because a worker an earlier test left
// running may still read it when a later test sets or restores it.
var sendDrainSeam atomic.Pointer[func(w *Worker)]
