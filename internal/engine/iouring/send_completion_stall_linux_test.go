//go:build linux

package iouring

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#750, the fifth worker-thread site of the celeris#704 class: with
// AsyncHandlers, runAsyncHandler holds cs.detachMu across ProcessH1, i.e. for
// as long as the user handler runs, and handleSend applied every ring SEND
// completion of a conn that has a detachMu under a blocking Lock (the
// notification, F_MORE, SEND_ZC-fallback and error branches, and all of
// completeSend). A SEND completing while the conn's next handler ran parked
// the LockOSThread'd worker, and every connection of its ring, until the
// handler returned. The common shape is a pipelining client: the rest of a
// large response goes out as a ring SEND, the next request's handler starts,
// and then the client reads.
//
// The unit arms hold detachMu the way a running handler does, deliver the
// completion, and ask handleSend to return while the lock is held. The
// completion must then be applied, in order, when the dispatch goroutine
// hands the conn back, or when the conn is closed. The end-to-end arm is the
// issue's measurement: a fast keep-alive conn on the same worker must not
// carry the handler's duration.

// sendCQE750 is a SEND completion of rig's conn: res bytes (or -errno), with
// the CQE flags of a plain SEND (0), a SEND_ZC's first completion (F_MORE) or
// its notification (F_NOTIF).
func sendCQE750(rig *stallRig704, res int32, flags uint32) *completionEntry {
	return &completionEntry{UserData: encodeUserDataGen(udSend, rig.local, rig.cs.generation), Res: res, Flags: flags}
}

// inFlight750 puts a ring SEND of tail on rig's conn, as flushSend leaves it.
func inFlight750(rig *stallRig704, tail string, zc bool) {
	cs := rig.cs
	cs.sendBuf = append(cs.sendBuf[:0], tail...)
	cs.sending = true
	cs.sendIsZC = zc
}

// handBack750 runs the REAL dispatch loop after the handler returned: its loop
// top hands the conn back (relinkOwed), and it parks. Then the worker drains
// the detach queue, which is where the held completions are applied.
func handBack750(t *testing.T, rig *stallRig704) {
	t.Helper()
	w := rig.w
	w.asyncWG.Add(1)
	go w.runAsyncHandler(rig.cs)
	for dl := time.Now().Add(5 * time.Second); w.detachQPending.Load() == 0; {
		if time.Now().After(dl) {
			t.Fatal("the dispatch goroutine parked without handing the conn back")
		}
		time.Sleep(time.Millisecond)
	}
	w.drainDetachQueue()
}

// endDispatch750 ends the rig's dispatch goroutine, if any, and waits for it.
func endDispatch750(rig *stallRig704) {
	if rig.w.conns[rig.local] == rig.cs {
		rig.w.closeConn(rig.local)
	}
	rig.w.asyncWG.Wait()
}

func sqeOps750(sqes []sqeRec) []uint8 {
	var ops []uint8
	for _, s := range sqes {
		ops = append(ops, s.op)
	}
	return ops
}

