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
// asynchronously via the loop's detach queue, so a small headroom of
// in-flight chunks may still arrive after pause is requested — that
// headroom is the difference between cap(ch) and highWater.
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

	// metrics
	dropped atomic.Uint64 // chunks dropped because the channel was full
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
	// Fast path: already closed → drop. This is safe now that ch is NEVER
	// closed (see closeWith): even if the close lands right after this
	// check, the send below targets an open channel and cannot panic, and
	// the <-r.done case then drops the chunk. The OLD design closed ch,
	// which made this very check a TOCTOU — the send could then panic with
	// "send on closed channel", exactly the v1.5.7 weekend-soak crash.
	if r.closed.Load() {
		return false
	}
	select {
	case r.ch <- chunk:
		// Request pause when crossing the high-water mark. Edge-triggered:
		// only signal once per crossing, even if many chunks arrive in a row.
		if r.pause != nil && len(r.ch) >= r.highWater {
			r.pausedMu.Lock()
			if !r.pausedState {
				r.pausedState = true
				r.pausedMu.Unlock()
				r.pause()
			} else {
				r.pausedMu.Unlock()
			}
		}
		return true
	case <-r.done:
		// Reader closed concurrently; drop the chunk.
		return false
	default:
		r.dropped.Add(1)
		// Should not happen with backpressure correctly wired, but if it
		// does, poison the reader so the handler sees a clean error.
		r.closeWith(ErrReadLimit)
		return false
	}
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

		// Edge-triggered resume: when depth falls below low-water, lift
		// backpressure so the engine resumes inbound reads.
		if r.resume != nil {
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

// Dropped returns the number of inbound chunks dropped due to a full
// channel. Should be 0 with backpressure correctly wired.
func (r *chanReader) Dropped() uint64 {
	return r.dropped.Load()
}
