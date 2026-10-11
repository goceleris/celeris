//go:build linux

package iouring

// cancelAcceptSeam is a test seam (celeris#896): when set, cancelAccept calls it
// first, on the worker's thread, with the descriptor it is about to cancel. A
// test counts the calls to prove a shutdown reached stopAccepting or the pause
// close, and uses it to make the SQ ring full at that moment. Nil in production.
var cancelAcceptSeam func(w *Worker, lfd int)
