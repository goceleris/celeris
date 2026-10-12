//go:build linux

package eventloop

import (
	"context"
	"log/slog"
	"runtime/debug"
)

// runWatched runs w and reports a run that ends without returning.
//
// The drivers' own callbacks run on the worker goroutine: every onRecv and
// onClose, the OnPush of a redis connection, and, for postgres, the dest
// function a caller hands to Pool.CopyTo. One that calls runtime.Goexit (a
// test's t.FailNow or t.SkipNow, or a library that does) ends the worker
// goroutine. recover() returns nil for a Goexit, so nothing can catch it: the
// worker is gone, and every conn it owned is never read again (celeris#887
// item 18, the driver loop's twin of celeris#848 for the HTTP engines).
// The loop cannot survive that, so it must not be silent: returned is set only
// after run returns, and a Goexit or a panic leaves it unset, which the
// deferred report sees. It does not recover(): a panic still ends the process.
//
// The flag is set at this one call site rather than before each of run's
// returns: every return of run (the cancelled context, a failed epoll_wait) is
// a normal one, and a return added later is covered without a change here.
//
// The defer runs once, when the goroutine ends: it costs nothing per
// event-loop iteration and takes no lock. It runs before the deferred
// wg.Done of the caller, so Loop.Close, which waits for the workers, returns
// after the report.
func (w *worker) runWatched(ctx context.Context) {
	returned := false
	defer func() {
		if !returned {
			reportWorkerExit(w)
		}
	}()
	w.run(ctx)
	returned = true
}

// reportWorkerExit logs, at Error on slog's default logger, a worker
// goroutine that ended without returning. The driver packages have no logger
// of their own to take (the Loop is shared by every driver client in the
// process), so the default one is used. It runs on the dying goroutine and takes
// no lock: a callback may end the goroutine while it holds one.
func reportWorkerExit(w *worker) {
	// debug.Stack here shows the frames of the call that ended the goroutine:
	// deferred calls run before a Goexit unwinds, so the callback frame and
	// the runtime.Goexit call are still on the stack. Exit path only.
	slog.Default().Error("celeris/eventloop: driver event-loop worker goroutine ended without returning: "+
		"a callback on this worker (a driver's onRecv or onClose, a redis OnPush handler, a postgres CopyTo dest) "+
		"called runtime.Goexit (t.FailNow, t.SkipNow or a library that does), or a panic unwound it; "+
		"the connections this worker owned will not be read again; "+
		"these callbacks run on the event loop and must not call runtime.Goexit (celeris#887)",
		"worker", w.id,
		"stack", string(debug.Stack()),
	)
}
