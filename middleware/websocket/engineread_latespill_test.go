package websocket

import (
	"io"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// readResult is what one Read returned, handed back from the goroutine that
// ran it.
type readResult struct {
	b   []byte
	err error
}

// readAsync starts one handler Read of up to 1 byte in its own goroutine and
// returns the channel its result arrives on. The channel is buffered, so the
// goroutine never blocks on handing the result back.
func readAsync(r *chanReader) <-chan readResult {
	got := make(chan readResult, 1)
	go func() {
		p := make([]byte, 1)
		n, err := r.Read(p)
		got <- readResult{p[:n], err}
	}()
	return got
}

// lateSpillPauser installs engine callbacks that mirror the engine's
// recvPauseDesired (Swap-based in both engines, as in the celeris#667 and
// celeris#672 tests) and returns it.
func lateSpillPauser(r *chanReader) *atomic.Bool {
	var desired atomic.Bool
	r.SetPauser(func() { desired.Swap(true) }, func() { desired.Swap(false) })
	return &desired
}

// TestChanReaderLateSpillReachesParkedRead is the failing-first oracle for
// celeris#705, in the interleaving the issue reports.
//
// Append sends with a non-blocking select and, when the channel is full,
// spills the chunk in the select's default branch. Between the select finding
// the channel full and that branch reaching spillChunk, the handler can drain
// the whole channel and park in Read on the empty channel. spillChunk then
// queued the chunk in the spill, and Read promoted spill into the channel only
// after a successful dequeue (refillFromSpill), which a parked Read never
// makes again: the channel is empty, and requestPause has just paused the
// engine (the celeris#672 re-check does not lift a pause while chunks are
// spilled), so nothing else arrives either. The connection is wedged for
// good, with the engine paused, the channel empty and the chunk in the spill.
//
// The interleaving is laid out in order in one goroutine, with the parked
// handler in a second one:
//
//  1. the engine fills the channel through Append: the pause fires at
//     highWater and the in-flight burst fills the rest (celeris#484);
//  2. the next Append's select finds the channel full (the state is checked
//     here; the chunk it carries is the one step 4 spills);
//  3. the handler drains the channel through the real Read, and its next
//     Read parks on the empty channel;
//  4. the rest of that Append's default branch runs: spillChunk, then
//     requestPause.
//
// Only the gap between steps 2 and 4 is manufactured. The park is observed,
// not slept on: inside a synctest bubble, synctest.Wait returns once the
// handler is durably blocked, which a goroutine parked on the bubble's
// channels is.
//
// The oracle: the parked Read returns the late chunk, and the engine is not
// left paused with nothing buffered.
func TestChanReaderLateSpillReachesParkedRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newChanReader(8, 0, 0) // highWater 6, lowWater 2, spillMax 8
		// Released on every exit, so a Read still parked cannot outlive the
		// bubble; a no-op once the Read has returned.
		defer r.closeWith(io.EOF)
		desired := lateSpillPauser(r)

		// Step 1.
		for i := range cap(r.ch) {
			if !r.Append([]byte{'a' + byte(i)}) {
				t.Fatalf("harness: Append %d rejected below capacity", i)
			}
		}
		// Step 2: what the next Append's select sees.
		if len(r.ch) != cap(r.ch) || r.hasSpill() {
			t.Fatalf("harness: want a full channel and an empty spill, got depth %d spill %d",
				len(r.ch), r.spillLen.Load())
		}

		// Step 3.
		buf := make([]byte, 1)
		for i := range cap(r.ch) {
			if n, err := r.Read(buf); n != 1 || err != nil || buf[0] != 'a'+byte(i) {
				t.Fatalf("harness: drain read %d: n=%d err=%v byte=%q", i, n, err, buf[:n])
			}
		}
		got := readAsync(r)
		synctest.Wait()
		select {
		case res := <-got:
			t.Fatalf("harness: Read returned %q, %v with nothing buffered", res.b, res.err)
		default:
		}

		// Step 4.
		if !r.spillChunk([]byte{'S'}) {
			t.Fatal("harness: spillChunk rejected the late chunk")
		}
		r.requestPause()
		synctest.Wait()

		select {
		case res := <-got:
			if res.err != nil || string(res.b) != "S" {
				t.Fatalf("the parked Read returned %q, %v; want the late chunk %q", res.b, res.err, "S")
			}
		default:
			t.Fatalf("celeris#705: the handler's Read is still parked on an empty channel while "+
				"%d chunk(s) wait in the spill (depth %d, engine paused=%v). The chunk spilled after "+
				"the handler had drained the channel, and Read promotes spill only after a "+
				"successful dequeue, so the connection is wedged",
				r.spillLen.Load(), len(r.ch), desired.Load())
		}
		if desired.Load() || readerPaused(r) {
			t.Fatalf("engine left paused (engine=%v reader=%v) with nothing buffered (depth %d, spill %d)",
				desired.Load(), readerPaused(r), len(r.ch), r.spillLen.Load())
		}
	})
}