// TestIouringSendCompletionDoesNotWaitForARunningAsyncHandler: each kind of
// SEND completion, delivered while the conn's dispatch goroutine holds
// detachMu across a handler, must not park the worker, and must be applied
// exactly as it would have been, in order, at the goroutine's hand-back.
func TestIouringSendCompletionDoesNotWaitForARunningAsyncHandler(t *testing.T) {
	const tail = "the rest of response 1"
	const next = "response 2, written by the running handler"
	type step struct {
		res   int32
		flags uint32
	}
	for _, tc := range []struct {
		name  string
		zc    bool
		cqes  []step
		check func(t *testing.T, rig *stallRig704, placed []sqeRec)
	}{
		{
			// The SEND finished: the handler's response goes out next.
			name: "send", cqes: []step{{int32(len(tail)), 0}},
			check: func(t *testing.T, rig *stallRig704, placed []sqeRec) {
				cs := rig.cs
				if len(placed) != 1 || placed[0].op != opSEND || !cs.sending || string(cs.sendBuf) != next || len(cs.writeBuf) != 0 {
					t.Errorf("after the hand-back: placed %v sending=%v sendBuf=%q writeBuf=%q; want one SEND of %q",
						sqeOps750(placed), cs.sending, cs.sendBuf, cs.writeBuf, next)
				}
			},
		},
		{
			// A short SEND: its remainder is re-sent before the handler's bytes.
			name: "partial", cqes: []step{{5, 0}},
			check: func(t *testing.T, rig *stallRig704, placed []sqeRec) {
				cs := rig.cs
				if len(placed) != 1 || placed[0].op != opSEND || !cs.sending || string(cs.sendBuf) != tail[5:] ||
					string(cs.writeBuf) != next {
					t.Errorf("after the hand-back: placed %v sending=%v sendBuf=%q writeBuf=%q; want a SEND of the "+
						"remainder %q with %q still queued", sqeOps750(placed), cs.sending, cs.sendBuf, cs.writeBuf,
						tail[5:], next)
				}
			},
		},
		{
			// A SEND_ZC: its first completion and its notification both arrive
			// while the handler runs; they are applied in that order.
			name: "zc", zc: true, cqes: []step{{int32(len(tail)), cqeFMore}, {0, cqeFNotif}},
			check: func(t *testing.T, rig *stallRig704, placed []sqeRec) {
				cs := rig.cs
				if cs.zcNotifPending || len(placed) != 1 || placed[0].op != opSEND || string(cs.sendBuf) != next {
					t.Errorf("after the hand-back: zcNotifPending=%v placed %v sendBuf=%q; want the notification "+
						"applied and one SEND of %q", cs.zcNotifPending, sqeOps750(placed), cs.sendBuf, next)
				}
			},
		},
		{
			// The peer reset: the error is delivered, once, and the conn closed.
			name: "error", cqes: []step{{-int32(unix.ECONNRESET), 0}},
			check: func(t *testing.T, rig *stallRig704, _ []sqeRec) {
				rig.expectClosedOnce(t)
				want := errIORingSend(-int32(unix.ECONNRESET)).Error()
				if len(rig.notified) != 1 || rig.notified[0].Error() != want {
					t.Errorf("OnError calls = %v, want exactly one %q", rig.notified, want)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newStallRig704(t)
			w, cs := rig.w, rig.cs
			inFlight750(rig, tail, tc.zc)
			release := holdAsHandler704(t, cs, true)
			cs.writeBuf = append(cs.writeBuf, next...) // the handler writes, under the lock it holds

			for i, s := range tc.cqes {
				c := sendCQE750(rig, s.res, s.flags)
				if !returnsWhileHeld704(t, release, func() { w.handleSend(c, rig.local, time.Now().UnixNano()) }) {
					t.Fatalf("celeris#750: send completion %d of %d (res=%d flags=%#x) waited %v on cs.detachMu held "+
						"by a running async handler; the worker, and every connection of its ring, is parked until "+
						"the handler returns", i+1, len(tc.cqes), s.res, s.flags, stallWait704)
				}
			}
			t.Logf("celeris750 HELD arm=%s held=%d sending=%v relinkOwed=%v", tc.name, heldSends750(cs), cs.sending,
				relinkOwed704(cs))
			rig.expectWhole(t)
			if !cs.sending || string(cs.sendBuf) != tail || len(rig.notified) != 0 {
				t.Fatalf("the completion was applied under a running handler: sending=%v sendBuf=%q OnError=%v",
					cs.sending, cs.sendBuf, rig.notified)
			}
			if !relinkOwed704(cs) {
				t.Fatalf("the completion was held without a hand-back owed: it would never be applied")
			}

			release()
			handBack750(t, rig)
			placed := takeSQEs(w.ring)
			if heldSends750(cs) != 0 {
				t.Errorf("%d completion(s) still held after the hand-back", heldSends750(cs))
			}
			tc.check(t, rig, placed)
			endDispatch750(rig)
		})
	}
}

// TestIouringSendCompletionsAreAppliedInOrder: a SEND_ZC's first completion
// arrives while the handler runs and is held; its notification arrives after
// the handler returned, with the lock free, before the hand-back is drained.
// Applying the notification first completed the send against a stale byte
// count and left the first completion to set zcNotifPending for a
// notification that had already come: a conn stuck sending (the celeris#519
// class). The notification waits behind the held completion.
func TestIouringSendCompletionsAreAppliedInOrder(t *testing.T) {
	const tail = "the rest of response 1"
	rig := newStallRig704(t)
	w, cs := rig.w, rig.cs
	inFlight750(rig, tail, true)
	release := holdAsHandler704(t, cs, true)
	first := sendCQE750(rig, int32(len(tail)), cqeFMore)
	if !returnsWhileHeld704(t, release, func() { w.handleSend(first, rig.local, time.Now().UnixNano()) }) {
		t.Fatalf("celeris#750: the SEND_ZC first completion waited %v on a running handler's detachMu", stallWait704)
	}
	release()
	w.handleSend(sendCQE750(rig, 0, cqeFNotif), rig.local, time.Now().UnixNano())
	t.Logf("celeris750 ORDER held=%d zcNotifPending=%v sending=%v", heldSends750(cs), cs.zcNotifPending, cs.sending)
	handBack750(t, rig)
	if cs.sending || cs.zcNotifPending || len(cs.sendBuf) != 0 || heldSends750(cs) != 0 {
		t.Errorf("after the hand-back: sending=%v zcNotifPending=%v sendBuf=%q held=%d; want the send complete "+
			"(the notification applied after its first completion)", cs.sending, cs.zcNotifPending, cs.sendBuf,
			heldSends750(cs))
	}
	endDispatch750(rig)
}

// TestIouringCloseAppliesAHeldSendCompletion: a close that runs after the
// handler returned, before the goroutine's hand-back is drained (the timeout
// sweep, a recv FIN), must not defer itself behind cs.sending for a SEND
// whose completion has already arrived and is held: it applies the
// completion, and closes now, once. Arm error: the held completion is itself
// a failure, which closes the conn as it is applied (OnError once), and the
// close that applied it must not run a second teardown.
func TestIouringCloseAppliesAHeldSendCompletion(t *testing.T) {
	const tail = "the rest of response 1"
	for _, tc := range []struct {
		name string
		res  int32
	}{
		{"send", int32(len(tail))},
		{"error", -int32(unix.ECONNRESET)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newStallRig704(t)
			w, cs := rig.w, rig.cs
			inFlight750(rig, tail, false)
			release := holdAsHandler704(t, cs, true)
			c := sendCQE750(rig, tc.res, 0)
			if !returnsWhileHeld704(t, release, func() { w.handleSend(c, rig.local, time.Now().UnixNano()) }) {
				t.Fatalf("celeris#750: the send completion waited %v on a running handler's detachMu", stallWait704)
			}
			release()
			// The goroutine returns from its handler to its park (no hand-back drained).
			cs.asyncInMu.Lock()
			setParked704(cs, true)
			cs.asyncInMu.Unlock()

			w.closeConn(rig.local)
			t.Logf("celeris750 CLOSE arm=%s held=%d closing=%v slot_owned=%v OnError=%d", tc.name, heldSends750(cs),
				cs.closing, w.conns[rig.local] == cs, len(rig.notified))
			rig.expectClosedOnce(t)
			if heldSends750(cs) != 0 {
				t.Errorf("%d completion(s) still held after the close", heldSends750(cs))
			}
			if tc.res < 0 {
				want := errIORingSend(tc.res).Error()
				if len(rig.notified) != 1 || rig.notified[0].Error() != want {
					t.Errorf("OnError calls = %v, want exactly one %q", rig.notified, want)
				}
			}
			cs.asyncInMu.Lock()
			cs.asyncRun = false
			cs.asyncInMu.Unlock()
		})
	}
}

