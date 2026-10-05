//go:build linux

package recovery_test

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
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/probe"
	"github.com/goceleris/celeris/middleware/recovery"
)

// keepHandler keeps every record, cloned as slog requires, and formats
// nothing until asked: what an asynchronous or batching handler does, and
// what the recovery package doc recommends (slog.New(myAsyncHandler)).
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

// TestPanicRecordsSurviveNextRequest pins the recovery site of celeris#732
// (celeris#742 item 1).
//
// The middleware logs the method, path and request ID, and the panic value,
// to Config.Logger, any *slog.Logger. slog lets a Handler keep a Record
// after Handle returns by calling Record.Clone, which shares the strings;
// asynchronous and batching handlers do, and the package doc recommends one.
// On epoll and io_uring (and Adaptive, which runs them) the method (for one
// the H1 parser does not intern), the path, a request ID taken from a header
// and a panic value read from a header are views of the connection's receive
// buffer, which the engine reuses for the connection's next request.
//
// Every place the middleware logs is a route here: a panic (logPanic, with
// and without the stack), a broken pipe (handleBrokenPipe), an ErrorHandler
// that panics (safeCallHandler), a panic after the request context was
// cancelled, and one after the response was committed. Each route gets three
// requests with the same layout and different values over one keep-alive
// connection; every kept record must still read its own request.
func TestPanicRecordsSurviveNextRequest(t *testing.T) {
	methods := []string{"TRACE", "PURGE", "MKCOL"}
	// rec is one record a request leaves: its message, and whether its
	// "error" attribute is the panic value, which the handler reads from the
	// X-Panic header.
	type rec struct {
		msg      string
		panicVal bool
	}
	type site struct {
		name    string
		cfg     func(*slog.Logger) recovery.Config
		handler celeris.HandlerFunc
		recs    []rec // in the order the middleware logs them
	}
	panicHeader := func(c *celeris.Context) error { panic(c.Header("x-panic")) }
	sites := []site{
		{"stack", func(l *slog.Logger) recovery.Config { return recovery.Config{Logger: l, StackSize: 4096} },
			panicHeader, []rec{{"panic recovered", true}}},
		{"nostack", func(l *slog.Logger) recovery.Config { return recovery.Config{Logger: l} }, // StackSize 0
			panicHeader, []rec{{"panic recovered", true}}},
		{"brokenpipe", func(l *slog.Logger) recovery.Config { return recovery.Config{Logger: l} },
			func(*celeris.Context) error { panic(syscall.EPIPE) }, []rec{{"broken pipe", false}}},
		{"errhandler", func(l *slog.Logger) recovery.Config {
			return recovery.Config{Logger: l, ErrorHandler: func(*celeris.Context, any) error { panic("handler down") }}
		}, panicHeader, []rec{{"panic recovered", true}, {"panic in error handler", false}}},
		{"cancelled", func(l *slog.Logger) recovery.Config { return recovery.Config{Logger: l} },
			func(c *celeris.Context) error {
				ctx, cancel := context.WithCancel(c.Context())
				c.SetContext(ctx)
				cancel()
				panic(c.Header("x-panic"))
			}, []rec{{"panic recovered", true}, {"panic after context cancelled", true}}},
		{"committed", func(l *slog.Logger) recovery.Config { return recovery.Config{Logger: l} },
			func(c *celeris.Context) error {
				_ = c.String(200, "ok")
				panic(c.Header("x-panic"))
			}, []rec{{"panic recovered", true}, {"panic after response committed", true}}},
	}
	for _, a := range mwArms(t) {
		t.Run(a.name, func(t *testing.T) {
			var mu sync.Mutex
			var recs []slog.Record
			logger := slog.New(keepHandler{mu: &mu, recs: &recs})
			addr, stop := startKeptServer(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async})
				// What the requestid middleware stores from a request header.
				srv.Use(func(c *celeris.Context) error {
					c.SetRequestID(c.Header("x-request-id"))
					return c.Next()
				})
				for _, s := range sites {
					mw := recovery.New(s.cfg(logger))
					for _, m := range methods {
						srv.Handle(m, "/r/"+s.name+"/:id", mw, s.handler)
					}
				}
				return srv
			})
			defer stop()

			type want struct{ msg, method, path, rid, err string }
			var wants []want
			for _, s := range sites {
				conn, br := keptDial(t, addr)
				for i, m := range methods {
					v := strings.Repeat(string(rune('a'+i)), 4)
					anyRoundTrip(t, conn, br, m+" /r/"+s.name+"/"+v+" HTTP/1.1\r\nHost: h\r\nX-Request-Id: rid-"+v+
						"\r\nX-Panic: pv-"+v+"\r\n\r\n")
					for _, r := range s.recs {
						w := want{msg: r.msg, method: m, path: "/r/" + s.name + "/" + v, rid: "rid-" + v}
						if r.panicVal {
							w.err = "pv-" + v
						}
						wants = append(wants, w)
					}
				}
				_ = conn.Close()
			}

			// Records are written before the response leaves on every path
			// but the committed one, which logs after c.String.
			var kept []slog.Record
			for i := 0; i < 200; i++ {
				mu.Lock()
				kept = append(kept[:0], recs...)
				mu.Unlock()
				if len(kept) >= len(wants) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(kept) != len(wants) {
				t.Fatalf("got %d records, want %d", len(kept), len(wants))
			}
			sort.SliceStable(kept, func(i, j int) bool { return kept[i].Time.Before(kept[j].Time) })
			var wrong []string
			for i, r := range kept {
				got := map[string]string{}
				r.Attrs(func(a slog.Attr) bool {
					if a.Value.Kind() == slog.KindString {
						got[a.Key] = a.Value.String()
					}
					return true
				})
				w := wants[i]
				check := func(k, wantV string) {
					if got[k] != wantV {
						wrong = append(wrong, fmt.Sprintf("record %d (%s, want %s %s) %s: %q (want %q)", i+1, r.Message, w.msg, w.path, k, got[k], wantV))
					}
				}
				if r.Message != w.msg {
					wrong = append(wrong, fmt.Sprintf("record %d message %q (want %q)", i+1, r.Message, w.msg))
				}
				check("method", w.method)
				check("path", w.path)
				check("request_id", w.rid)
				if w.err != "" {
					check("error", w.err)
				}
			}
			t.Logf("MW742RECOVERY arm=%s records=%d wrong=%d", a.name, len(kept), len(wrong))
			if len(wrong) > 0 {
				t.Errorf("a kept panic record reads other bytes after the connection's later requests:\n  %s", strings.Join(wrong, "\n  "))
			}
		})
	}
}

type keptArm struct {
	name   string
	engine celeris.EngineType
	async  bool
}

// mwArms returns std, and epoll, io_uring and Adaptive with sync and async
// handlers. With CELERIS_REQUIRE_IOURING_WORKERS=1 a kernel with no usable
// io_uring fails the test instead of dropping the io_uring arms.
func mwArms(t *testing.T) []keptArm {
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
	return append(arms, keptArm{"adaptive", celeris.Adaptive, false}, keptArm{"adaptive-async", celeris.Adaptive, true})
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

// anyRoundTrip writes one request and reads its whole response, whatever its
// status: the recovery paths answer 500, or 200 when the response was
// committed before the panic.
func anyRoundTrip(t *testing.T, conn net.Conn, br *bufio.Reader, req string) {
	t.Helper()
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status for %q: %v", strings.SplitN(req, "\r\n", 2)[0], err)
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
	if f := strings.Fields(status); len(f) < 2 {
		t.Fatalf("status %q for %q", strings.TrimSpace(status), strings.SplitN(req, "\r\n", 2)[0])
	}
}