// TestChanReaderSpillPublishedAfterDrain covers the same race with the drain
// on the other side of spillChunk's own look at the channel (celeris#705):
// spillChunk still finds the channel full, and the handler drains it while
// spillChunk is queuing the chunk, before the chunk is published.
//
// spillChunk works under spillMu, but Read's per-dequeue promotion
// (refillFromSpill) reads spillLen WITHOUT the lock, so that the steady state
// never takes it. A handler that drains in that window reads spillLen == 0
// after every dequeue and promotes nothing, then finds the channel empty. The
// chunk is in the spill, and the handler's next Read must still get it,
// whether that Read comes after the publication or is already waiting for it,
// and a close in between must not overtake it (celeris#484: what was received
// before a close is delivered before the close is reported).
//
// A single goroutine cannot stop spillChunk between two of its statements, so
// each case performs spillChunk's steps itself, holding spillMu as spillChunk
// does, with the handler's real Reads in the window:
//
//	lock; the channel is full; the chunk is queued;
//	    <the handler drains the channel through Read>
//	spillLen is published; unlock
//
// and then the rest of Append: requestPause.
func TestChanReaderSpillPublishedAfterDrain(t *testing.T) {
	// window runs the steps above up to the unlock, calling during() inside
	// the window after the drain. It returns the desired-pause mirror.
	window := func(t *testing.T, r *chanReader, during func()) *atomic.Bool {
		t.Helper()
		desired := lateSpillPauser(r)
		for i := range cap(r.ch) {
			if !r.Append([]byte{'a' + byte(i)}) {
				t.Fatalf("harness: Append %d rejected below capacity", i)
			}
		}

		r.spillMu.Lock()
		select {
		case r.ch <- []byte{'S'}:
			r.spillMu.Unlock()
			t.Fatal("harness: the channel had room; nothing would spill")
		default:
		}
		r.spill = append(r.spill, []byte{'S'})

		buf := make([]byte, 1)
		for i := range cap(r.ch) {
			if n, err := r.Read(buf); n != 1 || err != nil || buf[0] != 'a'+byte(i) {
				r.spillMu.Unlock()
				t.Fatalf("harness: drain read %d: n=%d err=%v byte=%q", i, n, err, buf[:n])
			}
		}
		if during != nil {
			during()
		}

		r.spillLen.Store(int64(len(r.spill)))
		r.spillMu.Unlock()
		r.spilled.Add(1)
		r.requestPause()
		return desired
	}
	// keptPaused checks the state the window leaves when nothing has read
	// since: the pause requestPause just decided finds the chunk spilled
	// behind an empty channel, and its celeris#672 re-check must keep it. A
	// chunk is buffered, and Read lifts the pause once it has promoted the
	// spill and drained to lowWater. This is what kills the #671 review's
	// mutant M5, the re-check without its !r.hasSpill() guard, which resumes
	// here (celeris#716, item 1).
	keptPaused := func(t *testing.T, r *chanReader, desired *atomic.Bool) {
		t.Helper()
		if len(r.ch) != 0 || r.spillLen.Load() != 1 {
			t.Fatalf("harness: want an empty channel and 1 spilled chunk, got depth %d spill %d",
				len(r.ch), r.spillLen.Load())
		}
		if !desired.Load() || !readerPaused(r) {
			t.Fatalf("the pause was lifted (engine=%v reader=%v) while a chunk is still spilled behind "+
				"the channel: the stale-pause re-check must keep a pause while anything is spilled",
				desired.Load(), readerPaused(r))
		}
	}
	// oracle checks what the handler's Read after the drain returned, and
	// that the engine was not left paused with nothing buffered.
	oracle := func(t *testing.T, r *chanReader, desired *atomic.Bool, res readResult) {
		t.Helper()
		if res.err != nil || string(res.b) != "S" {
			t.Fatalf("celeris#705: the Read after the drain returned %q, %v; want the chunk spilled "+
				"while the handler drained, %q (depth %d, spill %d, engine paused=%v)",
				res.b, res.err, "S", len(r.ch), r.spillLen.Load(), desired.Load())
		}
		if desired.Load() || readerPaused(r) {
			t.Fatalf("engine left paused (engine=%v reader=%v) with nothing buffered",
				desired.Load(), readerPaused(r))
		}
	}

	// The Read comes after the publication.
	t.Run("read-after-publication", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newChanReader(8, 0, 0)
			defer r.closeWith(io.EOF)
			desired := window(t, r, nil)
			keptPaused(t, r, desired)

			got := readAsync(r)
			synctest.Wait()
			select {
			case res := <-got:
				oracle(t, r, desired, res)
			default:
				t.Fatalf("celeris#705: the handler's Read parked on an empty channel while %d chunk(s) "+
					"wait in the spill (engine paused=%v): the chunk spilled while the handler drained, "+
					"and Read promotes spill only after a successful dequeue", r.spillLen.Load(), desired.Load())
			}
		})
	})

	// The reader is closed before the handler's next Read: the chunk was
	// received before the close, so it is delivered first.
	t.Run("closed-before-read", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newChanReader(8, 0, 0)
			defer r.closeWith(io.EOF)
			desired := window(t, r, nil)
			keptPaused(t, r, desired)
			r.closeWith(io.EOF)

			got := readAsync(r)
			synctest.Wait()
			select {
			case res := <-got:
				if res.err == io.EOF {
					t.Fatalf("celeris#705/#484: Read reported the close while %d chunk(s) received before it "+
						"wait in the spill", r.spillLen.Load())
				}
				oracle(t, r, desired, res)
			default:
				t.Fatal("Read blocked on a closed reader")
			}
			if n, err := r.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Fatalf("after the late chunk: n=%d err=%v, want the close (io.EOF)", n, err)
			}
		})
	})

	// The handler's next Read is already running while the chunk is being
	// published. The Read decides whether to wait with spillMu held by the
	// publisher; a Read that decided from spillLen alone (0 here) parks on the
	// empty channel and misses the chunk. The Read is given 50 ms to reach
	// that decision; this case can only pass wrongly on a host too loaded to
	// run it in that time, never fail wrongly, because a Read that has not yet
	// decided when the chunk is published finds it either way. (No synctest
	// bubble here: a goroutine waiting for a mutex is not durably blocked.)
	t.Run("read-waiting-in-window", func(t *testing.T) {
		r := newChanReader(8, 0, 0)
		defer r.closeWith(io.EOF)
		var got <-chan readResult
		desired := window(t, r, func() {
			got = readAsync(r)
			time.Sleep(50 * time.Millisecond)
		})
		select {
		case res := <-got:
			oracle(t, r, desired, res)
		case <-time.After(10 * time.Second):
			t.Fatalf("celeris#705: the handler's Read, waiting while the chunk was published, is still "+
				"blocked 10 s later with %d chunk(s) in the spill and depth %d (engine paused=%v)",
				r.spillLen.Load(), len(r.ch), desired.Load())
		}
	})
}

