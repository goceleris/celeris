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
	// the two. spillLen mirrors len(spill) so the steady-state hot path —
	// nothing has ever spilled — never takes that mutex at all, and so an
	// Append observing zero knows the spill stays empty for the duration
	// of its channel send.
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
func (r *chanReader) SetPauser(pause, resume func()) {
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
		if !r.spillChunk(chunk) {
			r.closeWith(ErrReadLimit)
			return false
		}
		r.requestPause()
		return true
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
		if !r.spillChunk(chunk) {
			r.closeWith(ErrReadLimit)
			return false
		}
		r.requestPause()
		return true
	}
}

// spillChunk queues chunk behind the full channel and accounts for it.
// Returns false when the spill is also full, which is the genuine
// read-limit condition: the peer has outrun both the channel and a full
// extra channel of spill. Both counters live here so they cannot diverge
// between Append's two spill paths.
func (r *chanReader) spillChunk(chunk []byte) bool {
	r.spillMu.Lock()
	if r.spillMax <= 0 || len(r.spill) >= r.spillMax {
		r.spillMu.Unlock()
		r.dropped.Add(1)
		return false
	}
	r.spill = append(r.spill, chunk)
	r.spillLen.Store(int64(len(r.spill)))
	r.spillMu.Unlock()
	r.spilled.Add(1)
	return true
}

// requestPause asks the engine to suspend inbound delivery, edge-triggered
// so the callback fires once per high-water crossing.
func (r *chanReader) requestPause() {
	if r.pause == nil {
		return
	}
	r.pausedMu.Lock()
	if r.pausedState {
		r.pausedMu.Unlock()
		return
	}
	r.pausedState = true
	r.pausedMu.Unlock()
	r.pause()
}

// refillFromSpill moves spilled chunks into the channel's tail while there
// is room. Order is preserved: every spill chunk is later than every chunk
// already in ch.
func (r *chanReader) refillFromSpill() {
	if r.spillLen.Load() == 0 {
		return
	}
	r.spillMu.Lock()
	defer r.spillMu.Unlock()
	i := 0
	for ; i < len(r.spill); i++ {
		select {
		case r.ch <- r.spill[i]:
			r.spill[i] = nil
		default:
			r.spill = r.spill[i:]
			r.spillLen.Store(int64(len(r.spill)))
			return
		}
	}
	r.spill = nil
	r.spillLen.Store(0)
}

// Read implements io.Reader. Blocks until a chunk arrives or the reader
// is closed. The bufio.Reader wrapping us calls Read in a tight loop, so
// the per-call overhead matters; this implementation has no allocations
// in the steady state.
func (r *chanReader) Read(p []byte) (int, error) {
	if len(r.cur) == 0 {
		if r.closed.Load() {
			return 0, r.closeErr()
		}
		// Block for the next chunk, waking on close via done. r.ch is never
		// closed, so a closed-channel receive can't be the wake signal here.
		select {
		case chunk := <-r.ch:
			r.cur = chunk
		case <-r.done:
			return 0, r.closeErr()
		}

		// Taking a chunk freed a slot: promote spilled chunks into the
		// channel's tail so len(r.ch) keeps reflecting the true buffered
		// depth the watermarks below are judged against.
		r.refillFromSpill()

		// Edge-triggered resume: when depth falls below low-water, lift
		// backpressure so the engine resumes inbound reads. Never resume
		// while chunks are still spilled — the buffer is over-full, which
		// is the opposite of the drained condition resume signals.
		if r.resume != nil && !r.hasSpill() {
			r.pausedMu.Lock()
			if r.pausedState && len(r.ch) <= r.lowWater {
				r.pausedState = false
				r.pausedMu.Unlock()
				r.resume()
			} else {
				r.pausedMu.Unlock()
			}
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
