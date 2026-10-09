//go:build linux

package singleflight

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/probe"
)

// TestWaiterResponseHeadersSurviveLeaderNextRequest pins the singleflight
// site of the celeris#732 class (request strings kept past the request),
// which #742 does not list.
//
// A waiter is answered with the leader's response headers. Their values can
// be request strings: middleware echo request headers into the response
// (requestid's X-Request-Id, cors's Access-Control-Allow-Origin), and on
// epoll and io_uring (and Adaptive, which runs them) those are views of the
// leader connection's receive buffer. The leader returns once it has handed
// the entry over, and its connection's next request is received into that
// buffer, while a waiter may serialize the headers later: here an outer
// middleware buffers the waiter's response (as compress or etag do) and
// flushes it only after the leader's connection has sent its next requests.
// The waiter's echoed header must still read the leader's first request.
//
// Coalescing needs the leader and the waiter in flight at once, so the route
// is marked Async: a handler that runs inline blocks the I/O worker that
// runs it, and so does the first run of a route that is async only by the
// server default (AsyncHandlers routes start inline, celeris#356), which
// would hold the waiter's request behind the leader on a one-worker ring.
func TestWaiterResponseHeadersSurviveLeaderNextRequest(t *testing.T) {
	for _, a := range sfArms(t) {
		t.Run(a.name, func(t *testing.T) {
			joined := make(chan struct{}, 1)
			prev := testHookWaiterJoined
			testHookWaiterJoined = func() { joined <- struct{}{} }
			t.Cleanup(func() { testHookWaiterJoined = prev })
			leaderIn := make(chan struct{}, 1)
			gate, release := sfGate()

			addr, stop := sfStartServer(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async})
				srv.Use(func(c *celeris.Context) error {
					if c.Header("x-role") != "w" {
						return c.Next()
					}
					c.BufferResponse()
					err := c.Next()
					<-gate
					if ferr := c.FlushResponse(); ferr != nil && err == nil {
						err = ferr
					}
					return err
				})
				srv.Use(New(Config{KeyFunc: func(*celeris.Context) string { return "one-key" }}))
				srv.GET("/sf/:id", func(c *celeris.Context) error {
					c.SetHeader("x-echo", c.Header("x-echo"))
					if c.Header("x-hold") == "1" {
						leaderIn <- struct{}{}
						select {
						case <-joined:
						case <-time.After(10 * time.Second):
						}
					}
					// The content type, too, as a request string.
					return c.Blob(200, c.Header("x-ct"), []byte("ok"))
				}).Async()
				return srv
			})
			defer stop()
			// Before stop, which waits for the waiter's handler: a failure
			// below must not leave it blocked on the gate.
			defer release()

			lconn, lbr := sfDial(t, addr)
			defer func() { _ = lconn.Close() }()
			wconn, wbr := sfDial(t, addr)
			defer func() { _ = wconn.Close() }()

			// The leader's first request, held until the waiter joins.
			sfWrite(t, lconn, "GET /sf/aaaa HTTP/1.1\r\nHost: h\r\nX-Role: l\r\nX-Hold: 1\r\nX-Echo: echo-aaaa\r\nX-Ct: text/x-aaaa\r\n\r\n")
			select {
			case <-leaderIn:
			case <-time.After(10 * time.Second):
				t.Fatal("the leader's handler did not start")
			}
			sfWrite(t, wconn, "GET /sf/zzzz HTTP/1.1\r\nHost: h\r\nX-Role: w\r\nX-Hold: 0\r\nX-Echo: echo-zzzz\r\nX-Ct: text/x-zzzz\r\n\r\n")
			if got, _ := sfReadEcho(t, lbr); got != "echo-aaaa" {
				t.Fatalf("leader's first response x-echo %q, want echo-aaaa", got)
			}
			// The leader's connection sends its next requests, with the same
			// layout, while the waiter's response is still buffered. Two of
			// them: an async handler's request is parsed from a double
			// buffer, so the first request's bytes are overwritten by the
			// request after next.
			for _, v := range []string{"bbbb", "cccc"} {
				sfWrite(t, lconn, "GET /sf/"+v+" HTTP/1.1\r\nHost: h\r\nX-Role: l\r\nX-Hold: 0\r\nX-Echo: echo-"+v+"\r\nX-Ct: text/x-"+v+"\r\n\r\n")
				if got, _ := sfReadEcho(t, lbr); got != "echo-"+v {
					t.Fatalf("leader's later response x-echo %q, want echo-%s", got, v)
				}
			}
			release()
			got, ct := sfReadEcho(t, wbr)
			t.Logf("MW742SINGLEFLIGHT arm=%s waiter x-echo=%q content-type=%q (want %q, %q)", a.name, got, ct, "echo-aaaa", "text/x-aaaa")
			if got != "echo-aaaa" || ct != "text/x-aaaa" {
				t.Errorf("the waiter's copy of the leader's x-echo and content-type reads %q, %q after the leader's connection sent its next requests; want %q, %q", got, ct, "echo-aaaa", "text/x-aaaa")
			}
		})
	}
}

