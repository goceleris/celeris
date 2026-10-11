//go:build linux

package iouring

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// celeris#811, the fixture half. The end-to-end test
// (dirty_busy_poll_811_linux_test.go) measures the CPU of a worker whose async
// conns wait on SENDs; these arms pin the two things it cannot reach by
// itself:
//
//   - every kind of outstanding SEND is given up by the dirty pass, and the
//     completion that follows is what acts for the conn (the wake the worker
//     owes once it stops polling the conn). The SEND_ZC arms matter because the
//     end-to-end test's ZC send may never be armed (a runner's 8 MiB memlock
//     fails it with ENOMEM and the send falls back to a plain one), and because
//     a SEND_ZC whose first completion failed leaves sending false and
//     zcNotifPending true, a state the pass used to treat as idle;
//   - the celeris#607 recv-stall episode of a conn that is owed its recv arm
//     while its SEND is outstanding survives the give-up and ends at the arm.

// TestIouringDirtyPassGivesUpAConnWithAnOutstandingSend811: after the pass the
// conn is off the list (so the loop may wait), nothing was placed for it, and
// the completion of its SEND, or its SEND_ZC's notification, sends what the
// handler wrote meanwhile.
func TestIouringDirtyPassGivesUpAConnWithAnOutstandingSend811(t *testing.T) {
	const tail = "the rest of response 1"
	const next = "response 2, written while the SEND was outstanding"
	for _, tc := range []struct {
		name  string
		zc    bool
		setup func(*stallRig704) // state after the SEND, before the pass
		// completions delivered after the pass, in order; the last one must
		// place exactly one SEND of want.
		cqes []step811
		want string
	}{
		{
			name: "send", setup: func(*stallRig704) {},
			cqes: []step811{{int32(len(tail)), 0}}, want: next,
		},
		{
			// SEND_ZC, first completion not yet seen: sending is true.
			name: "zc_in_flight", zc: true, setup: func(*stallRig704) {},
			cqes: []step811{{int32(len(tail)), cqeFMore}, {0, cqeFNotif}}, want: next,
		},
		{
			// SEND_ZC whose first completion failed (the kernel posts the
			// notification after it): sending false, zcNotifPending true. The
			// notification re-sends the unsent bytes as a plain SEND.
			name: "zc_first_completion_failed", zc: true,
			setup: func(rig *stallRig704) {
				rig.w.handleSend(sendCQE750(rig, -int32(unix.ENOMEM), cqeFMore), rig.local, time.Now().UnixNano())
			},
			cqes: []step811{{0, cqeFNotif}}, want: tail,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newStallRig704(t)
			w, cs := rig.w, rig.cs
			inFlight750(rig, tail, tc.zc)
			cs.writeBuf = append(cs.writeBuf, next...)
			tc.setup(rig)
			if tc.name == "zc_first_completion_failed" && (cs.sending || !cs.zcNotifPending) {
				t.Fatalf("apparatus: sending=%v zcNotifPending=%v, want false/true", cs.sending, cs.zcNotifPending)
			}
			if !cs.sending && !cs.zcNotifPending {
				t.Fatal("apparatus: no SEND outstanding")
			}
			takeSQEs(w.ring)
			w.markDirty(cs)

			w.flushDirty()
			if cs.dirty || w.dirtyHead != nil {
				t.Fatalf("celeris#811: the dirty pass left a conn with an outstanding SEND (sending=%v zcNotifPending=%v) "+
					"on the list: baseTimeout returns 0 while the list is not empty, and the worker polls for as long as the SEND stays out",
					cs.sending, cs.zcNotifPending)
			}
			if got := takeSQEs(w.ring); len(got) != 0 {
				t.Errorf("the pass placed %v for a conn whose SEND is outstanding", sqeOps750(got))
			}
			if d := w.baseTimeout(); d == 0 {
				t.Errorf("baseTimeout is 0 with an empty dirty list")
			}

			// The completions are what act for the conn now.
			var placed []sqeRec
			for _, s := range tc.cqes {
				w.handleSend(sendCQE750(rig, s.res, s.flags), rig.local, time.Now().UnixNano())
				placed = append(placed, takeSQEs(w.ring)...)
			}
			if len(placed) != 1 || placed[0].op != opSEND || !cs.sending || string(cs.sendBuf) != tc.want {
				t.Errorf("after the completion(s): placed %v sending=%v sendBuf=%q; want one SEND of %q",
					sqeOps750(placed), cs.sending, cs.sendBuf, tc.want)
			}
		})
	}
}

type step811 struct {
	res   int32
	flags uint32
}

