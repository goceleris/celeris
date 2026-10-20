//go:build linux

package iouring

import (
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
)

// celeris#884 items 20 and 21: what becomes of a send completion held for a
// dispatch goroutine (celeris#750, heldSends) once the goroutine is gone or
// its conn is.
//
// Item 20: drainDetachQueue applies a conn's held completions at the top of
// every entry, before any of the branches that look at the conn (a claimed
// hand-off, an h2c promotion, a close). The two exits below are the ones a
// goroutine takes on its own, with a completion held and a hand-back owed: the
// park claim (a transplant drain is active at its park) and the h2c upgrade
// exit. Each is a real dispatch goroutine; its one queue entry is drained and
// the completion must have been applied by then, which each arm reads off what
// the applied completion lets the branch do: the claim's reap and the
// hand-off it leads to (finishAsyncTransplant refuses a conn with a send in
// flight, so the reap is placed only if the completion was applied first),
// and the 101 the replay's flush submits behind the held SEND. With a single
// held entry, a replay that ran at a later entry would still leave heldSends
// at 0 in the end, so the park claim's arm does not rest on heldSends: the
// reap and the hand-off are its mandatory assertion.
//
// Item 21: nothing a held completion carries may reach a conn that is not the
// one it was held for. releaseConnState empties heldSends; replayHeldSends
// refuses an entry whose generation is not the conn's and whose conn is not
// the slot's owner. Each guard has its own arm, which defeats the others.

const handBackTail884 = "the rest of response 1"

// heldParked884 is a promoted async conn served by a REAL dispatch goroutine
// that is now parked (no transplant drain is set), with a ring SEND of
// handBackTail884 in flight whose completion arrived while a handler held
// detachMu (the test stands for the handler: holdAsHandler704) and was held.
// The hand-back is owed (relinkOwed). The goroutine knows nothing of it yet.
type heldParked884 struct{ f *fdlFixture }

func newHeldParked884(t *testing.T, h2c bool) *heldParked884 {
	t.Helper()
	f, h := asyncParkFixture(t, true)
	f.stopDrain()
	if h2c {
		// A fresh h1State with the upgrade on, as onAcceptedFD makes it.
		f.w.cfg.EnableH2Upgrade = true
		f.w.h2cfg = conn.H2Config{MaxConcurrentStreams: 100, InitialWindowSize: 65535,
			MaxFrameSize: 16384, MaxRequestBodySize: 1 << 20}
		f.w.initProtocol(f.cs)
	}
	if claimed, _ := f.asyncRequest(h); claimed {
		t.Fatal("apparatus: the goroutine claimed a hand-off with no drain set")
	}
	cs, w := f.cs, f.w

	// The worker's rest-of-response SEND, as flushSend leaves it. Under
	// detachMu, which is also what orders the goroutine's earlier writes of
	// these fields before the test's.
	cs.detachMu.Lock()
	cs.writeBuf = append(cs.writeBuf[:0], handBackTail884...)
	full := w.flushSend(cs)
	cs.detachMu.Unlock()
	if sqes := takeSQEs(w.ring); full || len(sqes) != 1 || sqes[0].op != opSEND || !cs.sending {
		t.Fatalf("apparatus: the rest of the response placed %v (ring full %v, sending %v), want one SEND", sqes, full, cs.sending)
	}

	// Its completion arrives while a handler holds detachMu: held.
	release := holdAsHandler704(t, cs, true)
	f.process(f.sendCQE())
	if heldSends750(cs) != 1 || !relinkOwed704(cs) {
		release()
		t.Fatalf("apparatus: held=%d relinkOwed=%v, want the completion held with a hand-back owed",
			heldSends750(cs), relinkOwed704(cs))
	}
	release()
	cs.asyncInMu.Lock()
	setParked704(cs, true) // the goroutine is where it was: parked in its cond wait
	cs.asyncInMu.Unlock()
	return &heldParked884{f: f}
}

// exit waits for the goroutine's one hand-back entry to be on the detach queue
// (the goroutine has exited) and returns, after checking there is exactly one.
func (hp *heldParked884) exit(t *testing.T) {
	t.Helper()
	w, cs := hp.f.w, hp.f.cs
	for dl := time.Now().Add(5 * time.Second); w.detachQPending.Load() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(dl) {
			t.Fatal("the dispatch goroutine did not exit within 5s")
		}
	}
	w.detachQMu.Lock()
	n := len(w.detachQueue)
	w.detachQMu.Unlock()
	cs.asyncInMu.Lock()
	run := cs.asyncRun
	cs.asyncInMu.Unlock()
	if n != 1 || run {
		t.Fatalf("apparatus: %d entries on the detach queue, asyncRun=%v; want the exit's one entry and a goroutine gone", n, run)
	}
}

