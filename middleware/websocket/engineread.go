package websocket

import (
	"io"
	"sync"
	"sync/atomic"
)

// chanReader is the engine-integrated read source. The engine event loop
// calls Append with each inbound chunk; the WebSocket reader goroutine
// reads from it via the io.Reader interface.
//
// chanReader replaces the previous io.Pipe + dataCh + pump-goroutine
// pipeline with a single channel that the bufio.Reader pulls from
// directly. This eliminates one extra goroutine context switch and one
// buffer copy per inbound frame, while still providing the bufio.Reader
// with a non-blocking source.
//
// chanReader implements TCP-level backpressure via watermarks: when the
// channel depth exceeds highWater, it calls the engine's pause callback
// (which suspends inbound delivery for this connection). When the depth
// drops below lowWater, it calls resume. The engine applies pause/resume
// asynchronously via the loop's detach queue, so a headroom of in-flight
// chunks may still arrive after pause is requested.
//
// That headroom CANNOT be guaranteed to cover the burst (celeris#484). The
// pause is applied on the worker thread in drainDetachQueue, but the kernel
// has already posted its multishot recv completions into the CQ ring; the
// worker drains that whole batch into Append before the cancel takes
// effect. The in-flight bound is the engine's per-worker provided-buffer
// ring, not cap(ch)-highWater. Measured at the default 256/75% (headroom
// 64) with 96 flooding connections on one worker: up to 71 chunks arrived
// after pause was requested, and 12 connections per run overflowed.
//
// Overflowing chunks must NOT be discarded. They were already read off the
// socket and copied, so dropping them silently truncates an established
// stream — the peer did nothing wrong and the frame data is intact. Instead
// they spill into a bounded queue that Read drains ahead of new arrivals.
// The spill holds at most cap(ch) chunks, so the worst-case buffered depth
// is twice the configured MaxBackpressureBuffer and still bounded; a stream
// that outruns even that is a genuine flood and gets ErrReadLimit.
type chanReader struct {
	ch  chan []byte
	cur []byte // partially consumed current chunk
	// done is closed exactly once, by closeWith, to signal shutdown. We
	// close done — NEVER ch — so a concurrent Append can never send on a
	// closed channel. Both Append and Read select on done to observe close.
	done   chan struct{}
	closed atomic.Bool  // CAS guard so done is closed exactly once
	err    atomic.Value // error sent to the next Read after closing

	// Backpressure callbacks (set after construction by the WS middleware
	// once the engine's PauseRecv/ResumeRecv are available). May be nil.
	pause  func()
	resume func()

	// Watermarks for pause/resume. When buffered depth ≥ highWater, pause
	// is requested; when ≤ lowWater, resume is requested.
	highWater int
	lowWater  int

	// pausedMu guards pausedState. The Read goroutine and the Append
	// caller (engine event loop) both inspect/transition this state, so
	// a tiny mutex serializes them. Using a flag with atomics is
	// insufficient because we want strict edge detection.
	pausedMu    sync.Mutex
	pausedState bool

	// spill holds chunks that arrived after the channel filled, in arrival
	// order. Ordering invariant: every spill chunk is strictly later than
	// every chunk in ch, so Read drains ch first and refills ch's tail from
	// spill's head. Once spill is non-empty ALL later chunks queue behind
	// it, or the stream would be reordered.
	//
	// Append is single-producer (the owning engine worker thread) and the
	// only writer to spill; Read is the only consumer. spillMu serializes
	// the two. spillLen mirrors len(spill) so that, while chunks are flowing
	// and nothing has spilled, neither side takes that mutex: an Append
	// observing zero knows the spill stays empty for the duration of its
	// channel send, and a Read that has just dequeued knows there is nothing
	// to promote. Read takes spillMu only when it finds the channel empty,
	// before it blocks (celeris#705, see next).
	spillLen atomic.Int64
	spillMu  sync.Mutex
	spill    [][]byte
	spillMax int // max chunks held in spill; 0 disables spilling

	// metrics
	dropped atomic.Uint64 // chunks dropped because the spill limit was hit
	spilled atomic.Uint64 // chunks that had to spill past the channel
}

