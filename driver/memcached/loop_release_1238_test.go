package memcached

import (
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/driver/internal/looptest"
)

// loopServers are the servers a driver opened WithEngine of cannot use the
// event loop of, so that the Client runs on the driver's standalone loop.
func loopServers(t *testing.T) map[string]ServerProvider {
	return map[string]ServerProvider{
		"server not started":        looptest.Unstarted(),
		"std without AsyncHandlers": looptest.StartStd(t),
	}
}

// wantRefs fails the test unless the standalone loop's reference count is want.
func wantRefs(t *testing.T, want int, when string) {
	t.Helper()
	if got := eventloop.StandaloneRefs(); got != want {
		t.Errorf("%s: standalone loop references = %d, want %d", when, got, want)
	}
}

// clientHoldsTheLoop reports whether a Client on the standalone loop keeps it:
// it does where the loop's worker has the synchronous round trip (Linux). Where
// it does not, the Client falls back to its direct pool, which has no use for
// the loop, and gives the reference back at once.
func clientHoldsTheLoop(t *testing.T) bool {
	t.Helper()
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)
	wl := prov.WorkerLoop(0)
	if wl == nil {
		return true
	}
	_, ok := wl.(syncRoundTripper)
	return ok
}

// celeris#1238: a Client opened WithEngine of a server with no event loop of
// its own takes a reference to the standalone loop and must give it back on
// Close, once, however many goroutines close it.
func TestClientWithEngineReleasesTheStandaloneLoop1238(t *testing.T) {
	held := 0
	if clientHoldsTheLoop(t) {
		held = 1
	}
	for name, srv := range loopServers(t) {
		t.Run(name, func(t *testing.T) {
			before := eventloop.StandaloneRefs()
			// Another user of the loop, whose reference must survive the Client:
			// with none, the loop is gone after the first Release and a second
			// one could not be seen.
			other, err := eventloop.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer eventloop.Release(other)
			cl, err := NewClient("127.0.0.1:1", WithEngine(srv))
			if err != nil {
				t.Fatal(err)
			}
			wantRefs(t, before+1+held, "open")
			_ = cl.Close()
			wantRefs(t, before+1, "closed")
			_ = cl.Close()
			wantRefs(t, before+1, "closed twice")
		})
	}
}

func TestClientConcurrentCloseReleasesTheStandaloneLoopOnce1238(t *testing.T) {
	if !clientHoldsTheLoop(t) {
		t.Skip("the standalone loop is not used by a memcached Client on this platform")
	}
	for name, srv := range loopServers(t) {
		t.Run(name, func(t *testing.T) {
			before := eventloop.StandaloneRefs()
			other, err := eventloop.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer eventloop.Release(other)
			cl, err := NewClient("127.0.0.1:1", WithEngine(srv))
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_ = cl.Close()
				}()
			}
			wg.Wait()
			wantRefs(t, before+1, "closed by 8 goroutines")
		})
	}
}

func TestClusterClientWithEngineReleasesTheStandaloneLoop1238(t *testing.T) {
	for name, srv := range loopServers(t) {
		t.Run(name, func(t *testing.T) {
			_, addrs := startFakeCluster(t, 3)
			before := eventloop.StandaloneRefs()
			cc, err := NewClusterClient(ClusterConfig{Addrs: addrs, DialTimeout: time.Second, Engine: srv})
			if err != nil {
				t.Fatal(err)
			}
			if clientHoldsTheLoop(t) {
				wantRefs(t, before+len(addrs), "open")
			}
			_ = cc.Close()
			wantRefs(t, before, "closed")
		})
	}
}
