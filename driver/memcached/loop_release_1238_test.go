package memcached

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
// Close.
func TestClientWithEngineReleasesTheStandaloneLoop1238(t *testing.T) {
	for name, srv := range loopServers(t) {
		t.Run(name, func(t *testing.T) {
			before := eventloop.StandaloneRefs()
			cl, err := NewClient("127.0.0.1:1", WithEngine(srv))
			if err != nil {
				t.Fatal(err)
			}
			wantRefs(t, before+1, "open")
			_ = cl.Close()
			wantRefs(t, before, "closed")
			_ = cl.Close()
			wantRefs(t, before, "closed twice")
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
			wantRefs(t, before+len(addrs), "open")
			_ = cc.Close()
			wantRefs(t, before, "closed")
		})
	}
}
