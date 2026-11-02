//go:build linux

package memcached

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/busypolltest"
	"github.com/goceleris/celeris/internal/sockopts/sockoptstest"
)

// celeris#1228: see the redis driver's test of the same name.
func TestDriverSocketBusyPollFollowsTheServer(t *testing.T) {
	base := sockoptstest.Baseline(t)
	for _, tc := range []struct {
		name string
		d    time.Duration
		want int
	}{
		{"server off", 0, base},
		{"server 37us", 37 * time.Microsecond, 37},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := busypolltest.StartServer(t, tc.d)
			fake := startFake(t)
			c, err := dialMemcachedConn(context.Background(), srv.EventLoopProvider(), Config{Addr: fake.Addr(), Engine: srv}, 0)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() { _ = c.Close() }()
			if got := busypolltest.BusyPoll(t, c.fd); got != tc.want {
				t.Errorf("server SocketBusyPoll %v: the driver socket's SO_BUSY_POLL = %d, want %d (baseline %d)", tc.d, got, tc.want, base)
			}
		})
	}
}
