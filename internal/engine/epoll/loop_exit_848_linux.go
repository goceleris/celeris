//go:build linux

package epoll

import (
	"context"
	"runtime/debug"
)

// runWatched runs loop l and reports a run that ends without returning.
//
// A synchronous (non-Async) route runs its handler on the loop goroutine, so
// a handler that calls runtime.Goexit (t.FailNow, t.Fatal or t.SkipNow in a
// test's handler, or a library that does) ends the loop goroutine itself.
// recover() returns nil for a Goexit, so nothing can catch it: the loop is
// gone, every connection it owned is never served again, and the loop's
// SO_REUSEPORT listener, which only this loop accepts on, goes unserved too
// (celeris#848). The engine cannot survive that, so it must not be silent:
// returned is set only after run returns, and a Goexit or a panic leaves it
// unset, which the deferred report sees. It does not recover(): a panic still
// ends the process.
//
// The flag is set at this one call site rather than before each of run's
// returns. Every return of run, the setup errors reported on l.ready, the
// shutdown returns and the failed listener re-create, is a normal one, and
// a return added later is covered without a change here.
//
// A panic that unwinds the goroutine runs this defer too, before the runtime
// prints the panic and ends the process, so the report (a logger call and a
// debug.Stack) runs on a goroutine that is about to die; a blocking Logger
// handler would delay the crash. Detecting a panic needs recover(), which is
// not wanted here, so the report is not skipped for one.
//
// The defer also marks the loop exited, on a normal return as well: PauseAccept
// counts an exited loop as closed (Loop.exited). A loop that ended without
// returning has its SO_REUSEPORT listener closed there too (reportLoopExit),
// so a pause is not held up by it for its whole bound and a dead listener does
// not go on taking a share of new connections that nothing will accept.
//
// The defer runs once, when the loop goroutine ends: it costs nothing per
// event-loop iteration and takes no lock.
func (e *Engine) runWatched(ctx context.Context, l *Loop) {
	returned := false
	defer func() {
		if !returned {
			e.reportLoopExit(l)
		}
		// After shutdown's own flag and notify on a normal return, and after
		// the close below on an abnormal one; the notify is for a
		// PauseAccept that checked in between.
		l.exited.Store(true)
		l.pause.Notify()
	}()
	l.run(ctx)
	returned = true
}

// reportLoopExit records, and logs at Error, a loop goroutine that ended
// without returning, and takes the loop's listener out of the SO_REUSEPORT
// group. It runs on that goroutine, which owns liveConns, so the count needs
// no synchronization.
//
// The listener is closed first (stopAccepting: the epoll registration, then the
// descriptor, which epoll releases at once), then the flags PauseAccept reads
// are set and the wait is notified, then the log is written: a Logger that
// blocks must not hold up a PauseAccept. The loop's epoll descriptor and its
// connections stay open, as they did; only shutdown closes those and it never
// runs for a dead loop. A Goexit raised inside shutdown, after it closed the
// listener, finds listenFD already -1.
func (e *Engine) reportLoopExit(l *Loop) {
	e.abnormalLoopExits.Add(1)
	l.stopAccepting()
	l.listenFDClosed.Store(true)
	l.exited.Store(true)
	l.pause.Notify()
	if l.logger == nil {
		return
	}
	// debug.Stack here shows the frames of the call that ended the goroutine:
	// deferred calls run before a Goexit unwinds, so the handler (or callback)
	// frame and the runtime.Goexit call are still on the stack. Exit path only.
	l.logger.Error("epoll loop goroutine ended without returning: "+
		"a handler or callback on this loop goroutine called runtime.Goexit (t.FailNow, t.Fatal, t.SkipNow or a library that does), or a panic unwound it; "+
		"the connections this loop owned will not be served again and this loop's SO_REUSEPORT listener will not accept (it is closed now); "+
		"synchronous (non-Async) handlers must not call runtime.Goexit (celeris#848)",
		"engine", "epoll",
		"loop", l.id,
		"conns", len(l.liveConns),
		"stack", string(debug.Stack()),
	)
}
