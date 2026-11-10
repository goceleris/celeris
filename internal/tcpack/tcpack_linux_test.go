//go:build linux

package tcpack

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stub installs probe as the host probe for the test, with the latch cleared and
// the probe not yet started. The cleanup waits for the probe goroutine to end, so
// that a late answer cannot reach the next test.
func stub(t *testing.T, probe func() bool) {
	t.Helper()
	probing.Wait()
	filled.Store(false)
	started = sync.Once{}
	probeFn = probe
	t.Cleanup(func() {
		probing.Wait()
		filled.Store(false)
		started = sync.Once{}
		probeFn = probeLoopback
	})
}

func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	fin := make(chan struct{})
	go func() { f(); close(fin) }()
	select {
	case <-fin:
	case <-time.After(d):
		t.Fatalf("%s blocked for more than %v", what, d)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 400 {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within 2 s", what)
}

// This host's stack fills bytes_acked (Linux since 4.1), and the probe sees it.
func TestProbeSeesTheHostStack889(t *testing.T) {
	if !probeLoopback() {
		t.Fatal("the loopback probe found no acknowledged bytes on a Linux stack")
	}
}

// The real probe, started in the background, latches the answer.
func TestStartLatchesTheHostAnswer889(t *testing.T) {
	stub(t, probeLoopback)
	Start()
	eventually(t, "the background probe's answer", filled.Load)
	if !Filled() || !Usable(0, nil) {
		t.Fatal("0 on a stack that fills the field was not a count")
	}
}

// A 0 is a count only once the stack is known to fill the field; an error never
// is; a count above 0 is, and latches.
func TestUsableLatch889(t *testing.T) {
	var probes atomic.Int32
	stub(t, func() bool { probes.Add(1); return false }) // a stack that does not fill it

	if Usable(0, errors.New("getsockopt")) {
		t.Fatal("a failed read was usable")
	}
	for range 3 {
		if Usable(0, nil) {
			t.Fatal("0 on a stack that does not fill the field was a count")
		}
	}
	probing.Wait()
	if probes.Load() != 1 {
		t.Fatalf("the probe ran %d times, want once", probes.Load())
	}
	if !Usable(4096, nil) {
		t.Fatal("a count above 0 was not usable")
	}
	if !Usable(0, nil) {
		t.Fatal("after a socket reported a count, 0 stayed unusable (the latch is one-way)")
	}
	if Usable(0, errors.New("x")) {
		t.Fatal("an error is never usable")
	}
}

// Note latches and nothing unlatches.
func TestNoteLatches889(t *testing.T) {
	stub(t, func() bool { return false })
	Note(0)
	if filled.Load() {
		t.Fatal("a count of 0 latched")
	}
	Note(1)
	if !filled.Load() || !Filled() {
		t.Fatal("a count above 0 did not latch")
	}
	Note(0)
	if !Filled() {
		t.Fatal("the latch went back")
	}
}

// A probe that is slow (a broken loopback) must not block the thread that asks:
// Usable and Filled answer at once, false, however many callers there are, and the
// latch is set when the probe completes later.
func TestSlowProbeNeverBlocksTheCaller889(t *testing.T) {
	release := make(chan struct{})
	stub(t, func() bool { <-release; return true })

	within(t, time.Second, "Usable(0, nil) with a probe that has not completed", func() {
		if Usable(0, nil) {
			t.Error("0 was a count before the probe said so")
		}
	})
	var wg sync.WaitGroup
	for range 8 { // other loops asking meanwhile
		wg.Add(1)
		go func() {
			defer wg.Done()
			if Filled() || Usable(0, nil) {
				t.Error("answered true before the probe completed")
			}
		}()
	}
	within(t, time.Second, "concurrent Filled/Usable calls behind a slow probe", wg.Wait)

	close(release)
	eventually(t, "the latch after the probe completed", filled.Load)
	if !Usable(0, nil) {
		t.Fatal("0 was not a count after the probe completed")
	}
}

// A probe that never completes leaves 0 as "no count", without blocking, for good.
func TestNeverCompletingProbeLeavesFailOpen889(t *testing.T) {
	release := make(chan struct{})
	stub(t, func() bool { <-release; return false })
	t.Cleanup(func() { close(release) }) // runs before stub's cleanup (LIFO): lets the probe end
	for range 5 {
		within(t, time.Second, "Usable(0, nil)", func() {
			if Usable(0, nil) {
				t.Error("0 was a count with a probe that never completes")
			}
		})
	}
	if !Usable(7, nil) {
		t.Fatal("a count above 0 must still be usable")
	}
}
