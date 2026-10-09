//go:build linux

package epoll

// celeris#865: checkTimeouts read a conn's h1State (the Detached test, the idle
// deadline, the header deadline, and csWritePending on a detached conn) with no
// lock, while the dispatch goroutine's switchToH2Local sets cs.h1State = nil
// under cs.detachMu at an h2c upgrade. A data race, and a nil dereference
// between the pointer test and the dereference. The race tier reported it on
// kitchen_sink/epoll (probatorium run 37969571447, switchToH2Local loop.go:2248
// against checkTimeouts loop.go:3323, 2 reports in that run).
//
// The tests below drive checkTimeouts on the test goroutine, standing in for
// the loop thread, against a goroutine that performs switchToH2Local's writes
// in its order under the lock it holds, the precedent being
// TestSweepClassifiesUnderTheLockTheEngineRequires (sweep_r2_test.go).

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
)

// wantFlips865 is the floor of mutations the writer goroutine must have made
// before a race arm may pass: a fixed number of sweeps can starve it, and a
// run in which the two never interleaved proves nothing.
const wantFlips865 = 200

// raceArm865 runs checkTimeouts on this goroutine against writer, which
// mutates cs under cs.detachMu until stop is closed. prep runs before each
// sweep (loop-thread work). It fails when the writer ran fewer than
// wantFlips865 times, and reports how many sweeps ran.
func raceArm865(t *testing.T, l *Loop, prep func(), writer func(stop <-chan struct{}, flips *atomic.Int64)) {
	t.Helper()
	stop := make(chan struct{})
	var flips atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		writer(stop, &flips)
	}()
	deadline := time.Now().Add(20 * time.Second)
	sweeps := 0
	for flips.Load() < wantFlips865 && time.Now().Before(deadline) {
		if prep != nil {
			prep()
		}
		l.checkTimeouts()
		sweeps++
		runtime.Gosched() // never starve the writer this test races
	}
	close(stop)
	wg.Wait()
	if got := flips.Load(); got < wantFlips865 {
		t.Fatalf("celeris865 RACE PREMISE: the writer ran %d times in %d sweeps, want %d; the injection did "+
			"not fire often enough for the read and the write to interleave", got, sweeps, wantFlips865)
	}
	t.Logf("celeris865 sweeps=%d writer_flips=%d", sweeps, flips.Load())
}

// TestCheckTimeoutsReadsH1StateUnderDetachMu is the issue's race: the writer
// is the dispatch goroutine at an h2c upgrade, running while the sweep looks at
// the conn. The dispatch goroutine is RUNNING (inside a handler), the only
// state switchToH2Local runs in. No timeout is configured and no deadline set,
// so nothing is closed on the fixed code.
func TestCheckTimeoutsReadsH1StateUnderDetachMu(t *testing.T) {
	l := newLedgerLoop(t)
	_, cs := asyncConn(t, l)
	cs.asyncRun = true
	cs.asyncParked = false
	h1 := cs.h1State
	raceArm865(t, l, nil, func(stop <-chan struct{}, flips *atomic.Int64) {
		for {
			select {
			case <-stop:
				cs.detachMu.Lock()
				cs.h1State = h1
				cs.h2State = nil
				cs.protocol = engine.HTTP1
				cs.detachMu.Unlock()
				return
			default:
			}
			// switchToH2Local's writes, in its order, under its lock.
			cs.detachMu.Lock()
			cs.h1State = nil
			cs.h2State = &conn.H2State{}
			cs.protocol = engine.H2C
			cs.detachMu.Unlock()
			cs.detachMu.Lock()
			cs.h1State = h1
			cs.h2State = nil
			cs.protocol = engine.HTTP1
			cs.detachMu.Unlock()
			flips.Add(1)
		}
	})
	if l.conns[cs.fd] != cs || l.closeCount.Load() != 0 {
		t.Fatalf("celeris865: the sweep closed a conn with no deadline set (closeCount=%d)", l.closeCount.Load())
	}
}

// TestCheckTimeoutsReadsDetachedWriteStateUnderDetachMu is the same defect on
// the detached branch: once a WS/SSE conn's idle deadline has passed,
// checkTimeouts asks csWritePending, which reads writeBuf/writePos that a
// middleware's guarded writeFn appends to under detachMu. The holder is the
// bounded kind (asyncDetachUnlocked set, as after a Detach), so the sweep may
// skip a pass it cannot lock. The writer keeps writeBuf non-empty, so a
// correct sweep never closes the conn and the test needs no engine.
func TestCheckTimeoutsReadsDetachedWriteStateUnderDetachMu(t *testing.T) {
	l := newLedgerLoop(t)
	_, cs := asyncConn(t, l)
	cs.asyncRun = true
	cs.asyncDetachUnlocked = true
	cs.h1State.Detached.Store(true)
	cs.h1State.IdleDeadlineNs.Store(1) // expired, so csWritePending is asked
	cs.writeBuf = append(cs.writeBuf[:0], 'x')
	raceArm865(t, l, func() { cs.drainDeadline = 0 }, func(stop <-chan struct{}, flips *atomic.Int64) {
		for {
			select {
			case <-stop:
				return
			default:
			}
			cs.detachMu.Lock() // a guarded writeFn's append
			cs.writeBuf = append(cs.writeBuf[:0], 'y', 'z')
			cs.detachMu.Unlock()
			flips.Add(1)
		}
	})
	if l.conns[cs.fd] != cs || l.closeCount.Load() != 0 {
		t.Fatalf("celeris865: the sweep closed a detached conn that still had bytes queued (closeCount=%d)",
			l.closeCount.Load())
	}
}

