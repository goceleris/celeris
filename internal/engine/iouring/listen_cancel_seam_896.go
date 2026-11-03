//go:build linux

package iouring

import "sync/atomic"

// cancelAcceptSeam is a test seam (celeris#896): when set, cancelAccept calls it
// first, on the worker's thread, with the descriptor it is about to cancel. A
// test counts the calls to prove a shutdown reached stopAccepting or the pause
// close, and uses it to make the SQ ring full at that moment. Nil in production.
// An atomic.Pointer, as h2WriteDrainSeam is, because a worker an earlier test
// left running may still read it when a later test sets or restores it
// (celeris#1229).
var cancelAcceptSeam atomic.Pointer[func(w *Worker, lfd int)]