// TestIouringHeldCompletionAppliedAtEveryHandBackEntry884 (item 20): the exit
// by the park claim and the exit by the h2c upgrade each hand the conn back
// with a completion held; the entry's drain must apply it.
func TestIouringHeldCompletionAppliedAtEveryHandBackEntry884(t *testing.T) {
	t.Run("park_claim", func(t *testing.T) {
		hp := newHeldParked884(t, false)
		f, w, cs := hp.f, hp.f.w, hp.f.cs

		// A transplant drain is set while the goroutine sits parked; it wakes,
		// finds the conn eligible (nothing staged, a clean boundary), claims the
		// hand-off and exits.
		f.startDrain()
		cs.asyncInMu.Lock()
		cs.asyncCond.Broadcast()
		cs.asyncInMu.Unlock()
		hp.exit(t)
		if !cs.transplantPending.Load() {
			t.Fatal("apparatus: the goroutine exited without claiming the hand-off")
		}

		w.drainDetachQueue()
		sqes := takeSQEs(w.ring)
		t.Logf("celeris884 HANDBACK arm=park_claim held=%d sending=%v kernelInflight=%d recvArmed=%v placed=%v adopted=%d",
			heldSends750(cs), cs.sending, cs.kernelInflight, cs.recvArmed, sqes, f.tgt.adopted.Load())
		if heldSends750(cs) != 0 {
			t.Errorf("%d completion(s) still held after the claim's entry was drained", heldSends750(cs))
		}
		// The mandatory half: the claim went on to reap the armed recv. With the
		// completion still held, the SEND is still in flight and
		// finishAsyncTransplant leaves the conn alone.
		if len(sqes) != 1 || !f.isReap(sqes[0]) {
			t.Fatalf("the drain of the claim placed %v, want the reap of the armed recv (the held completion applied "+
				"first, so the claim found nothing in flight)", sqes)
		}
		f.process(f.recvCQE(-int32(unix.ECANCELED)))
		if n := f.tgt.adopted.Load(); n != 1 {
			t.Fatalf("the reap's -ECANCELED made %d hand-offs, want 1", n)
		}
	})

	t.Run("h2c_exit", func(t *testing.T) {
		hp := newHeldParked884(t, true)
		f, w, cs := hp.f, hp.f.w, hp.f.cs

		// The next request is the h2c upgrade. The goroutine wakes, takes it,
		// promotes the conn to H2 and exits. A ring SEND is still outstanding
		// (its completion is held), so the 101 stays staged in writeBuf.
		f.deliver(h2cUpgradeHead722("GET", 0))
		hp.exit(t)
		if !cs.asyncH2Promoted.Load() {
			t.Fatal("apparatus: the goroutine did not exit through the h2c upgrade")
		}

		w.drainDetachQueue()
		sqes := takeSQEs(w.ring)
		inH2 := false
		for _, fd := range w.h2Conns {
			inH2 = inH2 || fd == f.fd
		}
		t.Logf("celeris884 HANDBACK arm=h2c_exit held=%d sending=%v sendBuf=%d h2Conns=%v placed=%v", heldSends750(cs),
			cs.sending, len(cs.sendBuf), inH2, sqes)
		if heldSends750(cs) != 0 {
			t.Errorf("%d completion(s) still held after the h2c exit's entry was drained", heldSends750(cs))
		}
		// The applied completion lets the replay's flush submit the staged 101: one
		// SEND, behind the completed one (and the recv the completion re-arms).
		sends := 0
		for _, q := range sqes {
			if q.op == opSEND {
				sends++
			}
		}
		if sends != 1 || !cs.sending || len(cs.sendBuf) == 0 {
			t.Errorf("the drain placed %v (sending=%v, %d bytes to send), want one SEND of the staged 101 behind the "+
				"completed one", sqes, cs.sending, len(cs.sendBuf))
		}
		if !inH2 {
			t.Errorf("the entry did not register the promoted conn on the H2 write-queue list")
		}
	})
}

// reuse884 is the release of cs back to the pool and its next acquire for fd,
// as drainPendingRelease and acquireConnState do between them, minus the pool:
// a sync.Pool hands the same pointer back only by luck (under -race, a Put is
// dropped one time in four), and what is tested is the connState a reused
// pointer is. The generation comes from the real source, connGenSeq.
func reuse884(cs *connState, fd int) {
	releaseConnState(cs)
	g := connGenSeq.Add(1)
	if g == 0 {
		g = connGenSeq.Add(1)
	}
	cs.fd = fd
	cs.generation = g
	cs.detachMu = &sync.Mutex{}
	cs.asyncCond.L = &cs.asyncInMu
}

// holdOne884 holds one completion of the rig's conn, as a running handler
// would make the worker, and returns it.
func holdOne884(t *testing.T, rig *stallRig704, tail string) *completionEntry {
	t.Helper()
	inFlight750(rig, tail, false)
	release := holdAsHandler704(t, rig.cs, true)
	defer release()
	c := sendCQE750(rig, int32(len(tail)), 0)
	rig.w.handleSend(c, rig.local, time.Now().UnixNano())
	if heldSends750(rig.cs) != 1 {
		t.Fatalf("apparatus: the completion was not held (held=%d)", heldSends750(rig.cs))
	}
	return c
}

