package redis

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
)

// celeris#1246: SentinelClient.Close returned while the background work (the
// sentinel subscription, a failover in progress) could still hold a Client it had
// just built. Opened WithEngine of a server with no event loop of its own, that
// Client holds a reference to the driver's standalone loop, so the reference
// outlived Close until the work noticed the close (TestSentinelClientWithEngine
// ReleasesTheStandaloneLoop1238 saw one left on windows-latest). The tests below
// hold such a Client open from a hook while Close is called, which makes the
// race certain instead of a matter of scheduling.

// holdLateClient arms testHookClientBuilt for the SentinelClients opened with
// srv: the first Client of kind the background work builds signals entered and
// then stays in the hook until release is closed or Close cancels its ctx. The
// returned stop releases it, and disarms the hook, and is safe to call twice.
func holdLateClient(t *testing.T, srv ServerProvider, kind string) (entered <-chan struct{}, stop func()) {
	t.Helper()
	enter := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	hook := func(s *SentinelClient, k string, ctx context.Context) {
		if s.cfg.Engine != srv || k != kind {
			return
		}
		held := false
		once.Do(func() { close(enter); held = true })
		if !held {
			return
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	testHookClientBuilt.Store(&hook)
	var stopOnce sync.Once
	stop = func() {
		stopOnce.Do(func() {
			testHookClientBuilt.Store(nil)
			close(release)
		})
	}
	t.Cleanup(stop)
	return enter, stop
}

// awaitClosed1246 fails the test unless ch is closed within the deadline: the
// deadline only bounds a hang, nothing is ordered by it.
func awaitClosed1246(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// pollUntil1246 polls cond until it holds or the deadline passes. The deadline
// only bounds a hang: cond decides, and polling is for a state some goroutine
// reaches on its own (the subscription connecting), not for an ordering.
func pollUntil1246(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// awaitSubscribed1246 waits until the fake sentinel has recorded the
// +switch-master subscription of the SentinelClient: a notification pushed
// before that reaches nobody. The SUBSCRIBE is written without waiting for the
// server, so the SentinelClient having connected does not say it.
func awaitSubscribed1246(t *testing.T, fs *fakeSentinel) {
	t.Helper()
	pollUntil1246(t, "the sentinel to record the +switch-master subscription", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return len(fs.subWriters) > 0
	})
}

// The subscription is still connecting its first sentinel (it has built the
// sentinel's Client) when Close is called: Close must not return with that
// Client open.
func TestSentinelCloseWithASentinelClientInFlight1246(t *testing.T) {
	for name, srv := range loopServers(t) {
		t.Run(name, func(t *testing.T) {
			master := startFakeMaster(t)
			sentinel := startFakeSentinel(t, master.Addr())
			before := eventloop.StandaloneRefs()
			entered, stop := holdLateClient(t, srv, "sentinel")
			sc, err := NewSentinelClient(SentinelConfig{
				MasterName:    "mymaster",
				SentinelAddrs: []string{sentinel.Addr()},
				DialTimeout:   2 * time.Second,
				Engine:        srv,
			})
			if err != nil {
				t.Fatal(err)
			}
			awaitClosed1246(t, entered, "the subscription to build its sentinel Client")
			if got := eventloop.StandaloneRefs(); got != before+2 {
				t.Errorf("open: standalone loop references = %d, want %d (master and sentinel Clients)", got, before+2)
			}
			_ = sc.Close()
			wantRefs(t, before, "closed with a sentinel Client in flight")
			stop()
			_ = sc.Close()
			wantRefs(t, before, "closed twice")
		})
	}
}

// Every Close that returns, the one that closes and one that comes in while it
// runs, returns with nothing left open.
func TestSentinelConcurrentCloseWithAClientInFlight1246(t *testing.T) {
	for name, srv := range loopServers(t) {
		t.Run(name, func(t *testing.T) {
			master := startFakeMaster(t)
			sentinel := startFakeSentinel(t, master.Addr())
			before := eventloop.StandaloneRefs()
			entered, _ := holdLateClient(t, srv, "sentinel")
			sc, err := NewSentinelClient(SentinelConfig{
				MasterName:    "mymaster",
				SentinelAddrs: []string{sentinel.Addr()},
				DialTimeout:   2 * time.Second,
				Engine:        srv,
			})
			if err != nil {
				t.Fatal(err)
			}
			awaitClosed1246(t, entered, "the subscription to build its sentinel Client")
			// refs[i] is what Close number i saw on return; wg and the slice
			// are the only things the goroutines share.
			var refs [4]int
			var wg sync.WaitGroup
			for i := range refs {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_ = sc.Close()
					refs[i] = eventloop.StandaloneRefs()
				}()
			}
			wg.Wait()
			for i, got := range refs {
				if got != before {
					t.Errorf("Close %d returned with standalone loop references = %d, want %d", i, got, before)
				}
			}
		})
	}
}

// A failover is dialling the new master (its Client is built) when Close is
// called: Close must not return with that Client open.
func TestSentinelCloseWithAMasterClientInFlight1246(t *testing.T) {
	for name, srv := range loopServers(t) {
		t.Run(name, func(t *testing.T) {
			master1 := startFakeMaster(t)
			master2 := startFakeMaster(t)
			sentinel := startFakeSentinel(t, master1.Addr())
			before := eventloop.StandaloneRefs()
			sc, err := NewSentinelClient(SentinelConfig{
				MasterName:    "mymaster",
				SentinelAddrs: []string{sentinel.Addr()},
				DialTimeout:   2 * time.Second,
				Engine:        srv,
			})
			if err != nil {
				t.Fatal(err)
			}
			// The subscription has connected, so +switch-master reaches the
			// failover handler.
			awaitSubscribed1246(t, sentinel)
			entered, stop := holdLateClient(t, srv, "master")
			sentinel.SetMaster(master2.Addr())
			awaitClosed1246(t, entered, "the failover to build the new master's Client")
			_ = sc.Close()
			wantRefs(t, before, "closed with a master Client in flight")
			stop()
		})
	}
}

// The failover finishes, then Close: the swapped-out master Client is closed
// once, by the failover, and the new one once, by Close. A second release would
// take the reference of another user of the loop (other, below).
func TestSentinelCloseAfterAFailoverReleasesEachClientOnce1246(t *testing.T) {
	for name, srv := range loopServers(t) {
		t.Run(name, func(t *testing.T) {
			master1 := startFakeMaster(t)
			master2 := startFakeMaster(t)
			sentinel := startFakeSentinel(t, master1.Addr())
			before := eventloop.StandaloneRefs()
			other, err := eventloop.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer eventloop.Release(other)
			sc, err := NewSentinelClient(SentinelConfig{
				MasterName:    "mymaster",
				SentinelAddrs: []string{sentinel.Addr()},
				DialTimeout:   2 * time.Second,
				Engine:        srv,
			})
			if err != nil {
				t.Fatal(err)
			}
			first, err := sc.getClient()
			if err != nil {
				t.Fatal(err)
			}
			awaitSubscribed1246(t, sentinel)
			sentinel.SetMaster(master2.Addr())
			pollUntil1246(t, "the failover to swap the master Client", func() bool {
				c, err := sc.getClient()
				return err == nil && c != first
			})
			_ = sc.Close()
			wantRefs(t, before+1, "closed after a failover")
			_ = sc.Close()
			wantRefs(t, before+1, "closed twice")
		})
	}
}
