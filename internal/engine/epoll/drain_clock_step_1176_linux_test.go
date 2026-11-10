//go:build linux

package epoll

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/drainclock"
)

// celeris#1176 on epoll. The send drain's end (sendDrainWait) and the pool
// wait's (poolWaitSettled, h2PoolSettled) are time.Time values taken from
// time.Now() and the budget's Deadline(), compared with Before, After and
// time.Until: they use the monotonic reading both carry, so a step of the wall
// clock cannot end them early. What read the wall clock were the pacers of the
// shutdown's gate, producersStopNext and h1InProgressNext (time.Now().UnixNano()):
// a step backward held the next walk of the gate, and the cached "a request is
// in progress" of the wait's look, for as long as the step. These tests step
// the wall clock of the drainclock seam backward by a minute and a quarter,
// which is what a VM clock sync does after the host slept. A forward step
// makes a pacer fire early, which costs one more walk and nothing else, so it
// has no test.

var clockEpoch1176 = time.Now()

// stepClock1176 installs a clock whose wall reading is the real one plus an
// offset the test sets, and whose monotonic reading is the real one.
func stepClock1176(t *testing.T) (step func(time.Duration)) {
	t.Helper()
	var off atomic.Int64
	r := drainclock.SetSeam(func() drainclock.Reading {
		now := time.Now()
		return drainclock.Reading{Wall: now.UnixNano() + off.Load(), Mono: int64(now.Sub(clockEpoch1176)) + 1}
	})
	t.Cleanup(r)
	return func(d time.Duration) { off.Store(int64(d)) }
}

// The wait's look at HTTP/1 requests in progress answers "yes" for one poll
// (h2PoolDrainPollMs) and is asked again after it, whatever the wall clock did
// meanwhile: a request that ended is seen to have ended.
func TestH1InProgressPacingIgnoresABackwardClockStep1176(t *testing.T) {
	step := stepClock1176(t)
	r := newPromoted1165(t)
	r.cs.asyncPromoted = false
	_ = conn.ProcessH1(context.Background(), []byte("POST /x HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\nabc"), r.cs.h1State, r.l.handler, func([]byte) {})
	if !r.l.h1InProgress() {
		t.Fatal("premise: a request with a partial body is not in progress")
	}
	r.cs.h1State = conn.NewH1State() // the request ends
	step(-75 * time.Second)
	time.Sleep(3 * h2PoolDrainPollMs * time.Millisecond)
	if r.l.h1InProgress() {
		t.Fatalf("the look still answers yes %v after its poll, with the wall clock stepped back: the pacer is read from it",
			3*h2PoolDrainPollMs*time.Millisecond)
	}
}

// The gate walks the conns again once its poll has passed (poolWaitGate), as
// the wait's pacer says, whatever the wall clock did: producersStopNext moves
// on the second pass.
func TestPoolWaitGatePacingIgnoresABackwardClockStep1176(t *testing.T) {
	step := stepClock1176(t)
	r := newPromoted1165(t)
	r.cs.asyncPromoted = false
	_ = conn.ProcessH1(context.Background(), []byte("POST /x HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\nabc"), r.cs.h1State, r.l.handler, func([]byte) {})
	r.l.h2DrainStart = time.Now().Add(-2 * poolWaitGrace)
	r.l.poolWaitGate()
	first := r.l.producersStopNext
	if first == 0 {
		t.Fatal("premise: the gate's first pass did not walk")
	}
	step(-75 * time.Second)
	time.Sleep(3 * h2PoolDrainPollMs * time.Millisecond)
	r.l.poolWaitGate()
	if r.l.producersStopNext == first {
		t.Fatalf("the gate did not walk again %v after its poll, with the wall clock stepped back: the pacer is read from it",
			3*h2PoolDrainPollMs*time.Millisecond)
	}
}
