//go:build linux

package iouring

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// celeris#873: endOwedOpsAtShutdown waits for the ops still owed on the
// connections' descriptors, and its wait breaks out on any error of the ring's
// enter, EBUSY included. EBUSY is what a CQ ring that is full, with
// completions overflowed behind it, answers to a submit that waits for events:
// the likely state at a shutdown with many connections, whose cancels and
// shutdown(2)s each add a completion to a ring the loop is no longer reaping.
// The drain then gave up at once and shutdown closed every descriptor with
// the ops still owed, the pre-celeris#793 behaviour the drain exists to end.
//
// Which kernels answer EBUSY is a matter of source, not of this host: the
// wait in io_cqring_wait returns it on v5.19 (the floor) and v6.1 ("if we
// can't even flush overflow, don't wait for more"), and no longer on v6.2 and
// later. v6.1's io_uring_enter ends with "return submitted ? submitted : ret",
// so there the error surfaces only when the enter submitted nothing; this
// file's fake answers EBUSY after the submit too, a superset of that
// contract, and the drain handles both; on this package's CI kernel and on
// the 7.0 kernel of the laptop's Docker VM a full CQ ring with completions overflowed behind
// it is flushed and the drain never sees the error (the "real-kernel" arm
// below pins that). So the failing-first arm injects the answer: ebusyWait873
// is the contract of those kernels, and the arms around it say what the
// injection must not be able to pass.

// fillCQ873 posts NOPs, never reaped, until the CQ ring is full and some
// completions have overflowed behind it, and returns how many it posted.
// Their user_data is 0, a tag the drain ignores.
func fillCQ873(t *testing.T, r *Ring, extra int) int {
	t.Helper()
	total := int(r.params.cqEntries) + extra
	for placed := 0; placed < total; {
		n := min(total-placed, int(r.params.sqEntries)/2)
		for range n {
			sqe := r.GetSQE()
			if sqe == nil {
				t.Fatalf("SQ ring full after %d NOPs", placed)
			}
			*(*uint8)(sqe) = opNOP
			*(*uint64)(unsafe.Add(sqe, 32)) = 0
		}
		if _, err := r.Submit(); err != nil {
			t.Fatalf("submit NOPs after %d: %v", placed, err)
		}
		placed += n
	}
	// The NOPs complete inline at submit; give task work a moment anyway.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		head, tail := r.BeginCQ()
		if tail-head >= r.params.cqEntries {
			return total
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("CQ ring not full after %d NOPs", total)
	return total
}

// ebusyWait873 is shutdownRingWait as a 5.19 to 6.1 kernel answers it: the
// pending SQEs are submitted, then, while the CQ ring is full, the wait is
// refused with EBUSY (the real call is made only once the ring has room, so a
// drain that does not reap the ring can never get past it). hold keeps the
// answer EBUSY whatever the ring holds. calls and ebusy count what the drain
// met.
type ebusyWait873 struct {
	hold         bool
	err          error // instead of EBUSY
	calls, ebusy int
}

func (f *ebusyWait873) wait(r *Ring, d time.Duration) error {
	f.calls++
	if _, err := r.Submit(); err != nil {
		return err
	}
	head, tail := r.BeginCQ()
	if f.hold || tail-head >= r.params.cqEntries {
		f.ebusy++
		if f.err != nil {
			return f.err
		}
		return fmt.Errorf("io_uring_enter submit+wait timeout: %w", unix.EBUSY)
	}
	return r.SubmitAndWaitTimeout(d)
}

// logBuf873 is a logger whose output a test can read.
type logBuf873 struct{ bytes.Buffer }

func newOwedRig873(t *testing.T, fill bool) (*fdlFixture, *logBuf873) {
	t.Helper()
	f := newFDLFixture(t, false)
	w := f.w
	lb := &logBuf873{}
	w.logger = slog.New(slog.NewTextHandler(lb, &slog.HandlerOptions{Level: slog.LevelWarn}))
	// The recv the connection is owed, placed and submitted for real.
	if !w.prepareRecv(f.cs, f.cs.buf) {
		t.Fatal("recv arm refused")
	}
	if _, err := w.ring.Submit(); err != nil {
		t.Fatalf("submit recv: %v", err)
	}
	if fill {
		fillCQ873(t, w.ring, 40)
	}
	if f.cs.kernelInflight != 1 {
		t.Fatalf("setup: kernelInflight=%d, want 1", f.cs.kernelInflight)
	}
	return f, lb
}

func setWait873(t *testing.T, fn func(*Ring, time.Duration) error) {
	t.Helper()
	old := shutdownRingWait
	shutdownRingWait = fn
	t.Cleanup(func() { shutdownRingWait = old })
}

// TestShutdownDrainSurvivesAFullCQRing873: the drain ends the owed recv
// whatever the CQ ring holds when it starts.
func TestShutdownDrainSurvivesAFullCQRing873(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fill     bool
		inject   bool
		wantBusy bool
	}{
		{"real-kernel/empty-cq", false, false, false},
		{"real-kernel/full-cq", true, false, false},
		{"ebusy-kernel/full-cq", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newOwedRig873(t, tc.fill)
			w := f.w
			var inj ebusyWait873
			if tc.inject {
				setWait873(t, inj.wait)
			}
			head, tail := w.ring.BeginCQ()
			t.Logf("celeris873 setup: cqEntries=%d inCQ=%d kernelInflight=%d", w.ring.params.cqEntries, tail-head, f.cs.kernelInflight)

			start := time.Now()
			w.endOwedOpsAtShutdown()
			took := time.Since(start)
			t.Logf("celeris873 drain: took=%v kernelInflight_after=%d waits=%d ebusy=%d gaveUp=%d",
				took.Round(time.Microsecond), f.cs.kernelInflight, inj.calls, inj.ebusy, w.handoffLoss.shutdownDrainGaveUp.Load())
			if tc.wantBusy && inj.ebusy == 0 {
				t.Fatal("the injected EBUSY never reached the drain: the test would prove nothing")
			}
			if f.cs.kernelInflight != 0 {
				t.Fatalf("the drain ended with %d op(s) still owed on fd %d", f.cs.kernelInflight, f.fd)
			}
			if g := w.handoffLoss.shutdownDrainGaveUp.Load(); g != 0 {
				t.Fatalf("shutdownDrainGaveUp=%d after a drain that finished", g)
			}
			if took > shutdownFDDrainBound873 {
				t.Fatalf("the drain took %v (bound %v)", took, shutdownFDDrainBound873)
			}
		})
	}
}