// TestIouringDirtyPassGiveUpKeepsTheRecvStallEpisode811: a conn owed its recv
// arm (the SQ ring dropped it) with a SEND outstanding is passed over, and
// timed, by the celeris#607 witness. Giving it up must not end the episode at
// about zero: it ends when the SEND completes and the arm is placed.
func TestIouringDirtyPassGiveUpKeepsTheRecvStallEpisode811(t *testing.T) {
	rig := newStallRig704(t)
	w, cs := rig.w, rig.cs
	w.recvArm = &recvArmStats{}
	const tail = "the rest of response 1"
	inFlight750(rig, tail, false)
	cs.needsRecv = true
	cs.recvArmed = false
	w.markDirty(cs)

	w.flushDirty()
	if cs.dirty || w.dirtyHead != nil {
		t.Fatalf("celeris#811: a conn owed a recv arm with a SEND outstanding stays on the dirty list: the worker polls for as long as the SEND stays out")
	}
	if cs.recvStallSince == 0 || w.recvArm.stallEpisodes.Load() != 1 {
		t.Fatalf("the stall episode is not open after the give-up: recvStallSince=%d episodes=%d, want open and 1",
			cs.recvStallSince, w.recvArm.stallEpisodes.Load())
	}
	time.Sleep(2 * time.Millisecond)
	w.flushDirty() // not listed: nothing more may be counted
	takeSQEs(w.ring)

	w.handleSend(sendCQE750(rig, int32(len(tail)), 0), rig.local, time.Now().UnixNano())
	placed := takeSQEs(w.ring)
	if len(placed) != 1 || placed[0].op != opRECV || !cs.recvArmed || cs.needsRecv {
		t.Errorf("after the SEND completed: placed %v recvArmed=%v needsRecv=%v; want the owed recv armed",
			sqeOps750(placed), cs.recvArmed, cs.needsRecv)
	}
	if cs.recvStallSince != 0 || w.recvArm.stallEpisodes.Load() != 1 || w.recvArm.stallNanos.Load() < uint64(time.Millisecond) {
		t.Errorf("the episode did not end at the arm: recvStallSince=%d episodes=%d stallNanos=%d, want closed, 1 and >= 1 ms",
			cs.recvStallSince, w.recvArm.stallEpisodes.Load(), w.recvArm.stallNanos.Load())
	}
	if cs.dirty {
		t.Errorf("the conn is on the dirty list after its SEND completed and its recv was armed")
	}
}

// TestIouringCheckTimeoutsJudgesAFailedFirstZCCompletionByWriteTimeout811: the
// give-up takes a SEND_ZC conn whose first completion failed (sending false,
// zcNotifPending true) off the dirty list, and the list was what made the
// sweep judge it by WriteTimeout (cs.dirty). A conn with its notification
// still to come is a conn with a write outstanding, so the sweep reads
// zcNotifPending too. Both ways, so neither the hunk's removal nor a
// blanket "always WriteTimeout" passes:
//
//   - WriteTimeout shorter than the conn's age, ReadTimeout and IdleTimeout
//     longer: closed (judged by WriteTimeout);
//   - ReadTimeout shorter than its age, WriteTimeout longer: not closed.
func TestIouringCheckTimeoutsJudgesAFailedFirstZCCompletionByWriteTimeout811(t *testing.T) {
	const tail = "the rest of response 1"
	const age = 10 * time.Second
	for _, tc := range []struct {
		name      string
		write     time.Duration
		read      time.Duration
		wantClose bool
	}{
		{"write_timeout_exceeded", time.Second, time.Hour, true},
		{"only_read_timeout_exceeded", time.Hour, time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newStallRig704(t)
			w, cs := rig.w, rig.cs
			inFlight750(rig, tail, true)
			w.handleSend(sendCQE750(rig, -int32(unix.ENOMEM), cqeFMore), rig.local, time.Now().UnixNano())
			if cs.sending || !cs.zcNotifPending || cs.dirty {
				t.Fatalf("apparatus: sending=%v zcNotifPending=%v dirty=%v, want false/true/false", cs.sending, cs.zcNotifPending, cs.dirty)
			}
			takeSQEs(w.ring)
			w.cfg.WriteTimeout, w.cfg.ReadTimeout, w.cfg.IdleTimeout = tc.write, tc.read, time.Hour
			cs.lastActivity = time.Now().Add(-age).UnixNano()

			w.checkTimeouts()

			if cs.closing != tc.wantClose {
				t.Errorf("after the sweep: closing=%v, want %v (WriteTimeout %v, ReadTimeout %v, idle for %v): "+
					"a conn with its SEND_ZC notification outstanding is judged by WriteTimeout",
					cs.closing, tc.wantClose, tc.write, tc.read, age)
			}
		})
	}
}
