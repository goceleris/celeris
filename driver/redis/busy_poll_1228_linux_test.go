//go:build linux

package redis

import (
	"bufio"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/busypolltest"
	"github.com/goceleris/celeris/internal/sockopts/sockoptstest"
)

// celeris#1228: the driver put SO_BUSY_POLL=50 on every connection, unasked.
// It is opt-in now, as Config.SocketBusyPoll is for the accepted sockets: a
// driver opened WithEngine takes the server's value, and 0 leaves the socket as
// the kernel made it.
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
			fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
				if len(cmd) > 0 && strings.EqualFold(cmd[0], "HELLO") {
					handleHELLO(w, 3)
				}
			})
			c, err := dialRedisConn(context.Background(), srv.EventLoopProvider(), Config{Addr: fake.Addr(), Engine: srv}, 0)
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
