//go:build linux

package celeris_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/singleflight"
	"github.com/goceleris/celeris/middleware/timeout"
)

// celeris#836 on the wire. An HTTP/2 stream's context was a view of the
// pooled stream, so once the stream was released and reset its context said
// "not cancelled" again. A context derived from c.Context() runs a
// propagation goroutine that reads the parent's Err after it sees the
// parent's Done closed; when the reset came in between, Err was nil and the
// context package panicked ("context: internal error: missing cancel
// error"), which killed the process. The 24 h soak 37140355449 lost the
// kitchen_sink refapp that way on epoll and io_uring, on both arches.

// n836 is the number of requests per cell, CELERIS_836_N to change it.
func n836(def int) int {
	if n, err := strconv.Atoi(os.Getenv("CELERIS_836_N")); err == nil && n > 0 {
		return n
	}
	if lean761() {
		return def / 4
	}
	return def
}

// dur836 is how long each soak arm runs, CELERIS_836_DUR to change it.
func dur836(def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv("CELERIS_836_DUR")); err == nil && d > 0 {
		return d
	}
	return def
}

func h2cClient836(conns int) *http.Client {
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Protocols: p, MaxConnsPerHost: conns}}
}

// TestH2DerivedContextDoesNotPanic836: handlers that derive a context from
// c.Context() and return at once (as `defer cancel()` does), on a sync and
// an async route, 64 requests in flight over 4 h2c connections. Before the
// fix the test binary died with the panic above on epoll, io_uring and
// adaptive. std is the control (its streams are HTTP/1 streams, whose
// context is context.Background()).
func TestH2DerivedContextDoesNotPanic836(t *testing.T) {
	derive := map[string]func(context.Context) (context.Context, context.CancelFunc){
		"WithCancel": context.WithCancel,
		"WithTimeout": func(p context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(p, time.Hour)
		},
	}
	for _, e := range engines761 {
		for _, route := range []string{"sync", "async"} {
			t.Run(e.name+"/"+route, func(t *testing.T) {
				addr := startServer761(t, e.eng, false, func(s *celeris.Server) {
					for name, d := range derive {
						r := s.GET("/"+name, func(c *celeris.Context) error {
							ctx, cancel := d(c.Context())
							cancel()
							if ctx.Err() == nil {
								return c.String(500, "derived context not done after cancel")
							}
							return c.String(200, "ok")
						})
						if route == "async" {
							r.Async()
						}
					}
				})
				cl := h2cClient836(4)
				defer cl.CloseIdleConnections()
				n := n836(8000)
				var wg sync.WaitGroup
				var bad, proto1 atomic.Int64
				var firstErr atomic.Value
				sem := make(chan struct{}, 64)
				for i := range n {
					sem <- struct{}{}
					wg.Add(1)
					go func() {
						defer func() { <-sem; wg.Done() }()
						path := "/WithCancel"
						if i%2 == 1 {
							path = "/WithTimeout"
						}
						resp, err := cl.Get("http://" + addr + path)
						if err != nil {
							bad.Add(1)
							firstErr.CompareAndSwap(nil, err.Error())
							return
						}
						b, _ := io.ReadAll(resp.Body)
						_ = resp.Body.Close()
						if resp.ProtoMajor != 2 {
							proto1.Add(1)
						}
						if resp.StatusCode != 200 || string(b) != "ok" {
							bad.Add(1)
							firstErr.CompareAndSwap(nil, fmt.Sprintf("%d %q", resp.StatusCode, b))
						}
					}()
				}
				wg.Wait()
				if p := proto1.Load(); p != 0 {
					t.Fatalf("%d of %d requests were not answered over HTTP/2: this cell did not test h2c", p, n)
				}
				if b := bad.Load(); b != 0 {
					t.Fatalf("%d of %d requests failed, first: %v", b, n, firstErr.Load())
				}
			})
		}
	}
}

