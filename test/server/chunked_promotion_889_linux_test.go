//go:build linux

package celeris_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#889 (lane L9, items 46 and 51). Engine-level twins of the
// internal/conn tests: a chunked upload split across recvs must not promote
// its connection to the dispatch goroutine for life, a request pipelined
// behind it that names an Async route must still reach the goroutine, and a
// panicking OnExpectContinue must leave a log record.

var enginesNative889 = []struct {
	name string
	eng  celeris.EngineType
}{{"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}, {"adaptive", celeris.Adaptive}}

// start889 serves a sync POST /up and an Async() POST /aup that answer with
// the body length, a sync GET /ping and an Async() GET /slow, on one engine.
func start889(t *testing.T, cfg celeris.Config) (string, *celeris.Server) {
	t.Helper()
	retryUntil := time.Now().Add(30 * time.Second)
	for tries := 1; ; tries++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		cfg.Addr = addr
		s := celeris.New(cfg)
		up := func(c *celeris.Context) error { return c.String(http.StatusOK, "n=%d", len(c.Body())) }
		s.POST("/up", up)
		s.POST("/aup", up).Async()
		s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
		s.GET("/slow", func(c *celeris.Context) error { return c.String(http.StatusOK, "slow") }).Async()
		startDone := make(chan error, 1)
		go func() { startDone <- s.Start() }()
		err = waitReady761(addr, startDone)
		if err == nil {
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
			return addr, s
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.Shutdown(ctx)
		cancel()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(retryUntil) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		t.Fatalf("server did not start (try %d): %v", tries, err)
	}
}

func get889(t *testing.T, c net.Conn, br *bufio.Reader, path string) string {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: a\r\n\r\n", path); err != nil {
		t.Fatalf("write GET %s: %v", path, err)
	}
	status, body := read1029(t, br)
	if status != 200 {
		t.Fatalf("GET %s: status %d %q", path, status, body)
	}
	return body
}

// TestSplitChunkedUploadDoesNotPromote889 sends a chunked upload to a sync
// route in several segments with gaps, then many small requests on the same
// keep-alive connection. The connection ran no async route, so it must not
// have been handed to a dispatch goroutine: AsyncPromotedConns stays 0. On
// main the split body left the head in state.buffer and the promotion
// predicate took that for partial headers, for the life of the conn.
func TestSplitChunkedUploadDoesNotPromote889(t *testing.T) {
	for _, e := range enginesNative889 {
		t.Run(e.name, func(t *testing.T) {
			addr, srv := start889(t, celeris.Config{Engine: e.eng, Protocol: celeris.Auto})
			before := srv.EngineInfo().Metrics.AsyncPromotedConns
			c, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			br := bufio.NewReader(c)
			wire, _ := wire1029("/up", pat1029(60000), 7000, "", "", "")
			go write1029(c, equal1029(wire, 6, 5*time.Millisecond))
			if status, got := read1029(t, br); status != 200 || got != "n=60000" {
				t.Fatalf("upload: status %d body %q, want 200 n=60000", status, got)
			}
			for i := 0; i < 20; i++ {
				if got := get889(t, c, br, "/ping"); got != "ok" {
					t.Fatalf("ping %d: %q", i, got)
				}
			}
			if got := srv.EngineInfo().Metrics.AsyncPromotedConns - before; got != 0 {
				t.Fatalf("AsyncPromotedConns rose by %d: a split chunked upload to a sync route promoted its connection to the dispatch goroutine", got)
			}
		})
	}
}

// TestChunkedThenAsyncPipelined889 pipelines an Async route behind a chunked
// upload, in the same segment as the terminating chunk. The Async route must
// reach the dispatch goroutine (the conn is promoted, exactly once), the
// answers must come back in order, and the conn must serve later requests.
// On main the request behind a chunked body took the buffered parse path,
// which has no per-route async check: it ran on the event loop.
func TestChunkedThenAsyncPipelined889(t *testing.T) {
	for _, e := range enginesNative889 {
		for _, split := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/split=%v", e.name, split), func(t *testing.T) {
				addr, srv := start889(t, celeris.Config{Engine: e.eng, Protocol: celeris.Auto})
				before := srv.EngineInfo().Metrics.AsyncPromotedConns
				c, err := net.DialTimeout("tcp", addr, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = c.Close() }()
				_ = c.SetDeadline(time.Now().Add(30 * time.Second))
				br := bufio.NewReader(c)
				// One segment that fits a recv for sure, or a body that spans
				// several, whose last segment holds the end of the body, the
				// terminator and the pipelined Async request.
				n := 3000
				if split {
					n = 30000
				}
				wire, _ := wire1029("/up", pat1029(n), 1000, "", "", "")
				all := append(append([]byte(nil), wire...), "GET /slow HTTP/1.1\r\nHost: a\r\n\r\n"...)
				plan := []seg1029{{all, 0}}
				if split {
					plan = plan1029(all, []int{len(wire) / 3, len(wire) - 40}, 10*time.Millisecond)
				}
				go write1029(c, plan)
				if status, got := read1029(t, br); status != 200 || got != fmt.Sprintf("n=%d", n) {
					t.Fatalf("upload: status %d body %q, want 200 n=%d", status, got, n)
				}
				if status, got := read1029(t, br); status != 200 || got != "slow" {
					t.Fatalf("pipelined async: status %d body %q, want 200 slow", status, got)
				}
				for i := 0; i < 5; i++ {
					if got := get889(t, c, br, "/ping"); got != "ok" {
						t.Fatalf("ping %d: %q", i, got)
					}
				}
				if got := srv.EngineInfo().Metrics.AsyncPromotedConns - before; got != 1 {
					t.Fatalf("AsyncPromotedConns rose by %d, want 1: the Async route behind the chunked body did not reach the dispatch goroutine", got)
				}
			})
		}
	}
}

