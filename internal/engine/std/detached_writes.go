package std

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// pastDeadline is a write deadline that has already passed: set on a
// connection (or an HTTP/2 stream) it fails the write that is parked in it, and
// every write after it.
var pastDeadline = time.Unix(1, 0)

// detachedWrites is what a detached stream's writes (celeris#855) carry on the
// std engine, from Context.Detach until the request's ServeHTTP returns.
//
// net/http arms one absolute write deadline for the whole response
// (WriteTimeout), and a stream that is detached has none of it: an SSE stream
// is meant to outlive any WriteTimeout. Without a deadline, though, nothing
// ends a stream whose client stays connected and reads nothing: its handler
// parks in a socket write for ever. Two things bound that, neither absolute:
//
//   - each write call gets its own deadline, WriteTimeout from the moment it
//     starts, and none once it has returned (arm, disarm). A stream that is
//     idle between events is therefore never cut: over h2c the HTTP/2 server
//     resets a stream whose write deadline passes whether or not a write is
//     pending, so the deadline must not outlive the write. A negative
//     WriteTimeout (no timeout) arms nothing;
//   - the drain budget: when Shutdown's budget runs out the engine cancels the
//     base context, and poke then fails the write the stream is parked in and
//     every later one. A context cannot interrupt a socket write, and std
//     never closes the connection of a handler that is still running.
//
// Writes are serialised by the stream's writer (the SSE client's mutex), so
// arm and disarm do not race each other; poke runs on its own goroutine.
type detachedWrites struct {
	rc *http.ResponseController
	// wt is the deadline of each write, 0 for none.
	wt time.Duration
	// closing is set by poke, before it fails the stream's writes: once set,
	// arm and disarm keep the deadline in the past.
	closing atomic.Bool
	// stopPoke stops the poke registered on the engine's base context.
	stopPoke func() bool

	// mu orders poke against end: poke does not touch the response once the
	// request is over, where it would set a deadline for the next request on
	// a kept-alive connection (HTTP/1), or on a stream that has gone back to
	// its pool (HTTP/2).
	mu    sync.Mutex
	ended bool
}

// arm sets the deadline of the write that is about to start.
func (d *detachedWrites) arm() {
	closing := d.closing.Load()
	if d.wt <= 0 && !closing {
		return
	}
	dl := pastDeadline
	if !closing {
		dl = time.Now().Add(d.wt)
	}
	_ = d.rc.SetWriteDeadline(dl)
	// A poke between the load and the set would be undone by the set: look
	// again, now that the deadline is in place. poke stores closing before it
	// sets its deadline, so either this load sees closing, or poke's set
	// comes after ours.
	if !closing && d.closing.Load() {
		_ = d.rc.SetWriteDeadline(pastDeadline)
	}
}

// disarm clears the deadline of the write that has returned, unless poke has
// failed the stream's writes: its deadline stays.
func (d *detachedWrites) disarm() {
	if d.wt > 0 && !d.closing.Load() {
		_ = d.rc.SetWriteDeadline(time.Time{})
	}
}

// poke fails the writes of the stream: the drain budget has run out. The
// connection is not kept alive after it: net/http does not keep one whose
// handler's write failed, and the response's last write fails with it.
func (d *detachedWrites) poke() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ended {
		return
	}
	d.closing.Store(true)
	_ = d.rc.SetWriteDeadline(pastDeadline)
}

// end is called when the request's ServeHTTP is about to return, with the
// handler done. No poke touches the response after it returns. The response
// still has bytes to write (the end of a chunked body, the end of an HTTP/2
// stream), and it is the last write net/http makes with this deadline:
// bound it as the others, unless the stream was poked, when it fails.
func (d *detachedWrites) end() {
	d.mu.Lock()
	d.ended = true
	d.mu.Unlock()
	if d.stopPoke != nil {
		d.stopPoke()
	}
	if d.wt > 0 && !d.closing.Load() {
		_ = d.rc.SetWriteDeadline(time.Now().Add(d.wt))
	}
}