// TestIouringHeldSendCompletionDoesNotKeepTheRingPolling: while a completion
// is held, cs.sending stays set until the goroutine's hand-back, and a conn on
// the dirty list makes the worker wait with a zero timeout. Left listed, the
// held conn kept the ring polling for as long as the handler ran, where the
// blocking Lock had parked it (found in review of PR #801). The pass must
// give the conn up, as the celeris#704 give-up does, and the hand-back must
// list it again, with nothing the handler wrote lost.
//
// Arm held: the pass submitted the SEND of the rest of a response (the conn is
// listed while it is in flight), and it completes under the next handler.
// Arm held_again: the goroutine hands the conn back at the top of its loop and
// goes straight into the next pipelined request's handler, before the worker
// drains the hand-back; the drain's entry holds the completion again, and then
// lists the conn, as every entry does.
func TestIouringHeldSendCompletionDoesNotKeepTheRingPolling(t *testing.T) {
	const tail = "the rest of response 1"
	const next = "response 2, written by the running handler"
	for _, again := range []bool{false, true} {
		name := "held"
		if again {
			name = "held_again"
		}
		t.Run(name, func(t *testing.T) {
			rig := newStallRig704(t)
			w, cs := rig.w, rig.cs
			// The goroutine's direct write was short: the rest is queued, the
			// drain listed the conn, and the pass submits the SEND.
			cs.writeBuf = append(cs.writeBuf[:0], tail...)
			w.markDirty(cs)
			w.flushDirty()
			if placed := takeSQEs(w.ring); len(placed) != 1 || placed[0].op != opSEND || !cs.sending || !cs.dirty {
				t.Fatalf("apparatus: placed %v sending=%v dirty=%v; want the SEND in flight with the conn listed",
					sqeOps750(placed), cs.sending, cs.dirty)
			}
			release := holdAsHandler704(t, cs, true)
			cs.writeBuf = append(cs.writeBuf, next...) // the handler writes, under the lock it holds
			c := sendCQE750(rig, int32(len(tail)), 0)
			if !returnsWhileHeld704(t, release, func() { w.handleSend(c, rig.local, time.Now().UnixNano()) }) {
				t.Fatalf("celeris#750: the send completion waited %v on a running handler's detachMu", stallWait704)
			}
			if again {
				// The handler returns; the loop top hands the conn back, and the
				// next handler takes detachMu before the worker drains it.
				release()
				cs.asyncInMu.Lock()
				cs.relinkOwed = false
				w.enqueueDetach(cs)
				cs.asyncInMu.Unlock()
				release = holdAsHandler704(t, cs, true)
			}
			// The worker's per-iteration passes while the handler runs.
			listed := 0
			for range 3 {
				w.drainDetachQueue()
				w.flushDirty()
				if cs.dirty || w.dirtyHead != nil {
					listed++
				}
			}
			t.Logf("celeris750 HELDLIST arm=%s held=%d sending=%v relinkOwed=%v dirty=%v baseTimeout=%v listed_passes=%d/3",
				name, heldSends750(cs), cs.sending, relinkOwed704(cs), cs.dirty, w.baseTimeout(), listed)
			if heldSends750(cs) != 1 || !cs.sending || !relinkOwed704(cs) {
				t.Fatalf("apparatus: held=%d sending=%v relinkOwed=%v; want the completion held, with a hand-back owed",
					heldSends750(cs), cs.sending, relinkOwed704(cs))
			}
			if listed != 0 {
				t.Errorf("with a completion held, the conn stayed on the dirty list after %d of 3 passes: the "+
					"worker waits with a zero timeout, a spin, for as long as the handler runs", listed)
			}

			// The handler returns; the REAL dispatch loop hands the conn back.
			release()
			handBack750(t, rig)
			placed := takeSQEs(w.ring)
			t.Logf("celeris750 HELDLIST arm=%s after_handback held=%d placed=%v sendBuf=%q dirty=%v", name,
				heldSends750(cs), sqeOps750(placed), cs.sendBuf, cs.dirty)
			if heldSends750(cs) != 0 || len(placed) != 1 || placed[0].op != opSEND || string(cs.sendBuf) != next {
				t.Errorf("after the hand-back: held=%d placed %v sendBuf=%q; want the completion applied and one "+
					"SEND of %q", heldSends750(cs), sqeOps750(placed), cs.sendBuf, next)
			}
			if !cs.dirty {
				t.Errorf("the hand-back did not put the conn back on the dirty list")
			}
			endDispatch750(rig)
		})
	}
}

