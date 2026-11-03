//go:build linux

package redis

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/driver/internal/busypolltest"
	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/engine"
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
			fake := startFakeRedis(t, helloOnly)
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
			fake := startFakeRedis(t, helloOnly)
			calls := recordSetsockopt(t)
			c, err := dialRedisConn(context.Background(), srv.EventLoopProvider(), Config{Addr: fake.Addr(), Engine: srv}, 0)
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

func helloOnly(cmd []string, w *bufio.Writer) {
	if len(cmd) > 0 && strings.EqualFold(cmd[0], "HELLO") {
		handleHELLO(w, 3)
	}
}

// releaseLeakedLoop gives back the reference to the driver's standalone loop
// that a Client opened WithEngine of a server that has not started takes and
// never releases (its Engine is set, so it does not think it owns the loop).
// Left held, the loop is shared with every later test of the package, and a
// SentinelClient's Pub/Sub then keeps its connection after Close.
func releaseLeakedLoop(prov engine.EventLoopProvider, name string) {
	if strings.Contains(name, "server not started") {
		eventloop.Release(prov)
	}
}

func connFD(c *redisConn) int {
	if c.useDirect {
		return c.tcpFd
	}
	return c.fd
}

// Through the public NewClient: SO_BUSY_POLL read off the fd of a pooled command
// connection and of a Pub/Sub connection. A Client on an event loop (no engine,
// or the engine of a server that has not started) is tuned, with the option or
// the server's budget; the command connections of a started server's Client
// dial through Go's netpoll and are not, while its Pub/Sub connections stay on
// the server's loop and are. The sub-microsecond option is "not set".
func TestNewClientSocketBusyPoll(t *testing.T) {
	base := sockoptstest.Baseline(t)
	started := busypolltest.StartServer(t, 37*time.Microsecond)
	us := time.Microsecond
	for _, tc := range []struct {
		name        string
		opts        func() []Option
		cmd, pubsub int
	}{
		{"standalone", func() []Option { return nil }, base, base},
		{"standalone, option 29us", func() []Option { return []Option{WithSocketBusyPoll(29 * us)} }, 29, 29},
		{"standalone, option 500ns", func() []Option { return []Option{WithSocketBusyPoll(500 * time.Nanosecond)} }, base, base},
		{"server not started, 37us", func() []Option { return []Option{WithEngine(busypolltest.NewServer(37 * us))} }, 37, 37},
		{"server not started, off", func() []Option { return []Option{WithEngine(busypolltest.NewServer(0))} }, base, base},
		{"server not started, 37us, option 29us", func() []Option {
			return []Option{WithEngine(busypolltest.NewServer(37 * us)), WithSocketBusyPoll(29 * us)}
		}, 29, 29},
		{"server not started, 37us, option 500ns", func() []Option {
			return []Option{WithEngine(busypolltest.NewServer(37 * us)), WithSocketBusyPoll(500 * time.Nanosecond)}
		}, 37, 37},
		{"server started, 37us", func() []Option { return []Option{WithEngine(started)} }, base, 37},
		{"server started, 37us, option 29us", func() []Option { return []Option{WithEngine(started), WithSocketBusyPoll(29 * us)} }, base, 29},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := startFakeRedis(t, helloOnly)
			cl, err := NewClient(fake.Addr(), tc.opts()...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = cl.Close()
				releaseLeakedLoop(cl.pool.provider, tc.name)
			}()
			ctx := context.Background()
			c, err := cl.pool.acquireCmd(ctx, 0)
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			defer cl.pool.releaseCmd(c)
			if got := busypolltest.BusyPoll(t, connFD(c)); got != tc.cmd {
				t.Errorf("command conn (direct: %v): SO_BUSY_POLL = %d, want %d (baseline %d)", c.useDirect, got, tc.cmd, base)
			}
			p, err := cl.pool.acquirePubSub(ctx)
			if err != nil {
				t.Fatalf("acquire pubsub: %v", err)
			}
			defer func() { _ = p.Close() }()
			if got := busypolltest.BusyPoll(t, connFD(p)); got != tc.pubsub {
				t.Errorf("pubsub conn: SO_BUSY_POLL = %d, want %d (baseline %d)", got, tc.pubsub, base)
			}
		})
	}
}

// ClusterConfig and SentinelConfig forward SocketBusyPoll to the clients they
// open (ClusterClient.nodeOpts is the one place the cluster builds them).
func TestClusterConfigForwardsSocketBusyPoll(t *testing.T) {
	c := &ClusterClient{cfg: ClusterConfig{SocketBusyPoll: 29 * time.Microsecond}}
	var cfg Config
	for _, o := range c.nodeOpts() {
		o(&cfg)
	}
	if cfg.SocketBusyPoll != 29*time.Microsecond {
		t.Errorf("node Config.SocketBusyPoll = %v, want 29us", cfg.SocketBusyPoll)
	}
}

// A SentinelClient opens three kinds of client (the sentinel it asks for the
// master, the master, and the sentinel it subscribes to); the setsockopt seam,
// with the peer of each fd, shows SocketBusyPoll reaching every one of them.
func TestSentinelConfigForwardsSocketBusyPoll(t *testing.T) {
	master := startFakeMaster(t)
	sentinel := startFakeSentinel(t, master.Addr())
	var mu sync.Mutex
	perPeer := map[int]int{} // peer port -> SO_BUSY_POLL=29 calls
	old := setsockoptInt
	setsockoptInt = func(fd, level, opt, val int) error {
		if level == unix.SOL_SOCKET && opt == unix.SO_BUSY_POLL && val == 29 {
			if sa, err := unix.Getpeername(fd); err == nil {
				if in4, ok := sa.(*unix.SockaddrInet4); ok {
					mu.Lock()
					perPeer[in4.Port]++
					mu.Unlock()
				}
			}
		}
		return old(fd, level, opt, val)
	}
	t.Cleanup(func() { setsockoptInt = old })

	sc, err := NewSentinelClient(SentinelConfig{
		MasterName:     "mymaster",
		SentinelAddrs:  []string{sentinel.Addr()},
		DialTimeout:    2 * time.Second,
		SocketBusyPoll: 29 * time.Microsecond,
	})
	if err != nil {
		t.Fatalf("NewSentinelClient: %v", err)
	}
	defer func() { _ = sc.Close() }()
	if err := sc.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	port := func(addr string) int {
		_, p, _ := net.SplitHostPort(addr)
		n, _ := strconv.Atoi(p)
		return n
	}
	// The subscription to the sentinel is made by a goroutine: wait for it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		sent, mast := perPeer[port(sentinel.Addr())], perPeer[port(master.Addr())]
		mu.Unlock()
		if sent >= 2 && mast >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("SO_BUSY_POLL=29 calls on sentinel conns: %d (want >= 2: the master query and the subscription), on master conns: %d (want >= 1)", sent, mast)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
