//go:build linux

package memcached

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/busypolltest"
	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/engine"
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

// A driver on a server with busy poll off makes no SO_BUSY_POLL call; one on a
// server with it on makes exactly one, with the server's budget.
func TestDriverOnTheServerMakesTheServersBusyPollCalls(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    time.Duration
		want []int
	}{
		{"server off", 0, nil},
		{"server 37us", 37 * time.Microsecond, []int{37}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := busypolltest.StartServer(t, tc.d)
			fake := startFake(t)
			calls := recordSetsockopt(t)
			c, err := dialMemcachedConn(context.Background(), srv.EventLoopProvider(), Config{Addr: fake.Addr(), Engine: srv}, 0)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() { _ = c.Close() }()
			polls, _ := busyPollCalls(*calls)
			if len(polls) != len(tc.want) || (len(polls) == 1 && polls[0] != tc.want[0]) {
				t.Errorf("server SocketBusyPoll %v: SO_BUSY_POLL calls = %v, want %v", tc.d, polls, tc.want)
			}
		})
	}
}

// The driver's own option sets the budget on a standalone loop, where nothing
// else would, and wins over the server's; with neither a server's value nor
// an option, the standalone driver stays off.
func TestDriverSocketBusyPollOption(t *testing.T) {
	base := sockoptstest.Baseline(t)
	std, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eventloop.Release(std) })
	srvOn := busypolltest.StartServer(t, 37*time.Microsecond)
	srvOff := busypolltest.StartServer(t, 0)
	for _, tc := range []struct {
		name string
		prov engine.EventLoopProvider
		cfg  Config
		want int
	}{
		{"standalone, default", std, Config{}, base},
		{"standalone, option 29us", std, Config{SocketBusyPoll: 29 * time.Microsecond}, 29},
		{"server 37us, option 29us wins", srvOn.EventLoopProvider(), Config{Engine: srvOn, SocketBusyPoll: 29 * time.Microsecond}, 29},
		{"server off, option 29us", srvOff.EventLoopProvider(), Config{Engine: srvOff, SocketBusyPoll: 29 * time.Microsecond}, 29},
		{"server 37us, option unset inherits", srvOn.EventLoopProvider(), Config{Engine: srvOn}, 37},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := startFake(t)
			tc.cfg.Addr = fake.Addr()
			c, err := dialMemcachedConn(context.Background(), tc.prov, tc.cfg, 0)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() { _ = c.Close() }()
			if got := busypolltest.BusyPoll(t, c.fd); got != tc.want {
				t.Errorf("SO_BUSY_POLL = %d, want %d (baseline %d)", got, tc.want, base)
			}
		})
	}
}