// keep889 keeps every record, cloned.
type keep889 struct {
	mu   *sync.Mutex
	recs *[]slog.Record
}

func (h keep889) Enabled(context.Context, slog.Level) bool { return true }
func (h keep889) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	*h.recs = append(*h.recs, r.Clone())
	h.mu.Unlock()
	return nil
}
func (h keep889) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h keep889) WithGroup(string) slog.Handler      { return h }

// TestExpectContinuePanicIsLogged889: an OnExpectContinue that panics is
// treated as a rejection (417, connection closed), and now says so in the log
// with the panic value and the stack. On main the recover dropped it.
func TestExpectContinuePanicIsLogged889(t *testing.T) {
	for _, e := range enginesNative889 {
		for _, chunked := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/chunked=%v", e.name, chunked), func(t *testing.T) {
				var mu sync.Mutex
				var recs []slog.Record
				addr, _ := start889(t, celeris.Config{
					Engine:   e.eng,
					Protocol: celeris.Auto,
					Logger:   slog.New(keep889{&mu, &recs}),
					OnExpectContinue: func(method, path string, headers [][2]string) bool {
						panic("boom-expect-889")
					},
				})
				c, err := net.DialTimeout("tcp", addr, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = c.Close() }()
				_ = c.SetDeadline(time.Now().Add(20 * time.Second))
				var b bytes.Buffer
				if chunked {
					b.WriteString("POST /up HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\nTransfer-Encoding: chunked\r\n\r\n")
				} else {
					b.WriteString("POST /up HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\nContent-Length: 5\r\n\r\n")
				}
				_, _ = c.Write(b.Bytes())
				resp, err := http.ReadResponse(bufio.NewReader(c), nil)
				if err != nil {
					t.Fatalf("read: %v", err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusExpectationFailed {
					t.Fatalf("status %d, want 417", resp.StatusCode)
				}
				deadline := time.Now().Add(5 * time.Second)
				for {
					mu.Lock()
					var found bool
					for _, r := range recs {
						var all strings.Builder
						all.WriteString(r.Message)
						r.Attrs(func(a slog.Attr) bool { all.WriteString(" " + a.Key + "=" + a.Value.String()); return true })
						if strings.Contains(all.String(), "boom-expect-889") && strings.Contains(all.String(), "stack=") {
							found = true
						}
					}
					mu.Unlock()
					if found {
						return
					}
					if time.Now().After(deadline) {
						t.Fatalf("no log record names the panic value and carries a stack (%d records)", len(recs))
					}
					time.Sleep(20 * time.Millisecond)
				}
			})
		}
	}
}
