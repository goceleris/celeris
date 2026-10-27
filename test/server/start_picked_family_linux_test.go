//go:build linux

package celeris_test

import (
	"fmt"
	"net"
	"sync"
	"testing"

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
				var wg sync.WaitGroup
				addrs := make([]string, 2)
				for i := range addrs {
					wg.Add(1)
					go func() { defer wg.Done(); addrs[i] = st.start(t) }()
				}
				wg.Wait()
				if addrs[0] == addrs[1] {
					t.Errorf("both starters ended on %s: one of the two servers failed to bind it and the other's /ping answered for it", addrs[0])
				}
			})
		}
	}
}