// sfGate returns a gate the waiter's middleware blocks on and its release,
// which closes it once however often it is called.
func sfGate() (<-chan struct{}, func()) {
	gate := make(chan struct{})
	var once sync.Once
	return gate, func() { once.Do(func() { close(gate) }) }
}

type sfArm struct {
	name   string
	engine celeris.EngineType
	async  bool
}

// sfArms returns std, and epoll, io_uring and Adaptive with sync and async
// handlers by default. With CELERIS_REQUIRE_IOURING_WORKERS=1 a kernel with
// no usable io_uring fails the test instead of dropping the io_uring arms.
func sfArms(t *testing.T) []sfArm {
	t.Helper()
	arms := []sfArm{{"std", celeris.Std, false}, {"epoll", celeris.Epoll, false}, {"epoll-async", celeris.Epoll, true}}
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
		arms = append(arms, sfArm{"io_uring", celeris.IOUring, false}, sfArm{"io_uring-async", celeris.IOUring, true})
	case os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1":
		t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids dropping the io_uring arms", p.IOUringTier, p.KernelVersion)
	default:
		t.Logf("io_uring tier=%s kernel=%s: io_uring arms not run", p.IOUringTier, p.KernelVersion)
	}
	return append(arms, sfArm{"adaptive", celeris.Adaptive, false}, sfArm{"adaptive-async", celeris.Adaptive, true})
}

// sfStartServer starts the server mk builds on a fresh loopback listener and
// returns its address and a shutdown closure. A start that fails only with
// ENOMEM (io_uring ring memory still charged to RLIMIT_MEMLOCK) is retried
// with a new server for up to 10 s.
func sfStartServer(t *testing.T, mk func() *celeris.Server) (string, func()) {
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
		addr, err := sfWaitReady(s, done)
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

// sfWaitReady waits until the server accepts a connection, failing at once
// with Start's error if Start returns first (celeris#706).
func sfWaitReady(s *celeris.Server, done <-chan error) (string, error) {
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

func sfDial(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	return conn, bufio.NewReader(conn)
}

func sfWrite(t *testing.T, conn net.Conn, req string) {
	t.Helper()
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
}

// sfReadEcho reads one response, fails the test unless its status is 200,
// and returns its x-echo and content-type headers.
func sfReadEcho(t *testing.T, br *bufio.Reader) (string, string) {
	t.Helper()
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	n, echo, ct := 0, "", ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		if line == "\r\n" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.ToLower(k) {
		case "content-length":
			n, _ = strconv.Atoi(strings.TrimSpace(v))
		case "x-echo":
			echo = strings.TrimSpace(v)
		case "content-type":
			ct = strings.TrimSpace(v)
		}
	}
	if _, err := br.Discard(n); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if f := strings.Fields(status); len(f) < 2 || f[1] != "200" {
		t.Fatalf("status %q", strings.TrimSpace(status))
	}
	return echo, ct
}
