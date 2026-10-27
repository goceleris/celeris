//go:build linux

package redis

// celeris#887 item 29: watchFailover waited on sc.subPS.Channel(), which a
// conn drop does not close: the sentinel's PubSub reconnected to the same
// address for ever. A sentinel that went away after a successful
// Subscribe("+switch-master") was never replaced by the next address of
// SentinelAddrs, and the failovers it would have announced were missed.
//
// Both shapes of "went away" are tested: the sentinel is gone for good (its
// listener closed), and the sentinel accepts every reconnect and hangs up at
// once, so its PubSub is never down for long.

import (
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func (fs *fakeSentinel) subscribers() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.subWriters)
}

// dropAll closes every conn the sentinel has accepted so far.
func (fs *fakeSentinel) dropAll() {
	fs.fake.mu.Lock()
	conns := append([]net.Conn(nil), fs.fake.conns...)
	fs.fake.conns = nil
	fs.fake.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// flapEvery is the period of the hang-ups of the sentinel that stays up; 0
// means the sentinel is gone for good.
// reconnectLoops counts the PubSub reconnect goroutines of the process.
func reconnectLoops() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	return strings.Count(string(buf), "(*PubSub).reconnectLoop")
}

func sentinelMovesOn887(t *testing.T, flapEvery time.Duration) {
	goneForGood := flapEvery == 0
	oldGrace := sentinelDownGrace
	sentinelDownGrace = 600 * time.Millisecond
	t.Cleanup(func() { sentinelDownGrace = oldGrace })

	master1 := startFakeMaster(t)
	master2 := startFakeMaster(t)
	a := startFakeSentinel(t, master1.Addr())
	b := startFakeSentinel(t, master1.Addr())

	sc, err := NewSentinelClient(SentinelConfig{
		MasterName:    "mymaster",
		SentinelAddrs: []string{a.Addr(), b.Addr()},
		DialTimeout:   2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSentinelClient: %v", err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	waitFor(t, "the client to subscribe to the first sentinel", 5*time.Second, func() bool { return a.subscribers() >= 1 })
	if b.subscribers() != 0 {
		t.Fatalf("fixture: the second sentinel has %d subscribers before the first went away", b.subscribers())
	}

	loopsBefore := reconnectLoops()

	// The first sentinel goes away after the client subscribed to it.
	stop := make(chan struct{})
	var stopped atomic.Bool
	t.Cleanup(func() {
		if stopped.CompareAndSwap(false, true) {
			close(stop)
		}
	})
	if goneForGood {
		_ = a.fake.ln.Close()
		a.dropAll()
	} else {
		go func() {
			for {
				select {
				case <-stop:
					return
				case <-time.After(flapEvery):
					a.dropAll()
				}
			}
		}()
	}

	// The client must move on to the second sentinel ...
	waitFor(t, "the client to subscribe to the next sentinel after the first went away", 8*time.Second, func() bool { return b.subscribers() >= 1 })

	// The sentinel that was given up must not leave its PubSub reconnecting
	// for ever, against a closed pool if it no longer reaches the sentinel: no
	// reconnect goroutine is left from it ...
	waitFor(t, "the abandoned sentinel's PubSub reconnect loop to exit", 5*time.Second, func() bool { return reconnectLoops() <= loopsBefore })
	// ... and a sentinel that still accepts sees no new SUBSCRIBE from this
	// client once it has moved on (the backoff of the abandoned PubSub is long
	// by then, so the window is 2.5 s).
	if !goneForGood {
		close(stop)
		stopped.Store(true)
		time.Sleep(50 * time.Millisecond)
		before := a.subscribers()
		time.Sleep(2500 * time.Millisecond)
		if after := a.subscribers(); after != before {
			t.Errorf("the sentinel that was given up got %d more SUBSCRIBEs after the client moved on: its PubSub is still reconnecting", after-before)
		}
	}

	// ... and see the failover that sentinel announces.
	b.SetMaster(master2.Addr())
	waitFor(t, "the client to follow the failover announced by the next sentinel", 5*time.Second, func() bool {
		if err := sc.Set(t.Context(), "after", "2", 0); err != nil {
			return false
		}
		master2.mem.mu.Lock()
		defer master2.mem.mu.Unlock()
		return master2.mem.kv["after"] == "2"
	})
}

func TestSentinelMovesOnWhenItsSentinelIsGone887(t *testing.T) {
	sentinelMovesOn887(t, 0)
}

func TestSentinelMovesOnWhenItsSentinelHangsUpAfterEveryReplay887(t *testing.T) {
	sentinelMovesOn887(t, 5*time.Millisecond)
}

// The conn lives 200 ms between hang-ups, so a check that looks at "is the
// conn up now" sees a healthy PubSub about as often as a down one.
func TestSentinelMovesOnWhenItsConnKeepsDyingAfterAWhile887(t *testing.T) {
	sentinelMovesOn887(t, 200*time.Millisecond)
}
