//go:build linux

package iouring

import (
	"context"
	"runtime/debug"
)

// runWatched runs worker w and reports a run that ends without returning.
//
// A synchronous (non-Async) route runs its handler on the worker goroutine,
// so a handler that calls runtime.Goexit (t.FailNow, t.Fatal or t.SkipNow in
// a test's handler, or a library that does) ends the worker goroutine itself.
// recover() returns nil for a Goexit, so nothing can catch it: the worker is
// gone, every connection it owned is never served again, and the worker's
// SO_REUSEPORT listener, which only this worker accepts on, goes unserved too
// (celeris#848). The engine cannot survive that, so it must not be silent:
// returned is set only after run returns, and a Goexit or a panic leaves it
// unset, which the deferred report sees. It does not recover(): a panic still
// ends the process.
//
// The flag is set at this one call site rather than before each of run's
// returns. Every return of run, the setup errors reported on w.ready, the
// shutdown returns and the failed listener re-create, is a normal one, and
// a return added later is covered without a change here.
//
// A panic that unwinds the goroutine runs this defer too, before the runtime
// prints the panic and ends the process, so the report (a logger call and a
// debug.Stack) runs on a goroutine that is about to die; a blocking Logger
// handler would delay the crash. Detecting a panic needs recover(), which is
// not wanted here, so the report is not skipped for one.
//
// The defer also marks the worker exited, on a normal return as well:
// PauseAccept counts an exited worker as closed (Worker.exited). A worker that
// ended without returning has its SO_REUSEPORT listener closed there too
// (reportWorkerExit), so a pause is not held up by it for its whole bound and a
// dead listener does not go on taking a share of new connections that nothing
// will accept.
//
// The defer runs once, when the worker goroutine ends: it costs nothing per
// event-loop iteration and takes no lock.
func (e *Engine) runWatched(ctx context.Context, w *Worker) {
	returned := false
	defer func() {
		if !returned {
			e.reportWorkerExit(w)
		}
		// After shutdown's own flag and notify on a normal return, and after
		// the close below on an abnormal one; the notify is for a
		// PauseAccept that checked in between.
		w.exited.Store(true)
		w.pause.Notify()
	}()
	w.run(ctx)
	returned = true
}

// reportWorkerExit records, and logs at Error, a worker goroutine that ended
// without returning, and takes the worker's listener out of the SO_REUSEPORT
// group (leaveGroupAfterExit). It runs on that goroutine, which owns
// liveConns, so the count needs no synchronization. The log is written last: a
// Logger that blocks must not hold up a PauseAccept.
func (e *Engine) reportWorkerExit(w *Worker) {
	e.abnormalWorkerExits.Add(1)
	keptZC := w.leaveGroupAfterExit()
	w.exited.Store(true)
	w.pause.Notify()
	if w.logger == nil {
		return
	}
	w.logKeptZCSendBufs(keptZC)
	// debug.Stack here shows the frames of the call that ended the goroutine:
	// deferred calls run before a Goexit unwinds, so the handler (or callback)
	// frame and the runtime.Goexit call are still on the stack. Exit path only.
	w.logger.Error("io_uring worker goroutine ended without returning: "+
		"a handler or callback on this worker goroutine called runtime.Goexit (t.FailNow, t.Fatal, t.SkipNow or a library that does), or a panic unwound it; "+
		"the connections this worker owned will not be served again and this worker's SO_REUSEPORT listener will not accept (it is closed now); "+
		"synchronous (non-Async) handlers must not call runtime.Goexit (celeris#848)",
		"engine", "io_uring",
		"worker", w.id,
		"conns", len(w.liveConns),
		"stack", string(debug.Stack()),
	)
}