// TestChanReaderPauseRecheckHoldsWhileSpilled pins the spill guard of
// requestPause's celeris#672 re-check (the `!r.hasSpill()` term): a pause
// decided while chunks are spilled behind the channel is kept, even when the
// channel alone is at lowWater. Without the guard (the #671 review's mutant
// M5, celeris#716 item 1) the re-check resumes the engine there.
//
// The spilled chunks are buffered as surely as the ones in the channel, so the
// true depth is len(r.ch) plus the spill, and a resume then re-opens the
// engine above lowWater, with the spill already holding part of the headroom
// that absorbs the engine's post-pause burst (celeris#484). Read lifts the
// pause instead, once it has promoted the spill and drained to lowWater.
//
// The state is reached with public configuration and an ordering the upgrade
// allows, using only gaps between statements:
//
//   - MaxBackpressureBuffer 2, BackpressureHighPct 100, BackpressureLowPct 50:
//     highWater 2, lowWater 1, spillMax 2;
//   - the engine delivers before the upgrade installs the callbacks
//     (SetPauser's comment: from the 101 on, the worker may be appending), so
//     the channel fills and a chunk spills with no pause possible;
//   - SetPauser runs;
//   - the next Append queues behind the spill (spillChunk), and the handler's
//     Read takes a chunk off the channel before that Append's requestPause.
//
// At the re-check the channel holds 1 chunk (lowWater) and the spill 2.
// Everything after it is the production code: the handler's Reads promote the
// spill and lift the pause at depth 1, and the stream arrives whole, in order.
func TestChanReaderPauseRecheckHoldsWhileSpilled(t *testing.T) {
	r := newChanReader(2, 100, 50)
	if r.highWater != 2 || r.lowWater != 1 || r.spillMax != 2 {
		t.Fatalf("harness: want highWater 2, lowWater 1, spillMax 2; got %d, %d, %d",
			r.highWater, r.lowWater, r.spillMax)
	}

	// Before SetPauser: the channel fills and one chunk spills.
	for _, c := range []byte("abc") {
		if !r.Append([]byte{c}) {
			t.Fatalf("harness: Append(%q) rejected", c)
		}
	}
	if len(r.ch) != 2 || r.spillLen.Load() != 1 {
		t.Fatalf("harness: want depth 2 and 1 spilled, got %d and %d", len(r.ch), r.spillLen.Load())
	}

	var desired atomic.Bool
	var pauses, resumes atomic.Uint32
	r.SetPauser(
		func() { pauses.Add(1); desired.Swap(true) },
		func() { resumes.Add(1); desired.Swap(false) },
	)

	// The next Append, up to its requestPause: spillLen is non-zero, so the
	// chunk queues behind the spill.
	if r.closed.Load() || r.spillLen.Load() == 0 {
		t.Fatal("harness: Append would not take its spill path")
	}
	if !r.spillChunk([]byte{'d'}) {
		t.Fatal("harness: spillChunk rejected 'd' below spillMax")
	}
	// The handler's Read takes its chunk off the channel (the statement Read
	// runs first; the handler's next Read does the promotion this one would
	// have done next).
	first := <-r.ch
	// The rest of the Append.
	r.requestPause()

	if len(r.ch) != r.lowWater || r.spillLen.Load() != 2 {
		t.Fatalf("harness: want depth %d (lowWater) and 2 spilled at the re-check, got %d and %d",
			r.lowWater, len(r.ch), r.spillLen.Load())
	}
	if pauses.Load() != 1 {
		t.Fatalf("harness: want the pause applied once, got %d", pauses.Load())
	}
	if !desired.Load() || !readerPaused(r) {
		t.Fatalf("the stale-pause re-check lifted the pause (engine=%v reader=%v, resumes=%d) with %d "+
			"chunk(s) buffered, %d of them spilled, against lowWater %d: it must keep a pause while "+
			"anything is spilled behind the channel",
			desired.Load(), readerPaused(r), resumes.Load(), len(r.ch)+int(r.spillLen.Load()),
			r.spillLen.Load(), r.lowWater)
	}

	// The handler goes on reading: the stream arrives whole and in order, and
	// the pause is lifted once the buffered depth is down to lowWater.
	got := append([]byte(nil), first...)
	buf := make([]byte, 1)
	for len(got) < 4 {
		n, err := r.Read(buf)
		if err != nil || n != 1 {
			t.Fatalf("read after the re-check: n=%d err=%v (got %q so far)", n, err, got)
		}
		got = append(got, buf[0])
	}
	if string(got) != "abcd" {
		t.Fatalf("stream = %q, want %q", got, "abcd")
	}
	if desired.Load() || readerPaused(r) || pauses.Load() != 1 || resumes.Load() != 1 {
		t.Fatalf("want the one pause lifted once, and nothing paused, after the drain: engine=%v "+
			"reader=%v pauses=%d resumes=%d", desired.Load(), readerPaused(r), pauses.Load(), resumes.Load())
	}
}
