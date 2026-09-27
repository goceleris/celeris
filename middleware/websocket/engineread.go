//line middleware/websocket/engineread.go:1:1
package websocket; import _cover_atomic_ "sync/atomic"

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
func newChanReader(capacity, highPct, lowPct int) *chanReader {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[0], 1);
	if capacity <= 0 {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[8], 1);
		capacity = 256
	}
	_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[1], 1);if highPct <= 0 || highPct > 100 {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[9], 1);
		highPct = 75
	}
	_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[2], 1);if lowPct <= 0 || lowPct >= highPct {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[10], 1);
		lowPct = 25
	}
	_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[3], 1);r := &chanReader{
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
	_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[4], 1);if r.highWater < 1 {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[11], 1);
		r.highWater = 1
	}
	_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[5], 1);if r.lowWater >= r.highWater {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[12], 1);
		r.lowWater = r.highWater - 1
	}
	_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[6], 1);if r.lowWater < 0 {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[13], 1);
		r.lowWater = 0
	}
	_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[7], 1);return r
}

// SetPauser installs the engine pause/resume callbacks. Safe to call once
// after construction; safe to call with (nil, nil) when the engine does
// not support backpressure (e.g. tests).
func (r *chanReader) SetPauser(pause, resume func()) {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[14], 1);
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
func (r *chanReader) Append(chunk []byte) bool {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[15], 1);
	// Fast path: already closed → drop. Safe because ch is NEVER closed
	// (closeWith closes `done`, not `ch`): even if the close lands right after
	// this check, the send below targets an open channel and cannot panic — a
	// chunk buffered as the reader closes is just never Read and is GC'd. No
	// <-r.done case is needed in the select (the send can neither panic nor
	// block forever — it is a non-blocking select-with-default), so Append
	// keeps the cheap single-op select. The OLD design closed ch, which made
	// this check a TOCTOU: the send could then panic with "send on closed
	// channel" — the v1.5.7 weekend-soak crash.
	if r.closed.Load() {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[18], 1);
		return false
	}
	// Ordering: once anything has spilled, every later chunk must queue
	// behind it. Append is the only producer, so an empty spill observed
	// here cannot become non-empty before the channel send below.
	_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[16], 1);if r.spillLen.Load() > 0 {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[19], 1);
		if !r.spillChunk(chunk) {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[21], 1);
			r.closeWith(ErrReadLimit)
			return false
		}
		_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[20], 1);r.requestPause()
		return true
	}

	_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[17], 1);select {
	case r.ch <- chunk:_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[22], 1);
		// Request pause when crossing the high-water mark. Edge-triggered:
		// only signal once per crossing, even if many chunks arrive in a row.
		if len(r.ch) >= r.highWater {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[26], 1);
			r.requestPause()
		}
		_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[23], 1);return true
	default:_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[24], 1);
		// Channel full: the pause was requested at highWater but the
		// engine's in-flight burst outran the headroom (celeris#484).
		// Spill rather than discard — these bytes are already off the
		// socket, so dropping them would truncate a healthy stream.
		if !r.spillChunk(chunk) {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[27], 1);
			r.closeWith(ErrReadLimit)
			return false
		}
		_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[25], 1);r.requestPause()
		return true
	}
}

// spillChunk queues chunk behind the full channel and accounts for it.
// Returns false when the spill is also full, which is the genuine
// read-limit condition: the peer has outrun both the channel and a full
// extra channel of spill. Both counters live here so they cannot diverge
// between Append's two spill paths.
func (r *chanReader) spillChunk(chunk []byte) bool {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[28], 1);
	r.spillMu.Lock()
	if r.spillMax <= 0 || len(r.spill) >= r.spillMax {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[30], 1);
		r.spillMu.Unlock()
		r.dropped.Add(1)
		return false
	}
	_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[29], 1);r.spill = append(r.spill, chunk)
	r.spillLen.Store(int64(len(r.spill)))
	r.spillMu.Unlock()
	r.spilled.Add(1)
	return true
}

// requestPause asks the engine to suspend inbound delivery, edge-triggered
// so the callback fires once per high-water crossing.
func (r *chanReader) requestPause() {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[31], 1);
	if r.pause == nil {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[34], 1);
		return
	}
	_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[32], 1);r.pausedMu.Lock()
	if r.pausedState {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[35], 1);
		r.pausedMu.Unlock()
		return
	}
	_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[33], 1);r.pausedState = true
	r.pausedMu.Unlock()
	r.pause()
}

