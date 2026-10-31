//go:build linux

package epoll

import (
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/resource"
)

// celeris#1194. The sweep timer ticks every 25 ms on every loop, and a tick is
// an event of the epoll_wait it ends. Counted as activity it reset the adaptive
// backoff of the wait (adaptiveTimeoutMs) each time, so an idle loop never got
// past a few waits of 2 ms: 675 wakeups a second, against 215 for a loop with
// no timer. A round whose only event is the tick is an idle round now.
//
// The sweep runs on the loop's thread right at the tick, before the round is
// counted, so the counter it sees is the idle rounds since the last activity:
// a few (under 20) when each tick starts the backoff over, hundreds when the
// loop settles into its longest wait (above 100 rounds).
func TestSweepTickAloneIsAnIdleRound1194(t *testing.T) {
	var (
		mu   sync.Mutex
		most = map[*Loop]uint32{}
	)
	hook := func(l *Loop) {
		mu.Lock()
		if l.consecutiveEmpty > most[l] {
			most[l] = l.consecutiveEmpty
		}
		mu.Unlock()
	}
	testHookCheckTimeouts.Store(&hook)
	t.Cleanup(func() { testHookCheckTimeouts.Store(nil) })

	eng, _, _ := start1194(t, func(c *resource.Config) { c.ReadHeaderTimeout = 0 })
	time.Sleep(1500 * time.Millisecond)

	for i := 0; i < eng.NumWorkers(); i++ {
		l := eng.WorkerLoop(i).(*Loop)
		mu.Lock()
		got := most[l]
		mu.Unlock()
		t.Logf("loop %d: at most %d idle rounds in a row at a tick of the sweep timer in 1.5 s", i, got)
		if got <= 100 {
			t.Errorf("loop %d: %d idle rounds in a row at a tick, want more than 100 (the longest wait): the tick of the sweep timer starts the backoff of an idle loop over", i, got)
		}
	}
}
