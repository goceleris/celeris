package redis

import (
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

// celeris#1238: a Client opened WithEngine of a server with no event loop of
// its own takes a reference to the standalone loop and must give it back on
// Close, as one with no engine does.
func TestClientWithEngineReleasesTheStandaloneLoop1238(t *testing.T) {
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
			wantRefs(t, before+2, "open")
			_ = cl.Close()
			wantRefs(t, before+1, "closed")
			_ = cl.Close()
			wantRefs(t, before+1, "closed twice")
		})
	}
}

// A Client with no engine holds one reference until Close, as before.
func TestClientWithoutEngineLoopReference1238(t *testing.T) {
	before := eventloop.StandaloneRefs()
	cl, err := NewClient("127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	wantRefs(t, before+1, "no engine, open")
	_ = cl.Close()
	wantRefs(t, before, "no engine, closed")
}

func TestClusterClientWithEngineReleasesTheStandaloneLoop1238(t *testing.T) {
	for name, srv := range loopServers(t) {
		t.Run(name, func(t *testing.T) {
			node1, node2, node3 := setupCluster(t)
			before := eventloop.StandaloneRefs()
			cc, err := NewClusterClient(ClusterConfig{
				Addrs:       []string{node1.Addr(), node2.Addr(), node3.Addr()},
				DialTimeout: 2 * time.Second,
				Engine:      srv,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := eventloop.StandaloneRefs(); got <= before {
				t.Errorf("open: standalone loop references = %d, want more than %d", got, before)
			}
			_ = cc.Close()
			wantRefs(t, before, "closed")
		})
	}
}

func TestSentinelClientWithEngineReleasesTheStandaloneLoop1238(t *testing.T) {
	for name, srv := range loopServers(t) {
		t.Run(name, func(t *testing.T) {
			master := startFakeMaster(t)
			sentinel := startFakeSentinel(t, master.Addr())
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
			if got := eventloop.StandaloneRefs(); got <= before {
				t.Errorf("open: standalone loop references = %d, want more than %d", got, before)
			}
			_ = sc.Close()
			wantRefs(t, before, "closed")
		})
	}
}
