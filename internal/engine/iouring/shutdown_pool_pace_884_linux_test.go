//go:build linux

package iouring

import (
	"fmt"
	"sync"
	"testing"

	"github.com/goceleris/celeris/internal/conn"
)

// celeris#884 item 50, review round 1: the pool wait's gate walked every live
// connection on every pass of the loop, for as long as the HTTP/2 handlers
// ran (a serving phase, unlike the send drain: traffic goes on at its full
// rate). stopProducersPaced is that walk at the pool wait's own cadence,
// h2PoolDrainPoll: a producer whose lock was busy is tried again within it,
// and no pass pays the walk twice in a poll.

// TestStopProducersPacedRunsOncePerPoll884: the first call walks; a call
// within h2PoolDrainPoll of it does not (the conn it would end is left alone);
// one a poll later does, and ends it.
func TestStopProducersPacedRunsOncePerPoll884(t *testing.T) {
	w, cs := detachStrandRig806(t, nil)
	t0 := int64(1_000_000_000)

	cs.detachMu.Lock() // a handler running: the first walk's TryLock fails
	if !w.stopProducersPaced(t0) {
		t.Fatal("the first call did not walk")
	}
	cs.detachMu.Unlock()
	if cs.detachClosed {
		t.Fatal("setup: the walk ended a conn whose lock was held")
	}

	if w.stopProducersPaced(t0 + int64(h2PoolDrainPoll)/2) {
		t.Error("a call half a poll after a walk walked again")
	}
	if cs.detachClosed {
		t.Error("a paced-out call ended the conn's writes")
	}

	if !w.stopProducersPaced(t0 + int64(h2PoolDrainPoll)) {
		t.Fatal("a call a full poll after a walk did not walk: a conn whose lock was busy would never be tried again")
	}
	if !cs.detachClosed {
		t.Error("the walk a poll later did not end the conn's writes")
	}
}

// BenchmarkStopDetachedProducers884 is one pass of the walk over n live
// connections that are all detached with their writes already ended, which is
// what every pass after the first costs while the HTTP/2 handlers run: a
// TryLock and an unlock per conn. Multiply by the passes the pool wait makes
// per second (unpaced: one per loop pass) or 1000/h2PoolDrainPoll (paced).
func BenchmarkStopDetachedProducers884(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("conns=%d", n), func(b *testing.B) {
			w := &Worker{listenFD: -1, conns: make([]*connState, n+16)}
			for fd := 8; fd < n+8; fd++ {
				cs := &connState{fd: fd, liveIdx: -1, detachMu: &sync.Mutex{}, h1State: &conn.H1State{}, detachClosed: true}
				cs.h1State.Detached.Store(true)
				w.conns[fd] = cs
				w.addLiveConn(cs)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				w.stopDetachedProducers()
			}
		})
	}
}
