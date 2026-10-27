//go:build linux

package celeris_test

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#1104: every starter of test/server that picked a port, closed it and
// had Start bind it could be given a port another test of the process picked at
// the same time (package F, startPicked1104, moved three starters off it). This
// forces two concurrent calls of each remaining starter onto the same picked
// port (listenForStart1104: the first picker gets it, a picker that finds it
// held gets a fresh one) and requires them to end on different ports. A starter
// that frees the port between the pick and the bind gives both of them the same
// one, and one of the two servers is not the one that answers there.
//
// It runs on the std engine, the only one that serves on the listener it is
// handed. The epoll, io_uring and adaptive engines close it inside Listen and
// rebind the address with SO_REUSEPORT, so a hand-off only shortens their window
// and two servers can still end on one port: the same test with the engine
// swapped ended two servers on one port with epoll in 2 of 5 runs of up to 630
// subtests (2 failures in the run counted), with io_uring in 2 of 3 runs of 630
// (1 failure each), and the review of celeris#1139 saw 3 of 105 and 2 of 210. No
// retry sees it, as both binds succeed. The starters are tested here, on std,
// where the guarantee holds.
//
// The two starts of a pair run as parallel subtests, not as bare goroutines, so a
// start that fails (the starters call t.Fatal) ends its own subtest. A failed
// start leaves its address empty and is reported by its subtest alone.
func TestStartersForcedOntoOnePortEndOnPortsOfTheirOwn1104(t *testing.T) {
	std := celeris.Config{Engine: celeris.Std}
	none := func(*celeris.Server) {}
	starters := []struct {
		name  string
		start func(t *testing.T) string
	}{
		{"startServerConfig761", func(t *testing.T) string { return startServerConfig761(t, std, none) }},
		{"startServerConfig421", func(t *testing.T) string { return startServerConfig421(t, std, none) }},
		{"bodyServer889", func(t *testing.T) string { return bodyServer889(t, celeris.Std, 0) }},
		{"start889", func(t *testing.T) string { a, _ := start889(t, std); return a }},
		{"startServer870", func(t *testing.T) string { return startServer870(t, std) }},
		{"startGlobalChainServer836", func(t *testing.T) string { return startGlobalChainServer836(t, std, none) }},
		{"startServer964", func(t *testing.T) string { return startServer964(t, std) }},
		{"startServer760", func(t *testing.T) string { return startServer760(t, celeris.Std, 5*time.Second, 0, nil, none).addr }},
		{"startH2PoolServer759", func(t *testing.T) string { return startH2PoolServer759(t, celeris.Std, 5*time.Second, none).addr }},
	}
	for _, st := range starters {
		for round := 0; round < 3; round++ {
			t.Run(fmt.Sprintf("%s/round%d", st.name, round), func(t *testing.T) {
				pick, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				port := pick.Addr().String()
				_ = pick.Close()
				defer func(old func() (net.Listener, error)) { listenForStart1104 = old }(listenForStart1104)
				listenForStart1104 = func() (net.Listener, error) {
					if ln, err := net.Listen("tcp", port); err == nil {
						return ln, nil
					}
					return net.Listen("tcp", "127.0.0.1:0")
				}
				addrs := make([]string, 2)
				// A starter's server stops when its subtest ends, so each waits for
				// the other to be up first (the barrier), or the second could be given
				// the port of a first that is gone.
				var up sync.WaitGroup
				up.Add(len(addrs))
				bothUp := make(chan struct{})
				go func() { up.Wait(); close(bothUp) }()
				var overlapped atomic.Int32
				// The inner Run returns once both parallel subtests are done.
				t.Run("pair", func(t *testing.T) {
					for i := range addrs {
						t.Run(fmt.Sprintf("starter%d", i), func(t *testing.T) {
							t.Parallel()
							defer func() {
								up.Done()
								select {
								case <-bothUp:
									overlapped.Add(1)
								case <-time.After(30 * time.Second):
								}
							}()
							addrs[i] = st.start(t)
						})
					}
				})
				if overlapped.Load() != int32(len(addrs)) {
					t.Errorf("the two starters were not up at the same time (is -parallel 1?)")
				}
				if addrs[0] != "" && addrs[0] == addrs[1] {
					t.Errorf("both starters ended on %s: one of the two servers failed to bind it and the other's /ping answered for it", addrs[0])
				}
			})
		}
	}
}