// TestIouringHeldCompletionIsNotAppliedToANewOwner884 (item 21).
func TestIouringHeldCompletionIsNotAppliedToANewOwner884(t *testing.T) {
	const old = "the rest of response 1"
	const fresh = "the new owner's response, in flight"

	// release: a connState released with a completion held comes back empty.
	// Its next owner's own completion, delivered with the lock free, is applied
	// at once; a stale entry left behind would send it down the ordering hold
	// (a completion is held whatever the lock while an earlier one is) and the
	// new owner's SEND would never complete. Mutant Mrelease (releaseConnState
	// no longer empties heldSends) fails here.
	t.Run("release", func(t *testing.T) {
		rig := newStallRig704(t)
		w, cs := rig.w, rig.cs
		holdOne884(t, rig, old)
		reuse884(cs, rig.local)
		if n := heldSends750(cs); n != 0 {
			t.Errorf("releaseConnState left %d held completion(s) on the connState", n)
		}
		cs.sending, cs.sendBuf = true, append(cs.sendBuf[:0], fresh...)
		c := &completionEntry{UserData: encodeUserDataGen(udSend, rig.local, cs.generation), Res: int32(len(fresh))}
		w.handleSend(c, rig.local, time.Now().UnixNano())
		t.Logf("celeris884 REUSE arm=release held=%d sending=%v sendBuf=%d", heldSends750(cs), cs.sending, len(cs.sendBuf))
		if cs.sending || len(cs.sendBuf) != 0 || heldSends750(cs) != 0 {
			t.Errorf("the new owner's completion was not applied (sending=%v sendBuf=%q held=%d): a stale held "+
				"completion forced it to wait behind it", cs.sending, cs.sendBuf, heldSends750(cs))
		}
	})

	// generation: the same connState, a new generation, and a held entry of the
	// old one still on it (what an un-emptied release would leave). The replay
	// must drop it: applied, it completes the new owner's SEND with the old
	// owner's byte count. Mutant Mgen (the generation test dropped) fails here.
	t.Run("generation", func(t *testing.T) {
		rig := newStallRig704(t)
		w, cs := rig.w, rig.cs
		stale := *holdOne884(t, rig, old)
		reuse884(cs, rig.local)
		cs.sending, cs.sendBuf = true, append(cs.sendBuf[:0], fresh...)
		cs.heldSends = append(cs.heldSends[:0], stale)
		if decodeGen(stale.UserData) == cs.generation {
			t.Fatal("apparatus: the new owner has the old generation")
		}
		w.replayHeldSends(cs)
		t.Logf("celeris884 REUSE arm=generation held=%d sending=%v sendBuf=%d", heldSends750(cs), cs.sending, len(cs.sendBuf))
		if !cs.sending || string(cs.sendBuf) != fresh {
			t.Errorf("the old owner's held completion was applied to the new owner: sending=%v sendBuf=%q, want its "+
				"SEND still in flight with %q", cs.sending, cs.sendBuf, fresh)
		}
		if heldSends750(cs) != 0 {
			t.Errorf("%d stale completion(s) still held after the replay", heldSends750(cs))
		}
	})

	// slot: the conn left its slot and another connState owns the descriptor
	// number. The old conn's hand-back entry (its goroutine's exit) replays its
	// held completion, whose generation is the old conn's own: only the slot
	// check tells it is not the owner's. handleSend finds the slot's owner by
	// number, so a replay that went on would complete the OWNER's SEND.
	t.Run("slot", func(t *testing.T) {
		rig := newStallRig704(t)
		w, oldCS := rig.w, rig.cs
		holdOne884(t, rig, old)
		next := acquireConnState(t.Context(), rig.local, 4096, true)
		next.sending, next.sendBuf = true, append(next.sendBuf[:0], fresh...)
		w.conns[rig.local] = next
		w.enqueueDetach(oldCS)
		w.drainDetachQueue()
		t.Logf("celeris884 REUSE arm=slot held=%d owner_sending=%v owner_sendBuf=%d", heldSends750(oldCS), next.sending,
			len(next.sendBuf))
		if !next.sending || string(next.sendBuf) != fresh {
			t.Errorf("the old conn's held completion was applied to the slot's new owner: sending=%v sendBuf=%q, "+
				"want its SEND still in flight with %q", next.sending, next.sendBuf, fresh)
		}
		if heldSends750(oldCS) != 0 {
			t.Errorf("%d completion(s) still held on the conn that left its slot", heldSends750(oldCS))
		}
		w.conns[rig.local] = oldCS // the rig's cleanup closes the descriptor through it
	})
}
