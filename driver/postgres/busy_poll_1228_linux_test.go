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