// TestH2RequestContextEndsWithItsRequest836: a context a handler keeps after
// it returns (a goroutine it started, a context derived from it) is the
// context of that request only. Once the request is over it is cancelled for
// good, while later requests reuse the pooled streams it came from. Before
// the fix it read "not cancelled" again as soon as its stream was reset.
func TestH2RequestContextEndsWithItsRequest836(t *testing.T) {
	for _, e := range engines761 {
		if e.name == "std" {
			continue // context.Background() on std; nothing to end
		}
		for _, route := range []string{"sync", "async"} {
			t.Run(e.name+"/"+route, func(t *testing.T) {
				const kept = 64
				keptCtx := make(chan context.Context, kept)
				addr := startServer761(t, e.eng, false, func(s *celeris.Server) {
					r := s.GET("/keep", func(c *celeris.Context) error {
						select {
						case keptCtx <- c.Context():
						default:
						}
						return c.String(200, "ok")
					})
					if route == "async" {
						r.Async()
					}
				})
				cl := h2cClient836(1)
				defer cl.CloseIdleConnections()
				get := func() {
					resp, err := cl.Get("http://" + addr + "/keep")
					if err != nil {
						t.Fatal(err)
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					if resp.ProtoMajor != 2 {
						t.Fatalf("answered over %s, want HTTP/2", resp.Proto)
					}
				}
				for range kept {
					get()
				}
				// More requests reuse the released streams.
				for range 4 * kept {
					get()
				}
				// Received, not ranged over after a close: the handler's
				// send happens before its response, but the race detector
				// does not see the socket in between, and a close would be
				// reported as racing the sends.
				for i := 1; i <= kept; i++ {
					ctx := <-keptCtx
					// The stream is released just after the response is
					// written; give that a moment.
					deadline := time.Now().Add(5 * time.Second)
					for ctx.Err() == nil && time.Now().Before(deadline) {
						time.Sleep(time.Millisecond)
					}
					if !errors.Is(ctx.Err(), context.Canceled) {
						t.Fatalf("kept context %d: Err = %v after its request ended and its stream was reused, want context.Canceled", i, ctx.Err())
					}
					select {
					case <-ctx.Done():
					default:
						t.Fatalf("kept context %d: Done open after its request ended", i)
					}
					child, cancel := context.WithCancel(ctx)
					if child.Err() == nil {
						t.Fatalf("kept context %d: a context derived from it after its request ended is live", i)
					}
					cancel()
				}
			})
		}
	}
}

// The soak arms (evidence: cluster-reads-20261004/soak-37140355449/h2c-hang):
// the probatorium h2c walker sends `GET /` with Upgrade: h2c to kitchen_sink,
// whose global chain is timeout(5s) then singleflight, and which has no route
// for "/". Since celeris#916 an unmatched request runs the global chain;
// every walker fire has the same singleflight key, a follower waits on
// c.Context().Done(), that promotes timeout's lazy context to a
// context.WithDeadline of the stream's context, and its propagation goroutine
// raced the stream's release. A: timeout+singleflight; B: timeout only (the
// control: nothing waits on Done); D: as A, plus a NotFound handler (how the
// chain ran for an unmatched request before #916); E: the std engine. Before
// the fix A and D killed the process on epoll (in 1-6 s in the container
// repro, 3 of 3), B and E survived.

// preamble836 is the walker's h2c upgrade request (probatorium
// validation/h2c.go at e192920).
func preamble836(hostPort string) []byte {
	const settingsB64 = "AAQAAP__AAMAAABk"
	return []byte("GET / HTTP/1.1\r\nHost: " + hostPort + "\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: " + settingsB64 + "\r\n\r\n")
}

type tally836 struct {
	sent, upgraded, declined, other, rst, dialFail, writeFail atomic.Int64
	hangEOF, hangTimeout, hangReset, hangOther                atomic.Int64
	firstHang                                                 atomic.Value
}

func (t *tally836) String() string {
	return fmt.Sprintf("sent=%d upgraded=%d declined=%d other=%d rst=%d hang_eof=%d hang_timeout=%d hang_reset=%d hang_other=%d dial_fail=%d write_fail=%d",
		t.sent.Load(), t.upgraded.Load(), t.declined.Load(), t.other.Load(), t.rst.Load(), t.hangEOF.Load(),
		t.hangTimeout.Load(), t.hangReset.Load(), t.hangOther.Load(), t.dialFail.Load(), t.writeFail.Load())
}

func (t *tally836) hangs() int64 {
	return t.hangEOF.Load() + t.hangTimeout.Load() + t.hangReset.Load() + t.hangOther.Load()
}

// fire836 is one walker fire, classified as fireH2CChurn does. mode 0 resets
// before reading, 1 reads the 101, 2 reads it and sends a broken preface.
func fire836(addr string, mode int, t *tally836) {
	t.sent.Add(1)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.dialFail.Add(1)
		time.Sleep(10 * time.Millisecond)
		return
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(preamble836(addr)); err != nil {
		t.writeFail.Add(1)
		return
	}
	if mode == 0 {
		t.rst.Add(1)
		return
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		switch {
		case err == nil:
			t.hangOther.Add(1)
		case errors.Is(err, os.ErrDeadlineExceeded):
			t.hangTimeout.Add(1)
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			t.hangEOF.Add(1)
		case errors.Is(err, syscall.ECONNRESET):
			t.hangReset.Add(1)
		default:
			t.hangOther.Add(1)
		}
		t.firstHang.CompareAndSwap(nil, fmt.Sprintf("mode %d: %d bytes, %v", mode, n, err))
		return
	}
	switch s := string(buf[:n]); {
	case strings.HasPrefix(s, "HTTP/1.1 101"):
		t.upgraded.Add(1)
		if mode == 2 {
			_, _ = conn.Write([]byte("PRI * HTTP/2"))
		}
	case strings.HasPrefix(s, "HTTP/1.0 "), strings.HasPrefix(s, "HTTP/1.1 "):
		t.declined.Add(1)
	default:
		t.other.Add(1)
	}
}

func TestH2CUnmatchedThroughTimeoutSingleflight836(t *testing.T) {
	arms := []struct {
		name             string
		singleflight, nf bool
		probers          int
		defaultDur       time.Duration
	}{
		{"A-timeout+singleflight", true, false, 24, 3 * time.Second},
		{"B-timeout-only", false, false, 24, time.Second},
		{"D-timeout+singleflight+NotFound", true, true, 24, 3 * time.Second},
	}
	for _, e := range engines761 {
		for _, arm := range arms {
			t.Run(e.name+"/"+arm.name, func(t *testing.T) {
				addr := startGlobalChainServer836(t, celeris.Config{
					Engine:   e.eng,
					Protocol: celeris.Auto,
					// kitchen_sink's AsyncHandlers, except under the race
					// detector: the async h2c upgrade path has a data race
					// of its own on epoll (celeris#865, switchToH2Local vs
					// checkTimeouts), which this test would report instead of
					// testing #836. The handlers of an h2c connection run
					// inline either way. Lift this with #865's fix.
					AsyncHandlers:   !raceOn761,
					ReadTimeout:     30 * time.Second,
					WriteTimeout:    30 * time.Second,
					IdleTimeout:     120 * time.Second,
					ShutdownTimeout: 10 * time.Second,
				}, func(s *celeris.Server) {
					s.Use(timeout.New(timeout.Config{Timeout: 5 * time.Second}))
					if arm.singleflight {
						s.Use(singleflight.New())
					}
					if arm.nf {
						s.NotFound(func(c *celeris.Context) error { return c.String(404, "404 Not Found") })
					}
					s.GET("/api/short", func(c *celeris.Context) error { return c.String(200, "ok") })
				})
				var tl tally836
				stop := time.Now().Add(dur836(arm.defaultDur))
				var wg sync.WaitGroup
				for i := range arm.probers {
					wg.Add(1)
					go func(seed uint64) {
						defer wg.Done()
						rng := rand.New(rand.NewPCG(seed, ^seed^0xdeadbabecafef00d))
						for time.Now().Before(stop) {
							fire836(addr, rng.IntN(3), &tl)
						}
					}(uint64(i + 1))
				}
				wg.Wait()
				t.Logf("%s %s: %s", e.name, arm.name, &tl)
				if tl.upgraded.Load() == 0 {
					t.Fatalf("no fire was upgraded: this arm did not test h2c (%s)", &tl)
				}
				if h := tl.hangs(); h != 0 {
					t.Fatalf("%d fires got no answer to their upgrade (first: %v): %s", h, tl.firstHang.Load(), &tl)
				}
				if d := tl.declined.Load() + tl.other.Load(); d != 0 {
					t.Fatalf("%d fires were not upgraded: %s", d, &tl)
				}
			})
		}
	}
}

// startGlobalChainServer836 is startServerConfig761 for a server with global
// middleware: Use must come before any route, and startServerConfig761
// registers /ping first. setup installs the middleware and the routes; the
// readiness probe /ping is registered after it.
func startGlobalChainServer836(t *testing.T, cfg celeris.Config, setup func(*celeris.Server)) string {
	t.Helper()
	retryUntil := time.Now().Add(30 * time.Second)
	for {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		cfg.Addr = addr
		s := celeris.New(cfg)
		setup(s)
		s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
		startDone := make(chan error, 1)
		go func() { startDone <- s.Start() }()
		if err = waitReady761(addr, startDone); err == nil {
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = s.Shutdown(ctx)
				select {
				case <-startDone:
				case <-time.After(15 * time.Second):
					t.Errorf("Start did not return within 15s of Shutdown")
				}
			})
			return addr
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.Shutdown(ctx)
		cancel()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(retryUntil) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		t.Fatalf("server did not start: %v", err)
	}
}
