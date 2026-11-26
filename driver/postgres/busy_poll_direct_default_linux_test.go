//go:build linux

package postgres

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/busypolltest"
	"github.com/goceleris/celeris/internal/sockopts/sockoptstest"
)

// A Connector dials direct connections by default, through Go's netpoll, which
// sets no SO_BUSY_POLL: neither Connector.WithSocketBusyPoll nor the server's
// Config.SocketBusyPoll (WithEngine) reaches them. The three tests below are the
// ones of busy_poll_1228_linux_test.go for the default Connector: they assert that
// the budget is not applied, as documented, rather than stay silent. Those tests
// themselves run on a Connector WithStreaming, the loop, and are otherwise
// unchanged.

// directSocket opens a connection through cn against a fake server and returns its
// socket; cn must be a Connector without WithStreaming.
func directSocket(t *testing.T, cn *Connector) (fd int) {
	t.Helper()
	c := connectPG(t, cn)
	if !c.useDirect {
		t.Fatal("not a direct connection")
	}
	if c.fd != 0 {
		t.Fatalf("a direct connection has a loop descriptor: fd = %d", c.fd)
	}
	if c.tcpFd == 0 {
		t.Fatal("a direct connection has no socket descriptor")
	}
	return c.tcpFd
}

// The server's SocketBusyPoll does not reach a default Connector's connection: its
// socket keeps the kernel's value, whether the server has it on or off.
func TestDefaultConnectorSocketBusyPollIgnoresTheServer(t *testing.T) {
	base := sockoptstest.Baseline(t)
	for _, tc := range []struct {
		name string
		d    time.Duration
	}{
		{"server off", 0},
		{"server 37us", 37 * time.Microsecond},
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
			if got := busypolltest.BusyPoll(t, directSocket(t, cn)); got != base {
				t.Errorf("server SocketBusyPoll %v: a direct connection's SO_BUSY_POLL = %d, want the baseline %d", tc.d, got, base)
			}
		})
	}
}

// A default Connector's connection makes no SO_BUSY_POLL call at all, with the
// server's budget on or off.
func TestDefaultConnectorMakesNoBusyPollCalls(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    time.Duration
	}{
		{"server off", 0},
		{"server 37us", 37 * time.Microsecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := busypolltest.StartServer(t, tc.d)
			cn := newFakePGConnector(t).WithEngine(srv)
			t.Cleanup(func() { _ = cn.Close() })
			calls := recordSetsockopt(t)
			directSocket(t, cn)
			if polls, _ := busyPollCalls(*calls); len(polls) != 0 {
				t.Errorf("server SocketBusyPoll %v: SO_BUSY_POLL calls = %v, want none", tc.d, polls)
			}
		})
	}
}

// The Connector's own WithSocketBusyPoll does not reach a direct connection
// either, whatever the order of the chain.
func TestDefaultConnectorSocketBusyPollOptionIsNotApplied(t *testing.T) {
	base := sockoptstest.Baseline(t)
	srvOn := busypolltest.StartServer(t, 37*time.Microsecond)
	for _, tc := range []struct {
		name  string
		build func(cn *Connector) *Connector
	}{
		{"option 29us", func(cn *Connector) *Connector { return cn.WithSocketBusyPoll(29 * time.Microsecond) }},
		{"server 37us, option 29us", func(cn *Connector) *Connector {
			return cn.WithEngine(srvOn).WithSocketBusyPoll(29 * time.Microsecond)
		}},
		{"option 29us, server 37us", func(cn *Connector) *Connector {
			return cn.WithSocketBusyPoll(29 * time.Microsecond).WithEngine(srvOn)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cn := tc.build(newFakePGConnector(t))
			t.Cleanup(func() { _ = cn.Close() })
			calls := recordSetsockopt(t)
			fd := directSocket(t, cn)
			if polls, _ := busyPollCalls(*calls); len(polls) != 0 {
				t.Errorf("SO_BUSY_POLL calls = %v, want none", polls)
			}
			if got := busypolltest.BusyPoll(t, fd); got != base {
				t.Errorf("SO_BUSY_POLL = %d, want the baseline %d", got, base)
			}
		})
	}
}
