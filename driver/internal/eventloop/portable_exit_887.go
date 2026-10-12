package eventloop

import (
	"log/slog"
	"runtime/debug"
)

// readerLoop runs the reader goroutine of c and reports a run that ends
// without returning.
//
// The drivers' callbacks run on this goroutine: onRecv, onClose, a redis
// OnPush handler, and a postgres CopyTo dest. One that calls runtime.Goexit (a
// test's t.FailNow, which is how this development path is used) ends the
// goroutine without a return, and recover() cannot see it: the conn is never
// read again and never reports onClose (celeris#887 item 18, the portable
// twin of the Linux worker's runWatched). returned is set only after
// readLoop returns, so a Goexit or a panic leaves it unset, which the deferred
// report sees. The wg.Done is deferred first so it runs last: Loop.Close,
// which waits for the readers, returns after the report.
func (w *portableWorker) readerLoop(c *portableConn) {
	defer w.wg.Done()
	returned := false
	defer func() {
		if !returned {
			reportReaderExit(w, c)
		}
	}()
	w.readLoop(c)
	returned = true
}

// reportReaderExit logs, at Error on slog's default logger, a reader goroutine
// that ended without returning. It runs on the dying goroutine and takes no
// lock: a callback may end the goroutine while it holds one.
func reportReaderExit(w *portableWorker, c *portableConn) {
	slog.Default().Error("celeris/eventloop: portable event-loop reader goroutine ended without returning: "+
		"a callback on this conn (a driver's onRecv or onClose, a redis OnPush handler, a postgres CopyTo dest) "+
		"called runtime.Goexit (t.FailNow, t.SkipNow or a library that does), or a panic unwound it; "+
		"this connection will not be read again and will not report onClose; "+
		"these callbacks run on the event loop and must not call runtime.Goexit (celeris#887)",
		"worker", w.id,
		"conn", c.fd,
		"stack", string(debug.Stack()),
	)
}
