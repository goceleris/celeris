//go:build linux

package iouring

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// The deterministic form of TestWriteClockStampAfterAQuietGapIsFresh1173
// (celeris#1173). On the worker's thread, right after a ring wait that returned
// no completion (ringWaitSeam), it ages the worker's clock by a second (what a
// long quiet wait leaves) and queues a PING on the conn's write queue, as a
// handler's write that landed after the loop looked for completions would; the
// drain of that very iteration stages the SEND, and h2WriteDrainSeam reads the
// age of the clock the write clock is stamped from. Twenty trials, the first at
// whatever the idle worker's empty-iteration count has reached and the others
// at 1, so a refresh gated on the count (>= 2 and up) is caught as well as none.
//
// A trial in which the eventfd's poll completion is already visible after the
// enqueue is not counted: that iteration has a completion, and its batch reads
// the clock. A ring without DEFER_TASKRUN (the base tier) posts that completion
// at once, so there no trial counts and the test asserts nothing; the unit test
// of readClockAfterWait below is what holds the rule on that tier.
func TestWriteClockStampAfterAnEmptyWaitIsFreshDeterministic1173(t *testing.T) {
	var (
		armed    atomic.Bool
		probing  atomic.Bool
		age      atomic.Int64
		iters    atomic.Int64
		visible  atomic.Int64
		resultCh = make(chan struct{}, 1)
	)
	oldWait := ringWaitSeam
	ringWaitSeam = func(w *Worker, _ ringWaitMode, err error) error {
		if err != nil || !armed.Load() || len(w.h2Conns) == 0 {
			return err
		}
		if h, tl := w.ring.BeginCQ(); h != tl {
			return err // this wait has a completion: not the case under test
		}
		cs := w.conns[w.h2Conns[0]]
		if cs == nil || cs.h2State == nil || cs.h2State.WriteQueuePending() {
			return err
		}
		if !armed.CompareAndSwap(true, false) {
			return err
		}
		iters.Store(int64(w.emptyIters))
		w.cachedNow -= int64(time.Second)
		probing.Store(true)
		// A PING frame (stream 0, 8 opaque bytes).
		cs.h2State.EnqueueWrite(0, []byte{0, 0, 8, 6, 0, 0, 0, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8})
		if h, tl := w.ring.BeginCQ(); h != tl {
			visible.Add(1)
		}
		return err
	}
	seam := func(w *Worker, _ int) {
		if probing.CompareAndSwap(true, false) {
			age.Store(time.Now().UnixNano() - w.cachedNow)
			select {
			case resultCh <- struct{}{}:
			default:
			}
		}
	}
	h2WriteDrainSeam.Store(&seam)
	t.Cleanup(func() { ringWaitSeam = oldWait; h2WriteDrainSeam.Store(nil) })

	g := &gate1173{started: make(chan struct{}, 1), gate: make(chan struct{})}
	run, addr := startGate1173(t, g)
	defer func() { _, _ = run.stop() }()

	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(60 * time.Second))
	if _, err := c.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(c, c)
	if err := fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	pings := make(chan struct{}, 8)
	readErr := make(chan error, 1)
	go func() {
		for {
			f, err := fr.ReadFrame()
			if err != nil {
				readErr <- err
				return
			}
			if pf, ok := f.(*http2.PingFrame); ok && !pf.IsAck() {
				pings <- struct{}{}
			}
		}
	}()
	time.Sleep(50 * time.Millisecond) // the SETTINGS exchange

	const trials = 20
	var worst time.Duration
	counted := 0
	for i := range trials {
		v0 := visible.Load()
		armed.Store(true)
		select {
		case <-resultCh:
		case err := <-readErr:
			t.Fatalf("trial %d: the conn ended (%v) before the probe fired", i, err)
		case <-time.After(3 * time.Second):
			t.Fatalf("trial %d: the probe never fired", i)
		}
		a := time.Duration(age.Load())
		select {
		case <-pings:
		case err := <-readErr:
			t.Fatalf("trial %d: the stamp was %v old and the conn ended (%v) before the PING arrived", i, a, err)
		case <-time.After(3 * time.Second):
			t.Fatalf("trial %d: no PING", i)
		}
		if visible.Load() != v0 {
			t.Logf("trial %d: the poll completion was visible after the enqueue (not counted), age %v", i, a)
			continue
		}
		counted++
		t.Logf("trial %d: emptyIters %d, age of the stamp %v", i, iters.Load(), a)
		worst = max(worst, a)
	}
	t.Logf("%d of %d trials counted, worst age %v", counted, trials, worst)
	if worst > writeTimeout1173/2 {
		t.Fatalf("the write clock of a response staged by the iteration of an empty wait starts %v in the past (WriteTimeout %v)", worst, writeTimeout1173)
	}
}

// readClockAfterWait reads the clock when the wait returned no completion,
// whatever the worker's empty-iteration count: the tier-independent form of the
// test above.
func TestReadClockAfterWaitIsFreshWithNoCompletion1173(t *testing.T) {
	for _, iters := range []uint32{0, 1, 2, 5, 10, 50, 100, 150, 200} {
		w := &Worker{emptyIters: iters}
		stale := time.Now().UnixNano() - int64(time.Second)
		w.cachedNow = stale
		w.readClockAfterWait(false)
		if age := time.Duration(time.Now().UnixNano() - w.cachedNow); age > 100*time.Millisecond {
			t.Errorf("emptyIters %d: the clock is %v old after a wait with no completion", iters, age)
		}
	}
}
