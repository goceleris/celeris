//go:build linux

package redis

// celeris#887 item 29, review round 1 of the PR: the give-up of watchFailover
// closed the sentinel's client outside sentinelMu while that sentinel was still
// listed, so SentinelClient.Close could close the same Client at the same time.
// redisPool.Close is not safe for that (it reads and writes p.provider with no
// lock and then releases the shared standalone loop), and the sentinel's
// clients, the master client and any other client of the process share that
// loop's refcount. Close racing a subscribeLoop that had already passed its
// closed check also left a freshly connected sentinel open for ever (pre-
// existing).

import (
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

// Two goroutines close one Client. Under -race this is a data race on
// p.provider unless Close is idempotent under concurrency, and a double
// Release of the loop otherwise.
func TestClientCloseTwiceConcurrently887(t *testing.T) {
	fake := startFakeRedis(t, nil)
	// A bystander on the same standalone loop must keep its conn.
	by := startHangUpServer(t, func(int) time.Duration { return 0 })
	ps := subscribeToHangUpServer(t, by)
	conn0 := ps.connNow()
	for i := 0; i < 50; i++ {
		cl, err := NewClient(fake.Addr(), WithForceRESP2())
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_ = cl.Close()
			}()
		}
		close(start)
		wg.Wait()
	}
	time.Sleep(100 * time.Millisecond)
	if c := ps.connNow(); c != conn0 || c.closed.Load() {
		t.Fatal("the bystander PubSub lost its conn: the standalone loop was released under it")
	}
}

// The give-up and SentinelClient.Close race: both sentinels hang up every 2 ms
// and the grace is 20 ms, so the client gives up one and moves to the other at
// about the rate the test closes it. A bystander on the shared loop must keep
// its conn; under -race the double close is reported.
func TestSentinelCloseVsGiveUp887(t *testing.T) {
	oldGrace := sentinelDownGrace
	sentinelDownGrace = 20 * time.Millisecond
	t.Cleanup(func() { sentinelDownGrace = oldGrace })
	master := startFakeMaster(t)
	a := startFakeSentinel(t, master.Addr())
	b := startFakeSentinel(t, master.Addr())
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
				a.dropAll()
				b.dropAll()
			}
		}
	}()
	by := startHangUpServer(t, func(int) time.Duration { return 0 })
	ps := subscribeToHangUpServer(t, by)
	conn0 := ps.connNow()
	for i := 0; i < 40; i++ {
		sc, err := NewSentinelClient(SentinelConfig{MasterName: "mymaster", SentinelAddrs: []string{a.Addr(), b.Addr()}, DialTimeout: time.Second})
		if err != nil {
			continue
		}
		time.Sleep(time.Duration(rand.IntN(150)) * time.Millisecond)
		_ = sc.Close()
	}
	time.Sleep(300 * time.Millisecond)
	if c := ps.connNow(); c != conn0 || c.closed.Load() {
		t.Errorf("the bystander PubSub lost its conn: the standalone loop was released under it")
	}
}

// Close at a random moment after NewSentinelClient: after Close has returned
// no sentinel conn may stay open. Before the fix subscribeLoop appended the
// sentinel it had just connected without looking at the closed flag, so it
// stayed in sentinelConns, open, after Close had cleared the list.
func TestSentinelCloseDuringSubscribeLeaksNothing887(t *testing.T) {
	master := startFakeMaster(t)
	a := startFakeSentinel(t, master.Addr())
	leaks, iters := 0, 300
	for i := 0; i < iters; i++ {
		sc, err := NewSentinelClient(SentinelConfig{
			MasterName:    "mymaster",
			SentinelAddrs: []string{a.Addr()},
			DialTimeout:   2 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Duration(rand.IntN(400)) * time.Microsecond)
		_ = sc.Close()
		time.Sleep(5 * time.Millisecond)
		sc.sentinelMu.Lock()
		var left *sentinelConn
		if len(sc.sentinelConns) > 0 {
			left = sc.sentinelConns[0]
		}
		sc.sentinelMu.Unlock()
		if left != nil && !left.subPS.closed.Load() {
			leaks++
			_ = left.subPS.Close()
			_ = left.client.Close()
		}
	}
	t.Logf("%d of %d iterations left an open sentinel PubSub and client after Close", leaks, iters)
	if leaks > 0 {
		t.Errorf("%d of %d Close calls left a sentinel connection open", leaks, iters)
	}
}