// TestIouringSendCompletionStillWaitsForABoundedHolder is the negative control
// for the unit arms, as #704's TestIouringCloseStillWaitsForABoundedHolder is
// for the close. The dispatch goroutine is PARKED, or past a Detach (it no
// longer holds the lock across a handler), so whoever holds detachMu is a
// guarded writeFn in one write, a hold bounded by a syscall. A completion must
// wait for it, as it always has, and be applied then: held instead, it would
// wait for a hand-back that nothing owes.
func TestIouringSendCompletionStillWaitsForABoundedHolder(t *testing.T) {
	const tail = "the rest of response 1"
	for _, tc := range []struct {
		name  string
		setup func(cs *connState) (running bool)
	}{
		{"parked", func(*connState) bool { return false }},
		{"after_detach", func(cs *connState) bool { cs.asyncDetachUnlocked = true; return true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newStallRig704(t)
			w, cs := rig.w, rig.cs
			inFlight750(rig, tail, false)
			release := holdAsHandler704(t, cs, tc.setup(cs))
			c := sendCQE750(rig, int32(len(tail)), 0)
			// handleSend is queued in holdOrLockSend's blocking Lock (its
			// TryLock failed, and the holder is a bounded one), or it returned
			// holding the completion: no timer decides (celeris#884 item 22).
			waits, done := waitsOnBoundedHolder884(t, cs.detachMu, func() {
				w.handleSend(c, rig.local, time.Now().UnixNano())
			})
			if !waits {
				release()
				t.Fatalf("handleSend did not wait in cs.detachMu's Lock while a bounded holder held it (waiters=%d, "+
					"returned=%v, held=%d): the completion was held, and nothing owes it back", mutexWaiters(cs.detachMu),
					chanClosed884(done), heldSends750(cs))
			}
			release()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("handleSend never returned after the holder released detachMu")
			}
			t.Logf("celeris750 BOUNDED arm=%s held=%d sending=%v", tc.name, heldSends750(cs), cs.sending)
			if heldSends750(cs) != 0 || cs.sending || len(cs.sendBuf) != 0 {
				t.Errorf("the completion was not applied after the bounded holder let go: held=%d sending=%v "+
					"sendBuf=%q", heldSends750(cs), cs.sending, cs.sendBuf)
			}
		})
	}
}