// newChanReader creates a chanReader with the given backpressure capacity
// and watermark percents (0-100). highPct/lowPct ≤ 0 fall back to 75/25.
// If capacity ≤ 0, the default of 256 is used.
func newChanReader(capacity, highPct, lowPct int) *chanReader {
	if capacity <= 0 {
		capacity = 256
	}
	if highPct <= 0 || highPct > 100 {
		highPct = 75
	}
	if lowPct <= 0 || lowPct >= highPct {
		lowPct = 25
	}
	r := &chanReader{
		ch:        make(chan []byte, capacity),
		done:      make(chan struct{}),
		highWater: capacity * highPct / 100,
		lowWater:  capacity * lowPct / 100,
		// Spill as many chunks again as the channel holds. The engine's
		// post-pause burst is bounded by its provided-buffer ring, which
		// is sized from connections-per-worker, not from this capacity;
		// one full extra channel of headroom absorbs it with margin
		// (measured worst case at the 256 default: 71 chunks).
		spillMax: capacity,
	}
	// Single-pass clamp: highWater must be ≥ 1, lowWater must satisfy
	// 0 < lowWater < highWater so resume always has somewhere to fire.
	// Two pathological edges to handle:
	//   capacity=1 → highWater=1, lowWater=0 → lowWater forced to 0 then
	//                clamped to highWater-1 = 0. OK, resume on empty.
	//   highPct≈lowPct → both round to the same value; lowWater needs to
	//                be strictly less than highWater for the read-side
	//                "drained" signal to fire.
	if r.highWater < 1 {
		r.highWater = 1
	}
	if r.lowWater >= r.highWater {
		r.lowWater = r.highWater - 1
	}
	if r.lowWater < 0 {
		r.lowWater = 0
	}
	return r
}

// SetPauser installs the engine pause/resume callbacks. Safe to call once
// after construction; safe to call with (nil, nil) when the engine does
// not support backpressure (e.g. tests).
//
// The callbacks are written under pausedMu, and requestPause reads them only
// under pausedMu, because nothing else orders the two: the upgrade calls
// SetPauser after Detach and after the 101 is written, and from then on the
// engine worker may already be appending (on an async-mode connection the
// upgrade does not run on the worker). Read's reads of r.resume need no lock:
// they run on the handler goroutine, which the upgrade starts after this
// returns.
func (r *chanReader) SetPauser(pause, resume func()) {
	r.pausedMu.Lock()
	defer r.pausedMu.Unlock()
	r.pause = pause
	r.resume = resume
}

// Append delivers an inbound chunk to the reader. Called by the engine
// event loop callback — must not block. Returns false if the chunk was
// dropped because the channel is full (which should be impossible when
// pause/resume are wired correctly, since the engine would have paused
// reads before the channel filled). On drop, the connection is poisoned
// with ErrReadLimit so the next Read returns the error.
//
// The caller is responsible for COPYING the chunk before calling Append
// (the engine reuses its read buffer after the callback returns).
func (r *chanReader) Append(chunk []byte) bool {
	// Fast path: already closed → drop. Safe because ch is NEVER closed
	// (closeWith closes `done`, not `ch`): even if the close lands right after
	// this check, the send below targets an open channel and cannot panic — a
	// chunk buffered as the reader closes is just never Read and is GC'd. No
	// <-r.done case is needed in the select (the send can neither panic nor
	// block forever — it is a non-blocking select-with-default), so Append
	// keeps the cheap single-op select. The OLD design closed ch, which made
	// this check a TOCTOU: the send could then panic with "send on closed
	// channel" — the v1.5.7 weekend-soak crash.
	if r.closed.Load() {
		return false
	}
	// Ordering: once anything has spilled, every later chunk must queue
	// behind it. Append is the only producer, so an empty spill observed
	// here cannot become non-empty before the channel send below.
	if r.spillLen.Load() > 0 {
		return r.queueBehind(chunk)
	}

	select {
	case r.ch <- chunk:
		// Request pause when crossing the high-water mark. Edge-triggered:
		// only signal once per crossing, even if many chunks arrive in a row.
		if len(r.ch) >= r.highWater {
			r.requestPause()
		}
		return true
	default:
		// Channel full: the pause was requested at highWater but the
		// engine's in-flight burst outran the headroom (celeris#484).
		// Spill rather than discard — these bytes are already off the
		// socket, so dropping them would truncate a healthy stream.
		return r.queueBehind(chunk)
	}
}

// queueBehind is Append for a chunk that could not simply take the channel's
// tail: the channel was full, or chunks were already spilled. It queues the
// chunk with spillChunk and poisons the reader with ErrReadLimit when the
// spill is full too. It requests the pause when the chunk spilled, and
// otherwise (spillChunk's retry put it in the channel, which the handler had
// drained in the meantime) only when the channel is at highWater, exactly as
// Append's channel path does: a chunk that sits below highWater in a drained
// channel is no reason to pause the engine.
func (r *chanReader) queueBehind(chunk []byte) bool {
	spilled, ok := r.spillChunk(chunk)
	if !ok {
		r.closeWith(ErrReadLimit)
		return false
	}
	if spilled || len(r.ch) >= r.highWater {
		r.requestPause()
	}
	return true
}

