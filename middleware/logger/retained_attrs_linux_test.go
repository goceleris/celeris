//go:build linux

package logger_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/middleware/logger"
	"github.com/goceleris/celeris/probe"
)

// keepHandler keeps every record, cloned as slog requires, and formats
// nothing until asked: what an asynchronous or batching handler does.
type keepHandler struct {
	mu   *sync.Mutex
	recs *[]slog.Record
}

func (h keepHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h keepHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	*h.recs = append(*h.recs, r.Clone())
	h.mu.Unlock()
	return nil
}

func (h keepHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h keepHandler) WithGroup(string) slog.Handler      { return h }

// TestKeptRecordSurvivesNextRequest pins the logger site of celeris#732.
//
// slog lets a Handler keep a Record after Handle returns by calling
// Record.Clone, which shares the strings. On epoll and io_uring the method
// (for one the H1 parser does not intern), path, query, Host, User-Agent,
// Referer and client IP the middleware logs, and the request ID, context
// value, response header and Fields value that code derives from request
// headers, are views of the connection's receive buffer, which the engine
// reuses for the connection's next request. Three requests with the same
// layout and different values go over one keep-alive connection; every kept
// record must still read its own request.
func TestKeptRecordSurvivesNextRequest(t *testing.T) {
	methods := []string{"TRACE", "PURGE", "MKCOL"}
	for _, a := range keptArms(t) {
		t.Run(a.name, func(t *testing.T) {
			var mu sync.Mutex
			var recs []slog.Record
			addr, stop := startKeptServer(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async})
				srv.Use(logger.New(logger.Config{
					Output:             slog.New(keepHandler{mu: &mu, recs: &recs}),
					LogHost:            true,
					LogUserAgent:       true,
					LogReferer:         true,
					LogQueryParams:     true,
					LogContextKeys:     []string{"tenant"},
					LogResponseHeaders: []string{"x-echo"},
					Fields: func(c *celeris.Context, _ time.Duration) []slog.Attr {
						return []slog.Attr{slog.Group("g", slog.String("tenant", c.Header("x-tenant")))}
					},
				}))
				h := func(c *celeris.Context) error {
					c.SetRequestID(c.Header("x-request-id"))
					c.Set("tenant", c.Header("x-tenant"))
					c.SetHeader("x-echo", c.Header("x-id"))
					// Not String or Blob: they reorder the response headers
					// in place, and ResponseHeaders no longer lists x-echo.
					return c.NoContent(204)
				}
				for _, m := range methods {
					srv.Handle(m, "/l/:id", h)
				}
				return srv
			})
			defer stop()

			conn, br := keptDial(t, addr)
			defer func() { _ = conn.Close() }()
			var want []map[string]string
			for i, m := range methods {
				v := strings.Repeat(string(rune('a'+i)), 4)
				keptRoundTrip(t, conn, br, m+" /l/"+v+"?q="+v+" HTTP/1.1\r\nHost: h-"+v+".example\r\nUser-Agent: agent-"+v+
					"\r\nReferer: https://r-"+v+".example/\r\nX-Forwarded-For: 10.0.0."+strconv.Itoa(i+1)+
					"\r\nX-Request-Id: rid-"+v+"\r\nX-Tenant: tenant-"+v+"\r\nX-Id: id-"+v+"\r\n\r\n")
				want = append(want, map[string]string{
					"method": m, "path": "/l/" + v, "query": "q=" + v, "host": "h-" + v + ".example",
					"user_agent": "agent-" + v, "referer": "https://r-" + v + ".example/", "client_ip": "10.0.0." + strconv.Itoa(i+1),
					"request_id": "rid-" + v, "ctx.tenant": "tenant-" + v, "resp_header.x-echo": "id-" + v, "g.tenant": "tenant-" + v,
				})
			}

			// The middleware logs after the response reaches the client.
			var kept []slog.Record
			for i := 0; i < 200; i++ {
				mu.Lock()
				kept = append(kept[:0], recs...)
				mu.Unlock()
				if len(kept) >= len(methods) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(kept) != len(methods) {
				t.Fatalf("got %d records, want %d", len(kept), len(methods))
			}
			sort.Slice(kept, func(i, j int) bool { return kept[i].Time.Before(kept[j].Time) })
			var wrong []string
			for i, r := range kept {
				got := map[string]string{}
				r.Attrs(func(a slog.Attr) bool {
					flattenStrings(got, "", a)
					return true
				})
				keys := make([]string, 0, len(want[i]))
				for k := range want[i] {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					if got[k] != want[i][k] {
						wrong = append(wrong, fmt.Sprintf("record %d %s: %q (want %q)", i+1, k, got[k], want[i][k]))
					}
				}
			}
			t.Logf("KEPT732LOGGER arm=%s records=%d wrong=%d", a.name, len(kept), len(wrong))
			if len(wrong) > 0 {
				t.Errorf("a kept record reads other bytes after the connection's later requests:\n  %s", strings.Join(wrong, "\n  "))
			}
		})
	}
}

func flattenStrings(dst map[string]string, prefix string, a slog.Attr) {
	switch a.Value.Kind() {
	case slog.KindString:
		dst[prefix+a.Key] = a.Value.String()
	case slog.KindGroup:
		for _, g := range a.Value.Group() {
			flattenStrings(dst, prefix+a.Key+".", g)
		}
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
// (engine/iouring/ring_budget_linux_test.go).
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