// refillFromSpill moves spilled chunks into the channel's tail while there
// is room. Order is preserved: every spill chunk is later than every chunk
// already in ch.
func (r *chanReader) refillFromSpill() {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[36], 1);
	if r.spillLen.Load() == 0 {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[39], 1);
		return
	}
	_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[37], 1);r.spillMu.Lock()
	defer r.spillMu.Unlock()
	i := 0
	for ; i < len(r.spill); i++ {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[40], 1);
		select {
		case r.ch <- r.spill[i]:_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[41], 1);
			r.spill[i] = nil
		default:_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[42], 1);
			r.spill = r.spill[i:]
			r.spillLen.Store(int64(len(r.spill)))
			return
		}
	}
	_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[38], 1);r.spill = nil
	r.spillLen.Store(0)
}

// Read implements io.Reader. Blocks until a chunk arrives or the reader
// is closed. The bufio.Reader wrapping us calls Read in a tight loop, so
// the per-call overhead matters; this implementation has no allocations
// in the steady state.
func (r *chanReader) Read(p []byte) (int, error) {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[43], 1);
	if len(r.cur) == 0 {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[45], 1);
		// Buffered chunks were received BEFORE the close and must be
		// delivered before it (celeris#484). A peer that sent data and
		// then went away still sent that data; reporting the close while
		// chunks are queued truncates the stream mid-frame and the
		// handler sees "unexpected EOF". Measured under flood: readers
		// were closed holding a completely full channel — 256 chunks
		// discarded per connection. So try the buffer first, and only
		// report the close once it is drained.
		select {
		case chunk := <-r.ch:_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[48], 1);
			r.cur = chunk
		default:_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[49], 1);
			if r.closed.Load() {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[51], 1);
				return 0, r.closeErr()
			}
			// Block for the next chunk, waking on close via done. r.ch is
			// never closed, so a closed-channel receive can't be the wake
			// signal here.
			_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[50], 1);select {
			case chunk := <-r.ch:_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[52], 1);
				r.cur = chunk
			case <-r.done:_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[53], 1);
				// The close and a final chunk can land together; select
				// picks randomly among ready cases, so re-check the
				// buffer rather than dropping what did arrive.
				select {
				case chunk := <-r.ch:_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[54], 1);
					r.cur = chunk
				default:_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[55], 1);
					return 0, r.closeErr()
				}
			}
		}

		// Taking a chunk freed a slot: promote spilled chunks into the
		// channel's tail so len(r.ch) keeps reflecting the true buffered
		// depth the watermarks below are judged against.
		_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[46], 1);r.refillFromSpill()

		// Edge-triggered resume: when depth falls below low-water, lift
		// backpressure so the engine resumes inbound reads. Never resume
		// while chunks are still spilled — the buffer is over-full, which
		// is the opposite of the drained condition resume signals.
		_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[47], 1);if r.resume != nil && !r.hasSpill() {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[56], 1);
			r.pausedMu.Lock()
			if r.pausedState && len(r.ch) <= r.lowWater {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[57], 1);
				r.pausedState = false
				r.pausedMu.Unlock()
				r.resume()
			} else{ _cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[58], 1);{
				r.pausedMu.Unlock()
			}}
		}
	}
	_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[44], 1);n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

// closeWith marks the reader as closed and stores err to surface from
// the next Read call. Idempotent. Safe to call from any goroutine.
func (r *chanReader) closeWith(err error) {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[59], 1);
	if !r.closed.CompareAndSwap(false, true) {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[62], 1);
		return
	}
	_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[60], 1);if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[63], 1);
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
	_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[61], 1);close(r.done)
}

// closeErr returns the stored close error, or io.EOF if none was set.
func (r *chanReader) closeErr() error {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[64], 1);
	if e := r.err.Load(); e != nil {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[66], 1);
		return e.(error)
	}
	_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[65], 1);return io.EOF
}

// hasSpill reports whether any chunk is still queued behind the channel.
func (r *chanReader) hasSpill() bool {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[67], 1);
	return r.spillLen.Load() > 0
}

// Dropped returns the number of inbound chunks dropped because both the
// channel and the spill buffer were full. Non-zero means a peer outran a
// paused connection by more than twice MaxBackpressureBuffer.
func (r *chanReader) Dropped() uint64 {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[68], 1);
	return r.dropped.Load()
}

// Spilled returns the number of inbound chunks that had to queue behind a
// full channel. Non-zero is normal under a burst the pause headroom could
// not cover; it is the signal that MaxBackpressureBuffer is tight for the
// offered load, not an error.
func (r *chanReader) Spilled() uint64 {_cover_atomic_.AddUint32(&GoCover_c633_engineread.Count[69], 1);
	return r.spilled.Load()
}