// spillChunk queues chunk behind everything already buffered and accounts
// for it: into the channel when nothing is spilled and the channel has room,
// otherwise at the tail of the spill. spilled reports which. ok is false when
// the spill is also full, which is the genuine read-limit condition: the peer
// has outrun both the channel and a full extra channel of spill. Both counters
// live here so they cannot diverge between Append's two spill paths.
//
// The channel is tried again, under spillMu, because Append's select found it
// full some time before this runs (celeris#705). In between, the handler may
// have drained it and parked in Read on the empty channel, and a Read parked
// there never promotes spill: refillFromSpill runs only after a dequeue. A
// chunk put in the spill then was stranded for good, since requestPause keeps
// the engine paused while anything is spilled. The retry and next's
// promoteSpill both run under spillMu, so whichever runs second sees what the
// first did: either this send finds the room the handler made, or the
// handler, before it blocks, finds the chunk in the spill.
func (r *chanReader) spillChunk(chunk []byte) (spilled, ok bool) {
	r.spillMu.Lock()
	if len(r.spill) == 0 {
		// Nothing is spilled, so every buffered chunk is in the channel and
		// the chunk can take the channel's tail without reordering the stream.
		select {
		case r.ch <- chunk:
			r.spillMu.Unlock()
			return false, true
		default:
		}
	}
	if r.spillMax <= 0 || len(r.spill) >= r.spillMax {
		r.spillMu.Unlock()
		r.dropped.Add(1)
		return false, false
	}
	r.spill = append(r.spill, chunk)
	r.spillLen.Store(int64(len(r.spill)))
	r.spillMu.Unlock()
	r.spilled.Add(1)
	return true, true
}

// requestPause asks the engine to suspend inbound delivery, edge-triggered
// so the callback fires once per high-water crossing.
//
// The engine callback is invoked WITH pausedMu held, and that is the point
// (celeris#667). Deciding under the lock but applying outside it let the
// appending goroutine and the draining goroutine reach the engine in the
// OPPOSITE order from the one in which they decided. The engine's closures
// are Swap-based and ignore the previous value — the early return skips only
// the wakeup, never the state write (the PauseRecv/ResumeRecv closures the
// engines install on detach, in internal/engine/iouring/worker.go and
// internal/engine/epoll/loop.go) — so whichever callback arrives last wins outright
// and nothing reconciles. A resume could therefore be applied before
// the pause it was meant to cancel, leaving the engine's recv paused while
// this reader believed it was running. Nothing re-evaluates after that, so
// the connection stopped delivering inbound data for the rest of its life.
// Holding the lock across the callback makes the order the engine observes
// equal to the order of pausedState transitions.
//
// LOCK ORDER. Under pausedMu the callbacks take two locks, one after the
// other and never nested: the engine's detachQMu, then, only when their
// append took the detach queue from empty to non-empty and after detachQMu is
// released, the READ lock of the loop's wakefd.WakeFD, in Signal
// (celeris#666). So the edges are pausedMu -> detachQMu and
// pausedMu -> WakeFD.mu (read). No cycle can pass through pausedMu, whatever
// a caller holds when it takes it, because neither lock ever waits on
// anything that could lead back:
//
//   - every detachQMu critical section in both engines is a queue append or
//     swap plus an atomic store, with no call out of the engine.
//   - WakeFD's read side, Signal, holds the lock across one write(2) on a
//     descriptor New and Set make O_NONBLOCK, so a reader never waits.
//   - WakeFD's writers, Set and Close, run only on the loop's own thread (at
//     start, when epoll creates its eventfd lazily, at shutdown, and when an
//     io_uring worker fails to start), are never reached from these
//     callbacks, and hold the lock only across fcntl, close(2) and atomic
//     stores. A writer therefore never waits on pausedMu, holding the write
//     lock or queued for it. sync.RWMutex does queue a new reader behind a
//     waiting writer, so a callback's Signal can wait for a Close, but that
//     Close waits only for the Signals already inside their write(2).
//   - pausedMu is an unexported field of an unexported type, acquired only in
//     this file (here, resumeIfDrained and SetPauser), and the engine packages
//     do not import middleware/websocket.
//
// TestChanReaderWakeFDWritersNeverWaitOnPausedMu forces the interleavings
// where a cycle would show, including a callback queued behind a waiting
// Close.
func (r *chanReader) requestPause() {
	r.pausedMu.Lock()
	// Deferred, not a plain Unlock after the callbacks: a callback that
	// panics would skip that Unlock, and a caller that recovers the panic
	// would leave pausedMu held for good — blocking every later resume check
	// in Read and the next high-water crossing here, on the engine worker
	// thread.
	defer r.pausedMu.Unlock()
	// r.pause (and r.resume below) are read under pausedMu: SetPauser may be
	// running on the upgrade goroutine while this runs on the engine worker.
	if r.pause == nil || r.pausedState {
		return
	}
	r.pausedState = true
	// Applied under pausedMu — see the lock-order note above.
	r.pause()
	// celeris#672: the pause above was decided from a depth SNAPSHOT taken in
	// Append, before this function took pausedMu. If the handler drained to
	// at-or-below lowWater in between, every resume check it made saw
	// pausedState == false and did nothing, so the pause is already stale.
	// When the drain reached empty, nothing can ever lift it: Read
	// re-evaluates the resume only after a successful dequeue, the engine is
	// paused so it delivers nothing, and the handler blocks in Read for the
	// rest of the connection's life. Re-check the watermark here, under the
	// same lock that applied the pause, and lift it at once if it is stale.
	//
	// Any dequeue that happens after this check is followed by a resume check
	// in Read under pausedMu, which now sees pausedState == true, so the
	// pause cannot go stale again once this critical section ends. The spill
	// guard matches Read's: never resume while chunks are still queued behind
	// the channel. Read promotes them (before it blocks, if it must;
	// celeris#705) and lifts the pause once it has drained to lowWater.
	if r.resume != nil && len(r.ch) <= r.lowWater && !r.hasSpill() {
		r.pausedState = false
		r.resume()
	}
}

