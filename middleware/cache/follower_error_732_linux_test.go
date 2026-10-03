//go:build linux

package cache_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/probe"
	"github.com/goceleris/celeris/middleware/cache"
	"github.com/goceleris/celeris/middleware/internal/sf"
	"github.com/goceleris/celeris/middleware/store"
)

// TestFollowerErrorSurvivesLeaderNextRequest pins the cache site of
// celeris#732: the coalesced fill (Singleflight) hands the leader's handler
// error to every follower, which returns it on its own request.
//
// The error can hold the leader's request strings (here errors.New of a
// header), and on epoll and io_uring (and Adaptive, which runs them) those
// are views of the leader connection's receive buffer. The leader returns
// once it has handed the result over and its connection's next requests are
// received into that buffer, while the follower formats the error later, as
// its logger or span would: here an outer middleware formats it after the
// leader's connection has sent two more requests (an async handler's request
// sits in a double buffer, so it takes two) with the same layout. The
// follower must read the leader's first request.
func TestFollowerErrorSurvivesLeaderNextRequest(t *testing.T) {
	for _, a := range c732Arms(t) {
		t.Run(a.name, func(t *testing.T) {
			joined := make(chan struct{}, 1)
			prev := sf.TestHookFollowerJoined
			sf.TestHookFollowerJoined = func() { joined <- struct{}{} }
			t.Cleanup(func() { sf.TestHookFollowerJoined = prev })
			leaderIn := make(chan struct{}, 1)
			gate := make(chan struct{})
			released := false
			release := func() {
				if !released {
					released = true
					close(gate)
				}
			}
			gotCh := make(chan string, 1)
			kv := store.NewMemoryKV()
			defer kv.Close()

			addr, stop := c732Start(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async})
				// Answers every request with 200, and reports the follower's
				// error once the gate opens.
				srv.Use(func(c *celeris.Context) error {
					if err := c.Next(); err != nil && c.Header("x-role") == "w" {
						<-gate
						gotCh <- err.Error()
					}
					if !c.IsWritten() {
						_ = c.String(200, "ok")
					}
					return nil
				})
				srv.Use(cache.New(cache.Config{Store: kv, Singleflight: true, KeyGenerator: func(*celeris.Context) string { return "one-key" }}))
				srv.GET("/c/:id", func(c *celeris.Context) error {
					if c.Header("x-hold") == "1" {
						leaderIn <- struct{}{}
						select {
						case <-joined:
						case <-time.After(10 * time.Second):
						}
						return errors.New(c.Header("x-err"))
					}
					return c.String(200, "fresh")
				}).Async()
				return srv
			})
			defer stop()
			// Before stop, which waits for the follower's handler.
			defer release()

			lconn, lbr := c732Dial(t, addr)
			defer func() { _ = lconn.Close() }()
			wconn, wbr := c732Dial(t, addr)
			defer func() { _ = wconn.Close() }()

			// The leader's first request, held until the follower joins.
			c732Write(t, lconn, "GET /c/aaaa HTTP/1.1\r\nHost: h\r\nX-Role: l\r\nX-Hold: 1\r\nX-Err: err-aaaa\r\n\r\n")
			select {
			case <-leaderIn:
			case <-time.After(10 * time.Second):
				t.Fatal("the leader's handler did not start")
			}
			c732Write(t, wconn, "GET /c/zzzz HTTP/1.1\r\nHost: h\r\nX-Role: w\r\nX-Hold: 0\r\nX-Err: err-zzzz\r\n\r\n")
			c732ReadAny(t, lbr)
			for _, v := range []string{"bbbb", "cccc"} {
				c732Write(t, lconn, "GET /c/"+v+" HTTP/1.1\r\nHost: h\r\nX-Role: l\r\nX-Hold: 0\r\nX-Err: err-"+v+"\r\n\r\n")
				c732ReadAny(t, lbr)
			}
			release()
			var got string
			select {
			case got = <-gotCh:
			case <-time.After(10 * time.Second):
				t.Fatal("the follower did not report the leader's error (did it coalesce?)")
			}
			c732ReadAny(t, wbr)
			t.Logf("MW732CACHEERR arm=%s follower saw %q (want %q)", a.name, got, "err-aaaa")
			if got != "err-aaaa" {
				t.Errorf("the follower's copy of the leader's error reads %q after the leader's connection sent its next requests; want %q", got, "err-aaaa")
			}
		})
	}
}

type c732Arm struct {
	name   string
	engine celeris.EngineType
	async  bool
}

// c732Arms returns std, and epoll, io_uring and Adaptive with sync and async
// handlers. With CELERIS_REQUIRE_IOURING_WORKERS=1 a kernel with no usable
// io_uring fails the test instead of dropping the io_uring arms.
func c732Arms(t *testing.T) []c732Arm {
	t.Helper()
	arms := []c732Arm{{"std", celeris.Std, false}, {"epoll", celeris.Epoll, false}, {"epoll-async", celeris.Epoll, true}}
	p := probe.Probe()
	usable := p.IOUringTier >= celerisengine.High && p.ProvidedBuffers
	if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
		// A ring can fail with ENOMEM against RLIMIT_MEMLOCK while the rings
		// of engines stopped moments ago are still charged.
		for deadline := time.Now().Add(10 * time.Second); !usable && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
			p = probe.Probe()
			usable = p.IOUringTier >= celerisengine.High && p.ProvidedBuffers
		}
	}
	switch {
	case usable:
		arms = append(arms, c732Arm{"io_uring", celeris.IOUring, false}, c732Arm{"io_uring-async", celeris.IOUring, true})
	case os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1":
		t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids dropping the io_uring arms", p.IOUringTier, p.KernelVersion)
	default:
		t.Logf("io_uring tier=%s kernel=%s: io_uring arms not run", p.IOUringTier, p.KernelVersion)
	}
	return append(arms, c732Arm{"adaptive", celeris.Adaptive, false}, c732Arm{"adaptive-async", celeris.Adaptive, true})
}

// c732Start starts the server mk builds on a fresh loopback listener and
// returns its address and a shutdown closure. It fails at once with Start's
// error if Start returns before the server is ready (celeris#706); a start
// that fails only with ENOMEM (io_uring ring memory still charged to
// RLIMIT_MEMLOCK) is retried with a new server for up to 10 s.
func c732Start(t *testing.T, mk func() *celeris.Server) (string, func()) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s := mk()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
		addr, err := c732WaitReady(s, done)
		if err == nil {
			return addr, func() { cancel(); <-done }
		}
		cancel()
		_ = ln.Close()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		t.Fatalf("server did not start: %v", err)
	}
}

func c732WaitReady(s *celeris.Server, done <-chan error) (string, error) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			if err == nil {
				err = errors.New("start returned before the server was ready")
			}
			return "", err
		default:
		}
		if a := s.Addr(); a != nil {
			if c, err := net.DialTimeout("tcp", a.String(), 100*time.Millisecond); err == nil {
				_ = c.Close()
				return a.String(), nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "", errors.New("server not ready within 30s")
}

func c732Dial(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	return conn, bufio.NewReader(conn)
}

func c732Write(t *testing.T, conn net.Conn, req string) {
	t.Helper()
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
}

// c732ReadAny reads one response of any status.
func c732ReadAny(t *testing.T, br *bufio.Reader) {
	t.Helper()
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read status: %v", err)
	}
	n := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		if line == "\r\n" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "content-length") {
			n, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	if _, err := br.Discard(n); err != nil {
		t.Fatalf("read body: %v", err)
	}
}
