//go:build linux

package cache_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/probe"
	"github.com/goceleris/celeris/middleware/cache"
)

// TestCustomKeyStillHitsAfterNextRequest pins the cache store's site of
// celeris#719 end to end.
//
// A KeyGenerator may return a request string; c.Path() is the obvious one,
// and on epoll and io_uring (and Adaptive, which runs them) it is a view of
// the connection's receive buffer. The default store, cache.MemoryStore, kept
// the key as its map key, so the key's bytes changed under the map when the
// connection sent its next request, and the next request for the first path
// from another connection missed and ran the handler again. Connection A
// requests /c/aaaa, then /c/bbbb with the same layout; connection B then
// requests /c/aaaa, which must be a HIT.
func TestCustomKeyStillHitsAfterNextRequest(t *testing.T) {
	for _, a := range mw719Arms(t) {
		t.Run(a.name, func(t *testing.T) {
			var runs atomic.Int32
			addr, stop := mw719Start(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async})
				srv.Use(cache.New(cache.Config{KeyGenerator: func(c *celeris.Context) string { return c.Path() }}))
				srv.GET("/c/:id", func(c *celeris.Context) error {
					runs.Add(1)
					return c.String(200, "body %s", c.Param("id"))
				})
				return srv
			})
			defer stop()

			req := func(p string) string { return "GET " + p + " HTTP/1.1\r\nHost: h\r\n\r\n" }
			ca, bra := mw719Dial(t, addr)
			defer func() { _ = ca.Close() }()
			for _, p := range []string{"/c/aaaa", "/c/bbbb"} {
				if st, _, _ := mw719Do(t, ca, bra, req(p)); st != 200 {
					t.Fatalf("first request for %s: %d", p, st)
				}
			}
			cb, brb := mw719Dial(t, addr)
			defer func() { _ = cb.Close() }()
			st, hdr, body := mw719Do(t, cb, brb, req("/c/aaaa"))
			t.Logf("MW719CACHE arm=%s repeat status=%d x-cache=%q body=%q handler runs=%d (want 2)", a.name, st, hdr["x-cache"], body, runs.Load())
			if st != 200 || hdr["x-cache"] != "HIT" || body != "body aaaa" || runs.Load() != 2 {
				t.Errorf("repeat of /c/aaaa from another connection: %d x-cache=%q %q with %d handler runs; want a HIT of %q and 2 runs", st, hdr["x-cache"], body, runs.Load(), "body aaaa")
			}
		})
	}
}

type mw719Arm struct {
	name   string
	engine celeris.EngineType
	async  bool
}

// mw719Arms returns std, and epoll, io_uring and Adaptive with sync and
// async handlers. With CELERIS_REQUIRE_IOURING_WORKERS=1 a kernel with no
// usable io_uring fails the test instead of dropping the io_uring arms.
func mw719Arms(t *testing.T) []mw719Arm {
	t.Helper()
	arms := []mw719Arm{{"std", celeris.Std, false}, {"epoll", celeris.Epoll, false}, {"epoll-async", celeris.Epoll, true}}
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
		arms = append(arms, mw719Arm{"io_uring", celeris.IOUring, false}, mw719Arm{"io_uring-async", celeris.IOUring, true})
	case os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1":
		t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids dropping the io_uring arms", p.IOUringTier, p.KernelVersion)
	default:
		t.Logf("io_uring tier=%s kernel=%s: io_uring arms not run", p.IOUringTier, p.KernelVersion)
	}
	return append(arms, mw719Arm{"adaptive", celeris.Adaptive, false}, mw719Arm{"adaptive-async", celeris.Adaptive, true})
}

// mw719Start starts the server mk builds on a fresh loopback listener and
// returns its address and a shutdown closure. It fails at once with Start's
// error if Start returns before the server is ready (celeris#706); a start
// that fails only with ENOMEM (io_uring ring memory still charged to
// RLIMIT_MEMLOCK) is retried with a new server for up to 10 s.
func mw719Start(t *testing.T, mk func() *celeris.Server) (string, func()) {
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
		addr, err := mw719WaitReady(s, done)
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

func mw719WaitReady(s *celeris.Server, done <-chan error) (string, error) {
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

func mw719Dial(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	return conn, bufio.NewReader(conn)
}

// mw719Do writes one request and reads its response: the status, the
// headers (lowercased names) and the body.
func mw719Do(t *testing.T, conn net.Conn, br *bufio.Reader, req string) (int, map[string]string, string) {
	t.Helper()
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status for %q: %v", strings.SplitN(req, "\r\n", 2)[0], err)
	}
	f := strings.Fields(line)
	if len(f) < 2 {
		t.Fatalf("status line %q", line)
	}
	status, _ := strconv.Atoi(f[1])
	hdr := map[string]string{}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		if l == "\r\n" {
			break
		}
		if k, v, ok := strings.Cut(l, ":"); ok {
			hdr[strings.ToLower(k)] = strings.TrimSpace(v)
		}
	}
	n, _ := strconv.Atoi(hdr["content-length"])
	body := make([]byte, n)
	if _, err := io.ReadFull(br, body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return status, hdr, string(body)
}
