//go:build linux

package adapters_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/probe"
	"github.com/goceleris/celeris/middleware/adapters"
)

// TestWrapMiddlewareKeptStringsSurviveNextRequest pins the
// middleware/adapters site of celeris#732.
//
// net/http lets a middleware keep the request's strings after it returns (a
// rate limiter's map key, a log line queued for later). On epoll and
// io_uring the method, path, query, Host and header strings buildRequest
// handed over were views of the connection's receive buffer, which the
// engine reuses for the connection's next request. Three requests with the
// same layout and different values go over one keep-alive connection; the
// wrapped middleware keeps its request's strings, and after the third
// request each kept set must still read its own request. The methods are
// ones the H1 parser does not intern, the header name "111-111" is one
// net/http's canonicalization returns unchanged (it has no letters), and no
// request has a query (with one, the URL was a new string and the path a
// copy by accident).
func TestWrapMiddlewareKeptStringsSurviveNextRequest(t *testing.T) {
	methods := []string{"TRACE", "PURGE", "MKCOL"}
	for _, a := range keptArms(t) {
		t.Run(a.name, func(t *testing.T) {
			kept := make(chan map[string]string, len(methods))
			mw := func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					got := map[string]string{
						"method": r.Method,
						"path":   r.URL.Path,
						"query":  r.URL.RawQuery,
						"host":   r.Host,
						"x-id":   r.Header.Get("X-Id"),
					}
					for k := range r.Header {
						if strings.HasSuffix(k, "-111") || strings.HasSuffix(k, "-222") || strings.HasSuffix(k, "-333") {
							got["digit-key"] = k
						}
					}
					kept <- got
					next.ServeHTTP(w, r)
				})
			}
			addr, stop := startKeptServer(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async})
				srv.Use(adapters.WrapMiddleware(mw))
				for _, m := range methods {
					srv.Handle(m, "/w/:id", func(c *celeris.Context) error { return c.String(200, "ok") })
				}
				return srv
			})
			defer stop()

			conn, br := keptDial(t, addr)
			defer func() { _ = conn.Close() }()
			var want []map[string]string
			for i, m := range methods {
				v := strings.Repeat(string(rune('a'+i)), 4)
				d := strings.Repeat(strconv.Itoa(i+1), 3)
				keptRoundTrip(t, conn, br, m+" /w/"+v+" HTTP/1.1\r\nHost: h-"+v+".example\r\nX-Id: id-"+v+"\r\n"+d+"-"+d+": k\r\n\r\n")
				want = append(want, map[string]string{
					"method": m, "path": "/w/" + v, "query": "", "host": "h-" + v + ".example",
					"x-id": "id-" + v, "digit-key": d + "-" + d,
				})
			}
			keptCompare(t, a.name, "KEPT732WRAP", want, kept)
		})
	}
}

// keptCompare reads one kept map per request from kept and reports every
// field that no longer reads its own request.
func keptCompare(t *testing.T, arm, tag string, want []map[string]string, kept <-chan map[string]string) {
	t.Helper()
	var wrong []string
	for i, w := range want {
		var got map[string]string
		select {
		case got = <-kept:
		case <-time.After(10 * time.Second):
			t.Fatalf("request %d: the handler did not report", i+1)
		}
		keys := make([]string, 0, len(w))
		for k := range w {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if got[k] != w[k] {
				wrong = append(wrong, fmt.Sprintf("request %d %s: %q (want %q)", i+1, k, got[k], w[k]))
			}
		}
	}
	t.Logf("%s arm=%s requests=%d wrong=%d", tag, arm, len(want), len(wrong))
	if len(wrong) > 0 {
		t.Errorf("strings kept from a request read other bytes after the connection's later requests:\n  %s", strings.Join(wrong, "\n  "))
	}
}

type keptArm struct {
	name   string
	engine celeris.EngineType
	async  bool
}

// keptArms returns std, and epoll and io_uring with sync and async
// handlers. With CELERIS_REQUIRE_IOURING_WORKERS=1 a kernel with no usable
// io_uring fails the test instead of dropping the io_uring arms.
func keptArms(t *testing.T) []keptArm {
	t.Helper()
	arms := []keptArm{
		{"std", celeris.Std, false},
		{"epoll", celeris.Epoll, false},
		{"epoll-async", celeris.Epoll, true},
	}
	if ok, p := keptProbeIOUring(); ok {
		arms = append(arms, keptArm{"io_uring", celeris.IOUring, false}, keptArm{"io_uring-async", celeris.IOUring, true})
	} else if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
		t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids dropping the io_uring arms", p.IOUringTier, p.KernelVersion)
	} else {
		t.Logf("io_uring tier=%s kernel=%s: io_uring arms not run", p.IOUringTier, p.KernelVersion)
	}
	return arms
}

// keptProbeIOUring probes the kernel's io_uring support. With
// CELERIS_REQUIRE_IOURING_WORKERS=1 a probe that finds no usable ring is
// retried for up to 10 s: the probe's ring can fail with ENOMEM against
// RLIMIT_MEMLOCK while the rings of engines stopped moments ago, or of
// another test binary of the same user, are still charged
// (internal/engine/iouring/ring_budget_linux_test.go).
func keptProbeIOUring() (usable bool, p celerisengine.CapabilityProfile) {
	p = probe.Probe()
	usable = p.IOUringTier >= celerisengine.High && p.ProvidedBuffers
	if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") != "1" {
		return usable, p
	}
	for deadline := time.Now().Add(10 * time.Second); !usable && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		p = probe.Probe()
		usable = p.IOUringTier >= celerisengine.High && p.ProvidedBuffers
	}
	return usable, p
}

// startKeptServer starts the server mk builds on a fresh loopback listener
// and returns its address and a shutdown closure. A start that fails only
// with ENOMEM (io_uring ring memory still charged to RLIMIT_MEMLOCK, see
// keptProbeIOUring) is retried with a new server for up to 10 s.
func startKeptServer(t *testing.T, mk func() *celeris.Server) (string, func()) {
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
		addr, err := keptWaitReady(s, done)
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

func keptWaitReady(s *celeris.Server, done <-chan error) (string, error) {
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

func keptDial(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	return conn, bufio.NewReader(conn)
}

// keptRoundTrip writes one request and reads its response; it fails the
// test unless the status is 2xx.
func keptRoundTrip(t *testing.T, conn net.Conn, br *bufio.Reader, req string) {
	t.Helper()
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	status, err := br.ReadString('\n')
	if err != nil {
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
	if f := strings.Fields(status); len(f) < 2 || f[1][0] != '2' {
		t.Fatalf("status %q for %q", strings.TrimSpace(status), strings.SplitN(req, "\r\n", 2)[0])
	}
}