// ---- end to end: the issue's measurement --------------------------------

// sendStallBig750 is /big's body: far larger than the client's receive buffer
// and the socket's send buffer, so the dispatch goroutine's direct write is
// short and the rest goes out as a ring SEND that waits for the client.
const sendStallBig750 = 3 << 20

type sendStallHandler750 struct {
	big  []byte
	gate *stallGate704
}

func (h sendStallHandler750) HandleStream(ctx context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	var body []byte
	switch s.Path {
	case "/big":
		body = h.big
	case "/slow":
		h.gate.hold()
		body = []byte("slow")
	default:
		id, _ := ctxkit.WorkerIDFrom(ctx)
		body = []byte("w=" + strconv.Itoa(id))
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", strconv.Itoa(len(body))}}, body)
}
func (sendStallHandler750) RouteAsync(_, p string) bool { return p == "/big" || p == "/slow" }
func (sendStallHandler750) HasAsyncRoutes() bool        { return true }

// TestIouringSendCompletionDuringASlowAsyncHandlerDoesNotStallItsWorker: a
// pipelining client asks for /big, whose tail goes out as a ring SEND, and
// then for /slow, an async handler the test gates; once it is running the
// client reads /big, so the SEND completes while the handler runs. A fast
// keep-alive conn on the same worker is pinged throughout and every request
// must complete while /slow is gated. The run must also have held a completion
// (sendHeldHook counted one): a run that never held one proves nothing about
// this path, and fails (celeris#884 items 19 and 23).
//
// The orderings the test needs are signals, not sleeps: /big's dispatch
// goroutine has parked (its direct write is over and it handed the rest to the
// worker) and a run of fast requests has gone through the worker's loop (the
// hand-back drained, the rest submitted as a ring SEND) before /slow is sent;
// /slow's handler has entered before the client reads /big. Sent earlier,
// /slow reaches the goroutine before the worker has placed the SEND, the
// dirty-list pass meets the running handler and gives the conn up, and no
// SEND is ever in flight to complete: a run that held nothing (celeris#704's
// give-up, not celeris#750's hold).
func TestIouringSendCompletionDuringASlowAsyncHandlerDoesNotStallItsWorker(t *testing.T) {
	var held atomic.Int64
	hook := func() { held.Add(1) }
	sendHeldHook.Store(&hook)
	t.Cleanup(func() { sendHeldHook.Store(nil) })

	big := bytes.Repeat([]byte("0123456789abcdef"), sendStallBig750/16)
	gate := newStallGate704(t)
	e, addr := startFDLEngine(t, sendStallHandler750{big: big, gate: gate}, func(c *resource.Config) { c.AsyncHandlers = true })
	gate.openAtCleanup(t)
	d := net.Dialer{Timeout: 3 * time.Second, Control: func(_, _ string, rc syscall.RawConn) error {
		return rc.Control(func(fd uintptr) { _ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 4096) })
	}}
	sc, err := d.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	slow := &stallConn704{c: sc, br: bufio.NewReaderSize(sc, 4096)}
	fast, worker := colocate704(t, addr, slow)
	var lat []time.Duration
	ping := func(n int) error {
		l, err := pingFast704(fast, n)
		lat = append(lat, l...)
		return err
	}

	parkedBefore := asyncParkedGoroutines884()
	_ = sc.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := sc.Write([]byte("GET /big HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("send /big: %v", err)
	}
	// The goroutine's direct write filled the buffers, handed the rest to the
	// worker and parked: the tail is the worker's, a ring SEND that waits for
	// the client. A byte at the client is not that signal (the write is still
	// running when the first one arrives).
	waitAsyncParked884(t, parkedBefore)
	if err := ping(10); err != nil { // the worker's loop drained the hand-back and submitted the SEND
		t.Fatalf("a fast request failed before /slow was sent: %v", err)
	}
	if got := held.Load(); got != 0 {
		t.Fatalf("apparatus: %d completion(s) held before /slow was sent", got)
	}

	if _, err := sc.Write([]byte("GET /slow HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("send /slow: %v", err)
	}
	gate.waitEntered(t) // the slow handler runs, holding the conn's detachMu

	// The client reads /big: the SEND completes while /slow's handler runs.
	type bigResult struct {
		ok  bool
		err error
	}
	bigDone := make(chan bigResult, 1)
	go func() {
		r1, err := http.ReadResponse(slow.br, nil)
		if err != nil {
			bigDone <- bigResult{false, err}
			return
		}
		var got []byte
		buf := make([]byte, 64<<10)
		for err == nil {
			var n int
			n, err = r1.Body.Read(buf)
			got = append(got, buf[:n]...)
		}
		_ = r1.Body.Close()
		if err == io.EOF {
			err = nil
		}
		bigDone <- bigResult{bytes.Equal(got, big), err}
	}()

	// While /slow is gated: fast requests until a completion has been held,
	// then a run more. A parked worker fails the first one that has to wait.
	var perr error
	for dl := time.Now().Add(stallDeadline704); held.Load() == 0 && perr == nil; {
		if time.Now().After(dl) {
			break
		}
		perr = ping(1)
	}
	heldAtWitness := held.Load()
	if perr == nil {
		perr = ping(stallPings704)
	}
	gate.open()
	br := <-bigDone

	// /slow's answer, read for the log only: whether it follows /big intact is
	// celeris#751, not this test.
	var slowBody []byte
	r2, slowErr := http.ReadResponse(slow.br, nil)
	if slowErr == nil {
		slowBody, slowErr = io.ReadAll(r2.Body)
		_ = r2.Body.Close()
	}

	worst, p50 := stallStats704(lat)
	t.Logf("celeris750 SENDSTALL worker=%s workers=%d samples=%d max_ms=%.1f p50_ms=%.2f budget_ms=%d held=%d "+
		"fast_err=%v big_ok=%v big_err=%v slow_body=%q slow_err=%v", worker, e.NumWorkers(), len(lat),
		float64(worst)/1e6, float64(p50)/1e6, stallBudget704.Milliseconds(), held.Load(), perr, br.ok, br.err,
		slowBody, slowErr)
	if perr != nil {
		t.Errorf("celeris#750: a fast request on worker %s did not complete within %v while a /slow async handler "+
			"was gated on another conn of the same worker, whose ring SEND completed mid-handler (held=%d): the "+
			"worker was parked on that conn's detachMu: %v", worker, stallDeadline704, held.Load(), perr)
	}
	if heldAtWitness == 0 {
		t.Errorf("apparatus: no send completion was held while /slow ran (sendHeldHook counted 0): the run did not " +
			"exercise celeris#750 (/big's tail was not a ring SEND in flight, or its completion did not come " +
			"while the handler ran)")
	}
	if stallBudgetStrict704() && worst > stallBudget704 {
		t.Errorf("celeris#750: a fast request on worker %s took %v (budget %v, CELERIS_STALL_BUDGET=1)",
			worker, worst, stallBudget704)
	}
	if br.err != nil || !br.ok {
		t.Errorf("reading /big: intact=%v err=%v", br.ok, br.err)
	}
}

// asyncParkedGoroutines884 returns the ids of the dispatch goroutines parked
// in their cond wait (an idle promoted conn's serveAsync), by stack header.
func asyncParkedGoroutines884() map[int]bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	ids := map[int]bool{}
	for _, g := range strings.Split(string(buf), "\n\n") {
		if !strings.Contains(g, "[sync.Cond.Wait") || !strings.Contains(g, ".serveAsync(") {
			continue
		}
		var id int
		if _, err := fmt.Sscanf(g, "goroutine %d [", &id); err == nil {
			ids[id] = true
		}
	}
	return ids
}

// waitAsyncParked884 waits until a dispatch goroutine that was not parked
// before the request under test was sent (before holds the ids then) is
// parked: the conn's own, now idle. Goroutines other tests left behind, which
// may park or exit meanwhile, are in before or are not new.
func waitAsyncParked884(t *testing.T, before map[int]bool) {
	t.Helper()
	for end := time.Now().Add(stallDeadline704); ; time.Sleep(200 * time.Microsecond) {
		for id := range asyncParkedGoroutines884() {
			if !before[id] {
				return
			}
		}
		if time.Now().After(end) {
			t.Fatalf("apparatus: no new dispatch goroutine parked within %v of /big (parked before: %d)", stallDeadline704, len(before))
		}
	}
}