// shutdownFDDrainBound873: a drain that waits out its 250 ms bound did not
// finish its job.
const shutdownFDDrainBound873 = 150 * time.Millisecond

// TestShutdownDrainGiveUpIsCounted873: a drain that cannot end the owed op is
// bounded, counted and logged, whichever way it fails: a ring that answers
// EBUSY for ever ends at the bound (and costs no more than it), a ring that
// fails outright at once.
func TestShutdownDrainGiveUpIsCounted873(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inj     ebusyWait873
		minTook time.Duration
		maxTook time.Duration
		wantErr string
	}{
		{"ebusy-for-ever", ebusyWait873{hold: true}, 200 * time.Millisecond, 600 * time.Millisecond, "reason=bound"},
		{"ring-error", ebusyWait873{hold: true, err: errors.New("io_uring_enter submit+wait timeout: " + unix.EINVAL.Error())}, 0, 100 * time.Millisecond, "reason=ring_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, lb := newOwedRig873(t, false)
			w := f.w
			// One more op the kernel owes, which never completes (a recv
			// count with no SQE, as drainOwed880 does): the drain cannot
			// finish, so it ends by its bound or by the ring's failure.
			f.cs.recvArmed = true
			f.cs.kernelInflight++
			inj := tc.inj
			setWait873(t, inj.wait)
			start := time.Now()
			w.endOwedOpsAtShutdown()
			took := time.Since(start)
			t.Logf("celeris873 giveup case=%s took=%v waits=%d gaveUp=%d log=%q", tc.name, took.Round(time.Millisecond), inj.calls, w.handoffLoss.shutdownDrainGaveUp.Load(), strings.TrimSpace(lb.String()))
			if g := w.handoffLoss.shutdownDrainGaveUp.Load(); g != 1 {
				t.Fatalf("shutdownDrainGaveUp=%d, want 1", g)
			}
			if !strings.Contains(lb.String(), "level=WARN") || !strings.Contains(lb.String(), tc.wantErr) || !strings.Contains(lb.String(), "conns_owed=1") {
				t.Fatalf("no WARN naming the give-up (%s, conns_owed=1): %q", tc.wantErr, lb.String())
			}
			if hasErr := strings.Contains(lb.String(), "ring_err="); hasErr != (tc.name == "ring-error") {
				t.Fatalf("the WARN's ring_err field is present=%v on case %s: it names the ring's error, so only a ring failure has one: %q", hasErr, tc.name, lb.String())
			}
			if took < tc.minTook || took > tc.maxTook {
				t.Fatalf("took %v, want within [%v, %v]", took, tc.minTook, tc.maxTook)
			}
		})
	}
}