// TestCheckTimeoutsDoesNotWaitForTheLock is the second control: the race test
// alone is passed by a blocking Lock, which parks the loop thread behind one
// slow handler (celeris#593, #669). With detachMu held by a bounded holder the
// sweep must return at once and close nothing, even with every deadline
// expired; with it held by a running handler it must return at once and still
// apply the conn's timeouts (the reap is owed to the handler, not skipped).
func TestCheckTimeoutsDoesNotWaitForTheLock(t *testing.T) {
	t.Run("bounded holder skips", func(t *testing.T) {
		rig := hijackRaceConn(t) // ReadTimeout 1 ms, lastActivity an hour old
		l, cs, local := rig.l, rig.cs, rig.local
		cs.asyncInMu.Lock()
		cs.asyncDetachUnlocked = true // a post-Detach holder: one guarded write
		cs.asyncInMu.Unlock()
		release := holdAsHandler(t, cs, true)
		if !returnsWhileHeld(t, release, l.checkTimeouts) {
			t.Fatalf("celeris865: checkTimeouts waited %v on a detachMu held by a bounded holder", stallWait)
		}
		if l.conns[local] != cs || cs.asyncClosed.Load() {
			t.Fatalf("celeris865: the sweep acted on a conn whose state it could not read")
		}
		// Skipped for one sweep, not for good: once the holder is done the next
		// sweep reaps the conn (its read timeout has long expired).
		release()
		l.checkTimeouts()
		if l.conns[local] != nil || l.closeCount.Load() != 1 {
			t.Fatalf("celeris865: the sweep after the holder released did not reap the expired conn: slot=%p closeCount=%d",
				l.conns[local], l.closeCount.Load())
		}
	})
	t.Run("running handler reaps", func(t *testing.T) {
		rig := hijackRaceConn(t)
		l, cs := rig.l, rig.cs
		release := holdAsHandler(t, cs, true)
		if !returnsWhileHeld(t, release, l.checkTimeouts) {
			t.Fatalf("celeris865: checkTimeouts waited %v on a detachMu held by a running handler", stallWait)
		}
		if !cs.asyncClosed.Load() {
			t.Fatalf("celeris865: the expired read timeout of a conn in a handler was not applied (no close owed)")
		}
	})
}

// TestSnapshotH1DeadlinesCases pins what the snapshot returns for each lock
// state, and that it never leaves the lock held (the caller's closeConn takes
// the same non-reentrant mutex).
func TestSnapshotH1DeadlinesCases(t *testing.T) {
	newConn := func(detached bool) *connState {
		cs := acquireConnState(t.Context(), 7, 64, true)
		cs.h1State = conn.NewH1State()
		cs.h1State.Detached.Store(detached)
		cs.h1State.IdleDeadlineNs.Store(111)
		cs.h1State.HeaderDeadlineNs.Store(222)
		return cs
	}
	t.Run("lock free", func(t *testing.T) {
		cs := newConn(true)
		cs.writeBuf = append(cs.writeBuf[:0], 'x')
		snap, ok := snapshotH1Deadlines(cs)
		want := h1DeadlineSnapshot{detached: true, idleDL: 111, writePending: true}
		if !ok || snap != want {
			t.Fatalf("snapshot = %+v ok=%v, want %+v ok=true", snap, ok, want)
		}
		if !cs.detachMu.TryLock() {
			t.Fatal("the snapshot returned with detachMu still held")
		}
		cs.detachMu.Unlock()
	})
	t.Run("not detached reads no write state", func(t *testing.T) {
		cs := newConn(false)
		cs.writeBuf = append(cs.writeBuf[:0], 'x')
		snap, ok := snapshotH1Deadlines(cs)
		if !ok || snap.detached || snap.writePending || snap.idleDL != 0 || snap.hdrDL != 222 {
			t.Fatalf("snapshot = %+v ok=%v", snap, ok)
		}
	})
	t.Run("h2 conn", func(t *testing.T) {
		cs := newConn(false)
		cs.h1State = nil // switchToH2Local ran
		snap, ok := snapshotH1Deadlines(cs)
		if !ok || snap != (h1DeadlineSnapshot{}) {
			t.Fatalf("snapshot of an h2 conn = %+v ok=%v, want zero ok=true", snap, ok)
		}
	})
	t.Run("sync conn has no lock", func(t *testing.T) {
		cs := newConn(false)
		cs.detachMu = nil
		snap, ok := snapshotH1Deadlines(cs)
		if !ok || snap.hdrDL != 222 {
			t.Fatalf("snapshot = %+v ok=%v", snap, ok)
		}
	})
	t.Run("running handler holds the lock", func(t *testing.T) {
		cs := newConn(false)
		release := holdAsHandler(t, cs, true)
		defer release()
		snap, ok := snapshotH1Deadlines(cs)
		if !ok || snap != (h1DeadlineSnapshot{}) {
			t.Fatalf("snapshot = %+v ok=%v, want zero ok=true (nothing read)", snap, ok)
		}
	})
	t.Run("bounded holder holds the lock", func(t *testing.T) {
		cs := newConn(true)
		release := holdAsHandler(t, cs, false) // parked: whoever holds it is a guarded write
		defer release()
		if _, ok := snapshotH1Deadlines(cs); ok {
			t.Fatal("snapshot reported ok while a bounded holder had the lock")
		}
	})
}