// resumeIfDrained is Read's edge-triggered resume: once the depth has fallen
// to lowWater it lifts the pause, deciding and applying under pausedMu for
// the reason and in the lock order documented on requestPause
// (celeris#667). The unlock is deferred for the same reason as there: a
// resume callback that panics must not leave pausedMu held.
func (r *chanReader) resumeIfDrained() {
	r.pausedMu.Lock()
	defer r.pausedMu.Unlock()
	if r.pausedState && len(r.ch) <= r.lowWater {
		r.pausedState = false
		r.resume()
	}
}

// refillFromSpill moves spilled chunks into the channel's tail while there
// is room, after a dequeue. It skips the lock when spillLen reads zero, so
// it can miss a chunk that spillChunk is still queuing; next's promoteSpill
// is the check that cannot.
func (r *chanReader) refillFromSpill() {
	if r.spillLen.Load() == 0 {
		return
	}
	r.spillMu.Lock()
	defer r.spillMu.Unlock()
	r.promoteLocked()
}

// promoteSpill is refillFromSpill without the lock-free shortcut: it always
// takes spillMu, so it sees any chunk spillChunk has queued. It reports
// whether it moved any chunk into the channel.
func (r *chanReader) promoteSpill() bool {
	r.spillMu.Lock()
	defer r.spillMu.Unlock()
	return r.promoteLocked()
}

// promoteLocked moves spilled chunks into the channel's tail while there is
// room, and reports whether it moved any. Order is preserved: every spill
// chunk is later than every chunk already in ch. spillMu must be held.
func (r *chanReader) promoteLocked() bool {
	n := 0
promote:
	for ; n < len(r.spill); n++ {
		select {
		case r.ch <- r.spill[n]:
			r.spill[n] = nil
		default:
			break promote
		}
	}
	if n == 0 {
		return false
	}
	if n == len(r.spill) {
		r.spill = nil
	} else {
		r.spill = r.spill[n:]
	}
	r.spillLen.Store(int64(len(r.spill)))
	return true
}