var GoCover_c633_engineread = struct {
	Count     [70]uint32
	Pos       [3 * 70]uint32
	NumStmt   [70]uint16
} {
	Pos: [3 * 70]uint32{
		95, 95, 0x130002, // [0]
		98, 98, 0x230002, // [1]
		101, 101, 0x260002, // [2]
		104, 115, 0x10002, // [3]
		124, 124, 0x150002, // [4]
		127, 127, 0x1f0002, // [5]
		130, 130, 0x140002, // [6]
		133, 133, 0xa0002, // [7]
		96, 97, 0x10003, // [8]
		99, 100, 0x10003, // [9]
		102, 103, 0x10003, // [10]
		125, 126, 0x10003, // [11]
		128, 129, 0x10003, // [12]
		131, 132, 0x10003, // [13]
		140, 142, 0x10002, // [14]
		163, 163, 0x150002, // [15]
		169, 169, 0x1b0002, // [16]
		178, 178, 0x90002, // [17]
		164, 165, 0x10003, // [18]
		170, 170, 0x1b0003, // [19]
		174, 175, 0xe0003, // [20]
		171, 173, 0x10004, // [21]
		182, 182, 0x1f0003, // [22]
		185, 185, 0xe0003, // [23]
		191, 191, 0x1b0003, // [24]
		195, 196, 0xe0003, // [25]
		183, 184, 0x10004, // [26]
		192, 194, 0x10004, // [27]
		206, 207, 0x330002, // [28]
		212, 216, 0xd0002, // [29]
		208, 211, 0x10003, // [30]
		222, 222, 0x140002, // [31]
		225, 226, 0x130002, // [32]
		230, 232, 0xb0002, // [33]
		223, 224, 0x10003, // [34]
		227, 229, 0x10003, // [35]
		239, 239, 0x1c0002, // [36]
		242, 245, 0x1e0002, // [37]
		255, 256, 0x150002, // [38]
		240, 241, 0x10003, // [39]
		246, 246, 0xa0003, // [40]
		248, 248, 0x140004, // [41]
		250, 252, 0xa0004, // [42]
		264, 264, 0x150002, // [43]
		319, 321, 0xf0002, // [44]
		273, 273, 0xa0003, // [45]
		302, 303, 0x10003, // [46]
		308, 308, 0x270003, // [47]
		275, 275, 0x110004, // [48]
		277, 277, 0x170004, // [49]
		283, 283, 0xb0004, // [50]
		278, 279, 0x10005, // [51]
		285, 285, 0x120005, // [52]
		290, 290, 0xc0005, // [53]
		292, 292, 0x130006, // [54]
		294, 294, 0x1c0006, // [55]
		309, 310, 0x300004, // [56]
		311, 314, 0x10005, // [57]
		315, 316, 0x10005, // [58]
		327, 327, 0x2b0002, // [59]
		330, 330, 0x100002, // [60]
		341, 341, 0xf0002, // [61]
		328, 329, 0x10003, // [62]
		331, 332, 0x10003, // [63]
		346, 346, 0x210002, // [64]
		349, 349, 0xf0002, // [65]
		347, 348, 0x10003, // [66]
		354, 355, 0x10002, // [67]
		361, 362, 0x10002, // [68]
		369, 370, 0x10002, // [69]
	},
	NumStmt: [70]uint16{
		1, // 0
		1, // 1
		1, // 2
		2, // 3
		2, // 4
		1, // 5
		1, // 6
		1, // 7
		1, // 8
		1, // 9
		1, // 10
		1, // 11
		1, // 12
		1, // 13
		2, // 14
		1, // 15
		1, // 16
		1, // 17
		1, // 18
		1, // 19
		2, // 20
		2, // 21
		1, // 22
		1, // 23
		1, // 24
		2, // 25
		1, // 26
		2, // 27
		2, // 28
		5, // 29
		3, // 30
		1, // 31
		2, // 32
		3, // 33
		1, // 34
		2, // 35
		1, // 36
		4, // 37
		2, // 38
		1, // 39
		1, // 40
		1, // 41
		3, // 42
		1, // 43
		3, // 44
		1, // 45
		2, // 46
		2, // 47
		1, // 48
		1, // 49
		1, // 50
		1, // 51
		1, // 52
		1, // 53
		1, // 54
		1, // 55
		2, // 56
		3, // 57
		1, // 58
		1, // 59
		1, // 60
		1, // 61
		1, // 62
		1, // 63
		1, // 64
		1, // 65
		1, // 66
		1, // 67
		1, // 68
		1, // 69
	},
}

var _ = _cover_atomic_.LoadUint32
