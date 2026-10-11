package std

import (
	"bufio"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// pastDeadline is a write deadline that has already passed: set on a
// connection (or an HTTP/2 stream) it fails the write that is parked in it, and
// every write after it.
var pastDeadline = time.Unix(1, 0)

// epoch is the origin of the monotonic timestamps a detached stream's writes
// record (time.Since reads the runtime clock; no wall clock, no allocation).
var epoch = time.Now()

// detachedWrites is what a detached stream's writes (celeris#855) carry on the
// std engine, from Context.Detach until the request's ServeHTTP returns.
//
// net/http arms one absolute write deadline for the whole response
// (WriteTimeout), and a stream that is detached has none of it: an SSE stream
// is meant to outlive any WriteTimeout. Without a deadline, though, nothing
// ends a stream whose client stays connected and reads nothing: its handler
// parks in a socket write for ever. Two things bound that, neither absolute:
//
//   - a watchdog: a write that has been in flight for WriteTimeout is failed
//     (cut). A stream that is idle between events is never cut, and a negative
//     WriteTimeout (no timeout) has no watchdog. The deadline is not set per
//     write: over h2c every SetWriteDeadline is a message to the connection's
//     one serve goroutine, shared by all its streams, and the HTTP/2 server
//     resets a stream whose write deadline passes whether or not a write is
//     pending. A write only records when it started, and the watchdog looks
//     at it, about once per WriteTimeout and only while a write has been
//     seen;
//   - the drain budget: when Shutdown's budget runs out the engine cancels the
//     base context, and poke then fails the write the stream is parked in and
//     every later one. A context cannot interrupt a socket write, and std
//     never closes the connection of a handler that is still running.
//
// A cut is a poke: the stream's write deadline is in the past for good
// (closing, set once), so every later write fails, and so do the last bytes
// net/http writes after the handler.
//
// Writes after Detach may come from more than one goroutine (Context.Detach is
// public), though net/http's ResponseWriter does not allow them to overlap
// without the caller's own lock. The bookkeeping is atomics and does not need
// that: with writes in flight the bound is measured from the start of the
// latest one, so a stream that keeps starting writes is never cut for a write
// that has finished.
type detachedWrites struct {
	rc *http.ResponseController
	// wt is the longest a write may be parked, 0 for no limit.
	wt time.Duration
	// closing is set before the stream's writes are failed: once set the
	// write deadline is in the past and stays there.
	closing atomic.Bool
	// stopPoke stops the poke registered on the engine's base context.
	stopPoke func() bool

	// start is when the latest write began (nanoseconds since epoch). It is
	// stored before inflight is raised, and read after it, so a reader that
	// sees a write in flight sees that write's start or a later one.
	start atomic.Int64
	// inflight counts the writes that have begun and not returned.
	inflight atomic.Int32
	// watching is true while the watchdog timer is scheduled.
	watching atomic.Bool
	// hijacked is set when the handler took the connection over.
	hijacked atomic.Bool

	// mu orders the watchdog and poke against end: neither touches the
	// response once the request is over, where it would set a deadline for the
	// next request on a kept-alive connection (HTTP/1), or on a stream that
	// has gone back to its pool (HTTP/2). It also guards timer.
	mu    sync.Mutex
	ended bool
	timer *time.Timer
}

// begin records that a write is starting, and wakes the watchdog if it stood
// down. Two atomic stores, one add and one clock read: nothing that waits.
func (d *detachedWrites) begin() {
	if d.wt <= 0 {
		return
	}
	d.start.Store(int64(time.Since(epoch)))
	d.inflight.Add(1)
	if !d.watching.Load() && !d.closing.Load() {
		d.wake()
	}
}

// done records that the write has returned.
func (d *detachedWrites) done() {
	if d.wt > 0 {
		d.inflight.Add(-1)
	}
}

// wake schedules the watchdog for a write that has just begun.
func (d *detachedWrites) wake() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ended || d.closing.Load() || !d.watching.CompareAndSwap(false, true) {
		return
	}
	if d.timer == nil {
		d.timer = time.AfterFunc(d.wt, d.watch)
		return
	}
	d.timer.Reset(d.wt)
}

// watch is the watchdog's timer: it cuts the stream if a write has been in
// flight for WriteTimeout, looks again when the write it found is due, and
// stands down when there is none (the next write wakes it).
func (d *detachedWrites) watch() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ended || d.closing.Load() {
		d.watching.Store(false)
		return
	}
	if d.inflight.Load() == 0 {
		d.watching.Store(false)
		// A write may have begun after the load and found watching still set:
		// look again, now that it is not. inflight is raised before begin
		// reads watching, so either that write is seen here, or it saw
		// watching false and wakes the watchdog itself (blocked on mu).
		if d.inflight.Load() == 0 {
			return
		}
		d.watching.Store(true)
	}
	// The write is in flight: start was stored before it was counted.
	if left := d.wt - (time.Since(epoch) - time.Duration(d.start.Load())); left > 0 {
		d.timer.Reset(left)
		return
	}
	d.watching.Store(false)
	d.cutLocked()
}

// cutLocked fails the stream's writes; mu is held and the request is not over.
// A stream is cut once: over h2c every deadline in the past is a message to the
// connection's serve goroutine that queues an RST_STREAM for the stream, and a
// stream that is closing has been reset already.
func (d *detachedWrites) cutLocked() {
	if d.closing.Swap(true) {
		return
	}
	_ = d.rc.SetWriteDeadline(pastDeadline)
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
	d.cutLocked()
}

// stop ends the watchdog and the poke: after it neither touches the response.
func (d *detachedWrites) stop() {
	d.mu.Lock()
	d.ended = true
	if d.timer != nil {
		d.timer.Stop()
	}
	d.mu.Unlock()
	if d.stopPoke != nil {
		d.stopPoke()
	}
}

// end is called when the request's ServeHTTP is about to return, with the
// handler done. The response still has bytes to write (the end of a chunked
// body, the end of an HTTP/2 stream), and it is the last write net/http makes
// with this deadline: bound it as the others, unless the stream was cut or
// poked, when it fails.
//
// On HTTP/2 the deadline is a message to the serve goroutine, which may be
// handled after the stream is closed; the timer it starts then can reset the
// closed stream once more, WriteTimeout later. net/http's server allows that
// (a duplicate RST_STREAM, which the client ignores).
func (d *detachedWrites) end() {
	d.stop()
	if d.wt > 0 && !d.closing.Load() && !d.hijacked.Load() {
		_ = d.rc.SetWriteDeadline(time.Now().Add(d.wt))
	}
}

// abandon is end for a response that was hijacked: the connection is the
// caller's, and no deadline of ours is set on it.
func (d *detachedWrites) abandon() {
	d.hijacked.Store(true)
	d.stop()
}

// detachedWriter is the http.ResponseWriter a detached stream writes through:
// the one net/http gave, with each Write and Flush noted for the watchdog. A
// stream that has not detached writes to net/http's directly.
type detachedWriter struct {
	http.ResponseWriter
	d *detachedWrites
}

func (w *detachedWriter) Write(p []byte) (int, error) {
	w.d.begin()
	n, err := w.ResponseWriter.Write(p)
	w.d.done()
	return n, err
}

func (w *detachedWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		w.d.begin()
		f.Flush()
		w.d.done()
	}
}

// Unwrap lets http.NewResponseController reach net/http's writer.
func (w *detachedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack hands the connection over, and ends the watchdog and the poke for it.
func (w *detachedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, stream.ErrHijackNotSupported
	}
	conn, brw, err := h.Hijack()
	if err == nil {
		w.d.abandon()
	}
	return conn, brw, err
}
