//go:build linux

package postgres

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/busypolltest"
	"github.com/goceleris/celeris/internal/sockopts/sockoptstest"
)

// celeris#1228: see the redis driver's test of the same name. The Connector is
// the entry point that registers connections on the engine's loop: a Pool on a
// native engine dials through Go's netpoll and sets no socket option.
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
			addr := startFakePG(t, func(c net.Conn) {
				fakePGTrustStartup(t, c, 42, 0x1ABCD123, func(c net.Conn) { _, _ = io.Copy(io.Discard, c) })
			})
			host, port, _ := net.SplitHostPort(addr)
			cn, err := NewConnector("postgres://u@" + host + ":" + port + "/d?sslmode=disable")
			if err != nil {
				t.Fatalf("connector: %v", err)
			}
			cn = cn.WithEngine(srv)
			defer func() { _ = cn.Close() }()
			dc, err := cn.Connect(context.Background())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			c := dc.(*pgConn)
			defer func() { _ = c.Close() }()
			if got := busypolltest.BusyPoll(t, c.fd); got != tc.want {
				t.Errorf("server SocketBusyPoll %v: the driver socket's SO_BUSY_POLL = %d, want %d (baseline %d)", tc.d, got, tc.want, base)
			}
		})
	}
}

// connectorSocket opens one connection through cn against a fake server and
// returns its socket.
func connectorSocket(t *testing.T, cn *Connector) *pgConn {
	t.Helper()
	dc, err := cn.Connect(context.Background())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := dc.(*pgConn)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func newFakePGConnector(t *testing.T) *Connector {
	t.Helper()
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 42, 0x1ABCD123, func(c net.Conn) { _, _ = io.Copy(io.Discard, c) })
	})
	host, port, _ := net.SplitHostPort(addr)
	cn, err := NewConnector("postgres://u@" + host + ":" + port + "/d?sslmode=disable")
	if err != nil {
		t.Fatalf("connector: %v", err)
	}
	t.Cleanup(func() { _ = cn.Close() })
	return cn
}

// A driver on a server with busy poll off makes no SO_BUSY_POLL call.
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
			cn := newFakePGConnector(t).WithEngine(srv)
			t.Cleanup(func() { _ = cn.Close() })
			calls := recordSetsockopt(t)
			connectorSocket(t, cn)
			polls, _ := busyPollCalls(*calls)
			if len(polls) != len(tc.want) || (len(polls) == 1 && polls[0] != tc.want[0]) {
				t.Errorf("server SocketBusyPoll %v: SO_BUSY_POLL calls = %v, want %v", tc.d, polls, tc.want)
			}
		})
	}
}

// The Connector's own option sets the budget on a standalone loop and wins over
// the server's, whichever of it and WithEngine comes first; with neither a
// server's value nor an option, the standalone Connector stays off.
func TestConnectorSocketBusyPollOption(t *testing.T) {
	base := sockoptstest.Baseline(t)
	srvOn := busypolltest.StartServer(t, 37*time.Microsecond)
	srvOff := busypolltest.StartServer(t, 0)
	for _, tc := range []struct {
		name  string
		build func(cn *Connector) *Connector
		want  int
	}{
		{"standalone, default", func(cn *Connector) *Connector { return cn }, base},
		{"standalone, option 29us", func(cn *Connector) *Connector { return cn.WithSocketBusyPoll(29 * time.Microsecond) }, 29},
		{"server 37us, option first", func(cn *Connector) *Connector {
			return cn.WithSocketBusyPoll(29 * time.Microsecond).WithEngine(srvOn)
		}, 29},
		{"server 37us, option last", func(cn *Connector) *Connector {
			return cn.WithEngine(srvOn).WithSocketBusyPoll(29 * time.Microsecond)
		}, 29},
		{"server off, option 29us", func(cn *Connector) *Connector {
			return cn.WithEngine(srvOff).WithSocketBusyPoll(29 * time.Microsecond)
		}, 29},
		{"server 37us, option unset inherits", func(cn *Connector) *Connector { return cn.WithEngine(srvOn) }, 37},
		{"server 37us, option set to 0 inherits", func(cn *Connector) *Connector {
			return cn.WithEngine(srvOn).WithSocketBusyPoll(0)
		}, 37},
		{"server 37us, option under a microsecond inherits", func(cn *Connector) *Connector {
			return cn.WithEngine(srvOn).WithSocketBusyPoll(500 * time.Nanosecond)
		}, 37},
		{"server 37us, option first, then 0 keeps the engine", func(cn *Connector) *Connector {
			return cn.WithEngine(srvOn).WithSocketBusyPoll(29 * time.Microsecond).WithSocketBusyPoll(0)
		}, 37},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cn := tc.build(newFakePGConnector(t))
			t.Cleanup(func() { _ = cn.Close() })
			c := connectorSocket(t, cn)
			if got := busypolltest.BusyPoll(t, c.fd); got != tc.want {
				t.Errorf("SO_BUSY_POLL = %d, want %d (baseline %d)", got, tc.want, base)
			}
		})
	}
}

// Through the public Open: a Pool dials on an event loop only when it is opened
// with the Engine of a server that has not started yet (it then uses its own
// loop), and takes that server's budget; with no engine, or a started server's,
// it dials through Go's netpoll and sets no option. A Pool has no option of its
// own.
func TestPoolSocketBusyPoll(t *testing.T) {
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
			addr := startFakePG(t, func(c net.Conn) {
				fakePGTrustStartup(t, c, 42, 0x1ABCD123, func(c net.Conn) { _, _ = io.Copy(io.Discard, c) })
			})
			host, port, _ := net.SplitHostPort(addr)
			p, err := Open("postgres://u@"+host+":"+port+"/d?sslmode=disable", tc.opts()...)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = p.Close() }()
			c, err := p.acquire(context.Background())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer p.release(c)
			fd := c.fd
			if fd == 0 {
				fd = int(c.tcpFd)
			}
			if got := busypolltest.BusyPoll(t, fd); got != tc.want {
				t.Errorf("SO_BUSY_POLL = %d, want %d (baseline %d)", got, tc.want, base)
			}
		})
	}
}