// next returns the next chunk in stream order. It blocks until one arrives,
// and reports the close only once nothing buffered is left to deliver.
func (r *chanReader) next() ([]byte, error) {
	for {
		// The close flag is read FIRST, before the buffers are looked at, and
		// the close is reported below only if it was already set here. The
		// engine appends and closes on one thread: its worker calls Append
		// for each chunk of a batch and then, on the peer's FIN or an error,
		// the error handler that calls closeWith. So a close seen here comes
		// after every chunk the engine appended before it, and the checks
		// below find each of them, in the channel or in the spill. Read after
		// those checks instead, the flag could come from a close that landed
		// once they had found nothing, and the chunks appended just before
		// that close would be dropped for an EOF (celeris#484's truncation).
		// The window is real: promoteSpill can wait for spillMu while
		// spillChunk's retry puts a chunk in the channel, and the worker can
		// finish its batch and close before this goroutine runs again.
		closed := r.closed.Load()
		select {
		case chunk := <-r.ch:
			return chunk, nil
		default:
		}
		// The channel is empty, but chunks can still be spilled behind it
		// (celeris#705): refillFromSpill's lock-free check after the last
		// dequeue may have run while spillChunk was queuing one. Blocking on
		// the channel now would strand them, since only a dequeue promotes
		// spill and none can come. So promote under spillMu first. This is
		// the only place a Read takes spillMu with nothing spilled, and only
		// once it has run out of chunks.
		if r.promoteSpill() {
			continue
		}
		// Buffered chunks were received BEFORE the close and must be
		// delivered before it (celeris#484). A peer that sent data and
		// then went away still sent that data; reporting the close while
		// chunks are queued truncates the stream mid-frame and the
		// handler sees "unexpected EOF". Measured under flood: readers
		// were closed holding a completely full channel — 256 chunks
		// discarded per connection. So the channel and the spill are
		// drained first, and the close is reported only after, and only
		// the close read before they were checked (see above).
		if closed {
			return nil, r.closeErr()
		}
		// Block for the next chunk, waking on close via done. r.ch is
		// never closed, so a closed-channel receive can't be the wake
		// signal here.
		select {
		case chunk := <-r.ch:
			return chunk, nil
		case <-r.done:
			// The close landed after the flag was read above, possibly
			// together with a final chunk (select picks randomly among
			// ready cases), so loop: the flag is read again, and the
			// channel and the spill are checked again, before the close
			// is reported.
		}
	}
}

// Read implements io.Reader. Blocks until a chunk arrives or the reader
// is closed. The bufio.Reader wrapping us calls Read in a tight loop, so
// the per-call overhead matters; this implementation has no allocations
// in the steady state.
func (r *chanReader) Read(p []byte) (int, error) {
	if len(r.cur) == 0 {
		chunk, err := r.next()
		if err != nil {
			return 0, err
		}
		r.cur = chunk

		// Taking a chunk freed a slot: promote spilled chunks into the
		// channel's tail so len(r.ch) keeps reflecting the true buffered
		// depth the watermarks below are judged against.
		r.refillFromSpill()

		// Edge-triggered resume: when depth falls below low-water, lift
		// backpressure so the engine resumes inbound reads. Never resume
		// while chunks are still spilled — the buffer is over-full, which
		// is the opposite of the drained condition resume signals.
		if r.resume != nil && !r.hasSpill() {
			r.resumeIfDrained()
		}
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

// closeWith marks the reader as closed and stores err to surface from
// the next Read call. Idempotent. Safe to call from any goroutine.
func (r *chanReader) closeWith(err error) {
	if !r.closed.CompareAndSwap(false, true) {
		return
	}
	if err != nil {
		r.err.Store(err)
	}
	// Close done — NOT ch — to wake any blocked Read and to signal any
	// in-flight Append to drop its chunk. The CAS above guarantees exactly
	// one closer, so this close is never doubled. We deliberately never
	// close ch: a concurrent Append may still be selecting on "r.ch <-
	// chunk", and closing ch under it would panic ("send on closed
	// channel") — the very race this reader must not have. err is stored
	// before the close so a Read woken by done observes it (the close is a
	// happens-before edge).
	close(r.done)
}

// closeErr returns the stored close error, or io.EOF if none was set.
func (r *chanReader) closeErr() error {
	if e := r.err.Load(); e != nil {
		return e.(error)
	}
	return io.EOF
}

// hasSpill reports whether any chunk is still queued behind the channel.
func (r *chanReader) hasSpill() bool {
	return r.spillLen.Load() > 0
}

// Dropped returns the number of inbound chunks dropped because both the
// channel and the spill buffer were full. Non-zero means a peer outran a
// paused connection by more than twice MaxBackpressureBuffer.
func (r *chanReader) Dropped() uint64 {
	return r.dropped.Load()
}

// Spilled returns the number of inbound chunks that had to queue behind a
// full channel. Non-zero is normal under a burst the pause headroom could
// not cover; it is the signal that MaxBackpressureBuffer is tight for the
// offered load, not an error.
func (r *chanReader) Spilled() uint64 {
	return r.spilled.Load()
}
