//go:build linux

package celeris_test

import (
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/internal/sockopts/sockoptstest"
)

// TestSocketBusyPollIsOptInThroughTheServer is celeris#1036
// through the public API, on every engine: a server built with
// celeris.Config{} puts no SO_BUSY_POLL on the sockets it accepts, and one
// built with Config.SocketBusyPoll puts exactly that budget on them. The
// engine-level tests build a resource.Config themselves, so they cannot see a
// Config field that toResourceConfig drops; this one starts celeris.Server.
// The std engine serves through net/http and ignores the field (documented on
// Config.SocketBusyPoll), so it reads the baseline either way.
func TestSocketBusyPollIsOptInThroughTheServer(t *testing.T) {
	base := sockoptstest.Baseline(t)
	for _, e := range drainEngines806 {
		for _, tc := range []struct {
			name string
			d    time.Duration
			want int
		}{
			{"default", 0, base},
			{"37us", 37 * time.Microsecond, 37},
		} {
			t.Run(e.name+"/"+tc.name, func(t *testing.T) {
				srv := startDrainServer806(t, e, 5*time.Second, 0,
					func(c *celeris.Config) { c.SocketBusyPoll = tc.d },
					func(*celeris.Server) {})
				want := tc.want
				if e.eng == celeris.Std {
					want = base
				}
				for range 3 {
					c, err := net.DialTimeout("tcp", srv.addr, 3*time.Second)
					if err != nil {
						t.Fatalf("dial: %v", err)
					}
					t.Cleanup(func() { _ = c.Close() })
					sockoptstest.RoundTrip(t, c)
					if got := sockoptstest.AcceptedBusyPoll(t, c); got != want {
						t.Errorf("%s with SocketBusyPoll %v: the accepted socket's SO_BUSY_POLL = %d, want %d (baseline %d)",
							e.name, tc.d, got, want, base)
					}
				}
			})
		}
	}
}
