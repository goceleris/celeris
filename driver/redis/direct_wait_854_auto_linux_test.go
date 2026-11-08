//go:build linux && !race

package redis

import (
	"context"
	"testing"
	"time"
)

// The two auto-mode tests of celeris#854 that need the in-process fake to
// answer quickly: under the race detector it is slow enough to cross the 300 us
// threshold legitimately, so they live in a !race file, which CI runs in the
// step "celeris#890 allocation guards and alloc-count tests, without -race".
// The mode is set through setDirectWaitForTest, never the environment.

// connDirectState reads the conn's average and switch under its writerMu. The
// client has one conn (pool size 1).
func connDirectState(t *testing.T, cl *Client) (ewma int64, on bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := cl.pool.acquireCmd(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.pool.releaseCmd(c)
	c.writerMu.Lock()
	defer c.writerMu.Unlock()
	return c.directWaitEWMA, c.directBlockOn
}

// TestDirectWaitFastRepliesDoNotBlock854: replies that come at once keep auto
// mode on the netpoll path. A loaded machine can starve the in-process fake
// into the odd slow reply, which moves the average over the threshold for a few
// Execs, so a quarter of the Execs may block (0 of 300 in nine of ten runs on 2
// CPUs with 4 busy loops, 17 in the tenth); "always blocks" would be 300.
func TestDirectWaitFastRepliesDoNotBlock854(t *testing.T) {
	const n = 300
	f := startWaitFake(t)
	cl := newWaitClient(t, f.addr(), 1)
	setDirectWaitForTest(t, directWaitAuto)
	execN(t, cl, 20, 1)
	before := directWaitSnapshot()
	execN(t, cl, n, 1)
	d := directWaitSnapshot().since(before)
	t.Logf("%d fast Execs -> %+v", n, d)
	if d.Blocks > n/4 {
		t.Errorf("blocks = %d over %d fast Execs, want at most %d", d.Blocks, n, n/4)
	}
}

// TestDirectWaitHysteresis854: a conn that was blocking stops when the replies
// speed up, and goes back to blocking when they slow down again. The slow
// phases are deterministic (the reply takes at least 2 ms, so the average is
// over the threshold whatever the load); the fast phase runs until the conn's
// switch is off, up to a bound, because how fast an in-process reply is
// depends on the machine, and a loaded one can push the average back over the
// threshold within a few Execs (the latch of TestDirectWaitLatch854), so the
// test asserts the switch turned off, not how long it stayed off.
func TestDirectWaitHysteresis854(t *testing.T) {
	f := startWaitFake(t)
	f.setDelay(2 * time.Millisecond)
	cl := newWaitClient(t, f.addr(), 1)
	setDirectWaitForTest(t, directWaitAuto)
	execN(t, cl, 6, 1)
	if _, on := connDirectState(t, cl); !on {
		t.Fatal("slow phase: the conn is not blocking after 6 slow Execs")
	}
	before := directWaitSnapshot()
	execN(t, cl, 5, 1)
	if d := directWaitSnapshot().since(before); d.Blocks < 3 {
		t.Fatalf("slow phase: blocks = %d over 5 Execs, want >= 3", d.Blocks)
	}

	f.setDelay(0)
	off := false
	for round := 0; round < 10 && !off; round++ {
		execN(t, cl, 40, 1) // the average falls from ~2 ms towards the in-process round trip
		_, on := connDirectState(t, cl)
		off = !on
	}
	if !off {
		ewma, _ := connDirectState(t, cl)
		t.Fatalf("fast phase: the conn still blocks after 400 fast Execs, average %v", time.Duration(ewma))
	}
	f.setDelay(2 * time.Millisecond)
	execN(t, cl, 6, 1)
	if _, on := connDirectState(t, cl); !on {
		t.Fatal("slow again: the conn is not blocking after 6 slow Execs")
	}
	before = directWaitSnapshot()
	execN(t, cl, 5, 1)
	if d := directWaitSnapshot().since(before); d.Blocks < 3 {
		t.Fatalf("slow again: blocks = %d over 5 Execs, want >= 3", d.Blocks)
	}
}
