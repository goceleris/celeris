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

// Through the public NewClient, the one entry point a user has: the only
// memcached connection on an event loop is that of a Client opened with the
// Engine of a server that has not started yet, and it takes that server's
// budget. A Client with no engine, and one with a started server's, dial through
// Go's netpoll and are never tuned; there is no option to change that.
func TestNewClientSocketBusyPoll(t *testing.T) {
	base := sockoptstest.Baseline(t)
	started := busypolltest.StartServer(t, 37*time.Microsecond)
	for _, tc := range []struct {
		name string
		opts func() []Option
		want int
	}{
		{"standalone", func() []Option { return nil }, base},
		{"server not started, 37us", func() []Option { return []Option{WithEngine(busypolltest.NewServer(37 * time.Microsecond))} }, 37},
		{"server not started, off", func() []Option { return []Option{WithEngine(busypolltest.NewServer(0))} }, base},
		{"server started, 37us (netpoll)", func() []Option { return []Option{WithEngine(started)} }, base},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := startFake(t)
			cl, err := NewClient(fake.Addr(), tc.opts()...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cl.Close() }()
			c, err := cl.pool.acquire(context.Background(), 0)
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			defer cl.pool.release(c)
			fd := c.fd
			if c.useDirect {
				fd = c.tcpFd
			}
			if got := busypolltest.BusyPoll(t, fd); got != tc.want {
				t.Errorf("SO_BUSY_POLL = %d, want %d (baseline %d, direct conn: %v)", got, tc.want, base, c.useDirect)
			}
		})
	}
}
