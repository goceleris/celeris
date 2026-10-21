//go:build linux

package iouring

import (
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// celeris#884 item 63 (from celeris#992 round 3, Opus verify): mutants that
// survived every celeris#896 test. The tests there run a real ring, whose
// completions arrive in the order the running kernel chooses, so the wait's
// decisions on a multishot accept's non-terminal CQE (F_MORE set), on a cancel
// answered with a positive count, and on the cancel's own CQE reaching
// cancelAccept's loop through processCQE were never made to differ by the
// order of anything. These tests take the kernel out: the worker is the
// unarmed one of newAcceptRig896 (nothing is in the kernel), and the ring wait
// (shutdownRingWait) is replaced by one that posts, call by call, the
// completions the test scripts into the CQ ring and never enters the ring. The
// wait then ends, or does not, on exactly what was posted, and the number of
// ring waits it made is the observation.
//
// What this does not settle is the item's open half: a terminal accept CQE
// left unread from an earlier listen socket that a re-created socket shares a
// number with ends the wait early, because the user data names only the
// descriptor. The scripts below treat "another descriptor" as the case the
// drain handles today; counting terminal CQEs against the cancel's result needs
// the order of the cancel's CQE and the accept's terminal CQE on the kernels CI
// runs, which no test here can supply (the scripts A and B pin both orders as
// the code takes them, not as any kernel posts them).

// postCQE writes c at the CQ ring's tail and publishes it. Nothing is in the
// kernel's hands on the rigs these tests use, so the kernel posts none of its
// own over it.
func (r *acceptRig896) postCQE(c completionEntry) {
	tail := atomic.LoadUint32((*uint32)(r.ring.cqTail))
	*r.ring.cqeAt(tail) = c
	atomic.StoreUint32((*uint32)(r.ring.cqTail), tail+1)
}

// scriptedCQE896 is one completion in a script. The op is udAccept or
// udListenCancel; the descriptor is the rig's listen socket's, or, with other
// set, the next descriptor number (the end of another socket's accept, or its
// cancel's answer).
type scriptedCQE896 struct {
	op    uint64
	other bool
	res   int32
	flags uint32
}

// ud is the user data of the completion for the rig's listen descriptor lfd.
func (s scriptedCQE896) ud(lfd int) uint64 {
	if s.other {
		lfd++
	}
	return encodeUserData(s.op, lfd)
}

var (
	// A multishot accept's delivery that does not end it: a connection
	// accepted (here an error, so no descriptor is owned by the test), F_MORE
	// set. Res is -EMFILE so the accept path's error class tells whether the
	// completion went through processCQE (AcceptFDLimit).
	accMore896 = scriptedCQE896{op: udAccept, res: -int32(unix.EMFILE), flags: cqeFMore}
	// The accept's terminal CQE: the cancel's -ECANCELED, F_MORE clear.
	accEnd896 = scriptedCQE896{op: udAccept, res: -int32(unix.ECANCELED)}
)

func cancelAns896(res int32) scriptedCQE896 { return scriptedCQE896{op: udListenCancel, res: res} }

// script896 says what each ring wait of the close posts, and what the close
// must then have done.
type script896 struct {
	name   string
	calls  [][]scriptedCQE896 // calls[i] is posted by the (i+1)th ring wait
	gaveUp uint64             // give-ups the close must count
	why    string
}

var scripts896 = []script896{
	{
		name:  "A_more_then_cancel_count_then_terminal",
		calls: [][]scriptedCQE896{{accMore896}, {cancelAns896(1)}, {accEnd896}},
		why: "the delivery with F_MORE set and the cancel's positive count both leave the accept running; " +
			"only its terminal CQE ends the wait: three waits",
	},
	{
		name:  "B_terminal_before_cancel_answer",
		calls: [][]scriptedCQE896{{accMore896}, {accEnd896}},
		why:   "the accept's terminal CQE ends the wait without the cancel's answer: two waits",
	},
	{
		name:  "C_cancel_counts_zero",
		calls: [][]scriptedCQE896{{accMore896}, {cancelAns896(0)}},
		why:   "a cancel that matched nothing (0) ends the wait: no accept holds the socket: two waits",
	},
	{
		name:  "D_cancel_enoent",
		calls: [][]scriptedCQE896{{cancelAns896(-int32(unix.ENOENT))}},
		why:   "-ENOENT, a kernel's way of saying nothing was armed, ends the wait: one wait",
	},
	{
		name:  "E_cancel_ealready_then_terminal",
		calls: [][]scriptedCQE896{{cancelAns896(-int32(unix.EALREADY))}, {accEnd896}},
		why:   "-EALREADY (the accept is being completed) leaves the wait to the accept's own CQE: two waits",
	},
	{
		name:   "F_cancel_rejected_gives_up",
		calls:  [][]scriptedCQE896{{cancelAns896(-int32(unix.EINVAL))}},
		gaveUp: 1,
		why:    "a cancel the kernel rejects cannot be waited on: the wait ends at once and counts a give-up: one wait",
	},
	{
		name: "G_another_sockets_ends_are_ignored",
		calls: [][]scriptedCQE896{
			{{op: udAccept, other: true, res: -int32(unix.ECANCELED)}, {op: udListenCancel, other: true}},
			{accEnd896},
		},
		why: "another descriptor's terminal accept CQE and cancel answer end nothing: two waits",
	},
	{
		name:  "H_everything_in_one_batch",
		calls: [][]scriptedCQE896{{accMore896, cancelAns896(1), accEnd896}},
		why:   "a batch that holds the delivery, the count and the terminal CQE in order is read to its end: one wait",
	},
}

// scriptedWait896 replaces shutdownRingWait for one test: the ring wait of call
// i posts calls[i] into r's CQ ring and returns without entering the ring. A
// wait past the script posts nothing and sleeps d, as a ring that never
// answers does; the close's own bound ends it, and the call count shows it.
func scriptedWait896(t *testing.T, r *acceptRig896, calls [][]scriptedCQE896) (waits *int) {
	t.Helper()
	n := new(int)
	lfd := r.w.listenFD // the close routes clear w.listenFD before they wait
	orig := shutdownRingWait
	shutdownRingWait = func(rg *Ring, d time.Duration) error {
		if rg != r.ring {
			t.Errorf("a ring wait on a ring that is not the rig's")
		}
		i := *n
		*n++
		if i >= len(calls) {
			time.Sleep(d)
			return nil
		}
		for _, s := range calls[i] {
			r.postCQE(completionEntry{UserData: s.ud(lfd), Res: s.res, Flags: s.flags})
		}
		return nil
	}
	t.Cleanup(func() { shutdownRingWait = orig })
	return n
}

// closeRoute896 is a way to close the rig's listen socket, and what a
// completion that reaches it does.
type closeRoute896 struct {
	name string
	run  func(r *acceptRig896)
	// processes: the route hands the completions it meets to processCQE, so
	// the accept path's error counters move (cancelAccept); the drain reads them
	// itself and counts nothing.
	processes bool
}

var closeRoutes896CQE = []closeRoute896{
	{"shutdown-drain", func(r *acceptRig896) { r.w.endOwedOpsAtShutdown(); r.closeListener() }, false},
	{"stopAccepting", func(r *acceptRig896) { closeRoute896ByName("stopAccepting")(r.w) }, true},
	{"pause-close", func(r *acceptRig896) { closeRoute896ByName("pause-close")(r.w) }, true},
}

// closeRoute896ByName finds a route of closeRoutes896 by its name, so a
// reordering of that table cannot silently swap the routes under test.
func closeRoute896ByName(name string) func(w *Worker) {
	for _, rt := range closeRoutes896 {
		if rt.name == name {
			return rt.close
		}
	}
	panic("closeRoutes896 has no route " + name)
}

// TestListenCancelWaitEndsOnTheScriptedCompletions896 plays every script
// through the three routes that wait for the accept (the shutdown drain, and
// cancelAccept through stopAccepting and the pause's close) on every tier's
// ring. The wait must make exactly the ring waits the script names, count the
// give-ups it names, and, on cancelAccept's routes, have handed each accept
// completion it met to processCQE (the delivery with F_MORE is an
// AcceptFDLimit, the terminal CQE an AcceptCancelled).
func TestListenCancelWaitEndsOnTheScriptedCompletions896(t *testing.T) {
	for _, rt := range closeRoutes896CQE {
		for _, sc := range scripts896 {
			for _, tc := range tiers896 {
				t.Run(rt.name+"/"+sc.name+"/"+tc.name, func(t *testing.T) {
					r := newAcceptRig896(t, tc.tier(), false)
					waits := scriptedWait896(t, r, sc.calls)
					rt.run(r)
					if *waits != len(sc.calls) {
						t.Errorf("%s made %d ring waits, want %d: %s", rt.name, *waits, len(sc.calls), sc.why)
					}
					if g := r.w.handoffLoss.shutdownListenCancelGaveUp.Load(); g != sc.gaveUp {
						t.Errorf("%d give-ups counted, want %d (%s)", g, sc.gaveUp, sc.why)
					}
					var more, ended uint64
					for _, call := range sc.calls {
						for _, s := range call {
							if s.op != udAccept {
								continue
							}
							if s.flags&cqeFMore != 0 {
								more++
							} else {
								ended++
							}
						}
					}
					wantMore, wantEnded := uint64(0), uint64(0)
					if rt.processes {
						wantMore, wantEnded = more, ended
					}
					if got := r.w.errs.AcceptFDLimit.Load(); got != wantMore {
						t.Errorf("%s: AcceptFDLimit = %d, want %d: the accept completions the close met were %shanded to processCQE",
							rt.name, got, wantMore, map[bool]string{true: "", false: "not "}[rt.processes])
					}
					if got := r.w.errs.AcceptCancelled.Load(); got != wantEnded {
						t.Errorf("%s: AcceptCancelled = %d, want %d", rt.name, got, wantEnded)
					}
					if head, tail := r.ring.BeginCQ(); head != tail {
						t.Errorf("%d completions are left unread in the CQ ring", tail-head)
					}
				})
			}
		}
	}
}

// TestListenCancelDeliveredAcceptReachesProcessCQE896: an accept completion
// that DELIVERS a connection (Res >= 0, F_MORE set: the multishot accept goes
// on) which the cancel wait meets must reach processCQE, whose accept path
// takes ownership of the descriptor (and closes it if it cannot). The
// scripts above deliver an error (-EMFILE), so a close that drops only the
// successful deliveries (the E13 round-3 verifier's own V5, `if
// !(ud&udMask == udAccept && c.Res >= 0) { w.processCQE(...) }`) passes them
// all: nothing but the wait's call count and the error counters is looked at
// there. Here the delivery's Res is math.MaxInt32, a descriptor number
// beyond the conn table that no process owns, so onAcceptedFD counts it in
// ConnTableCap and closes the nonexistent descriptor (EBADF, ignored); the
// counter is the observation that it was handled. Only the routes that hand
// the completions to processCQE (cancelAccept: stopAccepting and the pause's
// close) are played; the shutdown drain reads the CQEs itself.
func TestListenCancelDeliveredAcceptReachesProcessCQE896(t *testing.T) {
	deliver := scriptedCQE896{op: udAccept, res: math.MaxInt32, flags: cqeFMore}
	calls := [][]scriptedCQE896{{deliver}, {cancelAns896(1)}, {accEnd896}}
	for _, rt := range closeRoutes896CQE {
		if !rt.processes {
			continue
		}
		for _, tc := range tiers896 {
			t.Run(rt.name+"/"+tc.name, func(t *testing.T) {
				r := newAcceptRig896(t, tc.tier(), false)
				waits := scriptedWait896(t, r, calls)
				rt.run(r)
				if got := r.w.errs.ConnTableCap.Load(); got != 1 {
					t.Errorf("%s: ConnTableCap = %d, want 1: the delivered accept (fd %d, beyond the conn table, F_MORE set) "+
						"was not handed to processCQE, which counts it and closes the descriptor", rt.name, got, deliver.res)
				}
				if *waits != len(calls) {
					t.Errorf("%s made %d ring waits, want %d: the delivery and the cancel's count leave the accept running, "+
						"only its terminal CQE ends the wait", rt.name, *waits, len(calls))
				}
				if got := r.w.errs.AcceptCancelled.Load(); got != 1 {
					t.Errorf("%s: AcceptCancelled = %d, want 1 (the terminal CQE)", rt.name, got)
				}
				if head, tail := r.ring.BeginCQ(); head != tail {
					t.Errorf("%d completions are left unread in the CQ ring", tail-head)
				}
			})
		}
	}
}

// TestListenCancelAnswer896 is listenCancelAnswer's table: what the cancel's
// result says to the wait that made it.
func TestListenCancelAnswer896(t *testing.T) {
	for _, tc := range []struct {
		name     string
		res      int32
		wantDone bool
		wantErr  error // nil: no error; else the errno the error wraps
	}{
		{"zero_nothing_armed", 0, true, nil},
		{"enoent_nothing_armed", -int32(unix.ENOENT), true, nil},
		{"one_accept_cancelled", 1, false, nil},
		{"many_accepts_cancelled", 7, false, nil},
		{"ealready_being_completed", -int32(unix.EALREADY), false, nil},
		{"einval_rejected", -int32(unix.EINVAL), true, unix.EINVAL},
		{"ebadf", -int32(unix.EBADF), true, unix.EBADF},
		{"eperm", -int32(unix.EPERM), true, unix.EPERM},
		{"ecanceled_is_not_an_answer", -int32(unix.ECANCELED), true, unix.ECANCELED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done, err := listenCancelAnswer(tc.res)
			if done != tc.wantDone {
				t.Errorf("listenCancelAnswer(%d) done = %v, want %v", tc.res, done, tc.wantDone)
			}
			switch {
			case tc.wantErr == nil && err != nil:
				t.Errorf("listenCancelAnswer(%d) error = %v, want none", tc.res, err)
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Errorf("listenCancelAnswer(%d) error = %v, want one wrapping %v", tc.res, err, tc.wantErr)
			}
		})
	}
}
