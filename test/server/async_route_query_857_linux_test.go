//go:build linux

package celeris_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris"
)

// celeris#857. The H1 engines ask the router whether a request's route is
// Async with the request target as it came off the wire, query string
// included, and the router is keyed by path alone: an Async route requested
// as /block?keys=10 was never promoted to the dispatch goroutine, so its
// blocking handler ran on the event loop, one request per loop at a time.
// The rig below serves a handler that blocks until released, on a server of
// two loops, and sends it n requests at once on n connections (H1) or n
// streams of one connection (h2c by prior knowledge, on a server that also
// answers the readiness probe over H1). With the route promoted, all n are in the
// handler together; on the event loop only as many as there are loops (two).

const (
	conc857h        = 8
	inflightWait857 = 5 * time.Second
)

type block857 struct {
	inflight, max atomic.Int64
	release       chan struct{}
	once          sync.Once
}

func (b *block857) open() { b.once.Do(func() { close(b.release) }) }

func (b *block857) handler(c *celeris.Context) error {
	n := b.inflight.Add(1)
	defer b.inflight.Add(-1)
	for {
		m := b.max.Load()
		if n <= m || b.max.CompareAndSwap(m, n) {
			break
		}
	}
	select {
	case <-b.release:
	case <-time.After(30 * time.Second):
	}
	return c.String(http.StatusOK, "ok")
}

// waitInflight857 waits until n requests are inside the handler, or the
// window ends, and returns the most that were there at once.
func (b *block857) waitInflight(n int64) int64 {
	for deadline := time.Now().Add(inflightWait857); time.Now().Before(deadline) && b.max.Load() < n; {
		time.Sleep(5 * time.Millisecond)
	}
	return b.max.Load()
}

func startBlock857(t *testing.T, a arm848, proto celeris.Protocol) (string, *block857) {
	t.Helper()
	need848(t, a.eng)
	if a.start != "" {
		t.Setenv("CELERIS_ADAPTIVE_START", a.start)
	}
	b := &block857{release: make(chan struct{})}
	_, addr := startPickedPing1104(t, celeris.Config{Engine: a.eng, Protocol: proto, Workers: 2}, func(s *celeris.Server) {
		ping1104(s)
		s.GET("/block", b.handler).Async()
	})
	t.Cleanup(b.open) // runs before the server's shutdown: cleanups run last in, first out
	return addr, b
}

func TestAsyncRouteWithQueryRunsOnDispatchGoroutineH1857(t *testing.T) {
	for _, tc := range []struct{ name, target string }{
		{"noquery", "/block"},
		{"query", "/block?keys=10"},
		{"emptyquery", "/block?"},
	} {
		target := tc.target
		t.Run(tc.name, func(t *testing.T) {
			for _, a := range arms848() {
				t.Run(a.name, func(t *testing.T) {
					addr, b := startBlock857(t, a, celeris.HTTP1)
					var wg sync.WaitGroup
					for i := 0; i < conc857h; i++ {
						c, err := net.DialTimeout("tcp", addr, 5*time.Second)
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { _ = c.Close() })
						wg.Add(1)
						go func() {
							defer wg.Done()
							_ = c.SetDeadline(time.Now().Add(45 * time.Second))
							if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: a\r\n\r\n", target); err != nil {
								t.Errorf("write: %v", err)
								return
							}
							resp, err := http.ReadResponse(bufio.NewReader(c), nil)
							if err != nil {
								t.Errorf("read: %v", err)
								return
							}
							_, _ = io.Copy(io.Discard, resp.Body)
							_ = resp.Body.Close()
							if resp.StatusCode != http.StatusOK {
								t.Errorf("status %d", resp.StatusCode)
							}
						}()
					}
					got := b.waitInflight(conc857h)
					b.open()
					wg.Wait()
					if got != conc857h {
						t.Fatalf("GET %s: at most %d of %d concurrent requests were in the Async handler at once: the route was not promoted to the dispatch goroutine, so the handler ran on the event loop", target, got, conc857h)
					}
				})
			}
		})
	}
}

// A connection promoted by an Async route requested with a query string stays
// promoted: the revert check of the io_uring engine (canRevertToInline) asks the
// router about the promoting route again at every park, with the path it
// recorded, and a path that still carried its query came back "not async", so
// the conn was handed back to the loop and promoted again by the next request.
func TestAsyncRouteWithQueryKeepsConnPromotedOnce857(t *testing.T) {
	for _, a := range arms848() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			if a.start != "" {
				t.Setenv("CELERIS_ADAPTIVE_START", a.start)
			}
			s, addr := startPickedPing1104(t, celeris.Config{Engine: a.eng, Protocol: celeris.HTTP1, Workers: 2}, func(s *celeris.Server) {
				ping1104(s)
				s.GET("/q", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") }).Async()
			})
			before := s.EngineInfo().Metrics.AsyncPromotedConns
			c, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			br := bufio.NewReader(c)
			for i := 0; i < 10; i++ {
				if got := get889(t, c, br, fmt.Sprintf("/q?i=%d", i)); got != "ok" {
					t.Fatalf("request %d: %q", i, got)
				}
				time.Sleep(2 * time.Millisecond) // the dispatch goroutine parks between requests
			}
			if got := s.EngineInfo().Metrics.AsyncPromotedConns - before; got != 1 {
				t.Fatalf("AsyncPromotedConns rose by %d over 10 requests of one connection to an Async route with a query string, want 1", got)
			}
		})
	}
}

// The h2 processor cuts the query off :path itself before asking the router,
// so h2 never had the defect; this keeps it that way.
func TestAsyncRouteWithQueryRunsOffTheLoopH2857(t *testing.T) {
	for _, a := range arms848() {
		t.Run(a.name, func(t *testing.T) {
			addr, b := startBlock857(t, a, celeris.Auto)
			c := dialH2raw889(t, addr, h2dial889{})
			for i := 0; i < conc857h; i++ {
				c.request(uint32(2*i+1), "GET", "/block?keys=10", true, true)
			}
			got := b.waitInflight(conc857h)
			b.open()
			ended := 0
			_ = c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			for ended < conc857h {
				f, err := c.fr.ReadFrame()
				if err != nil {
					t.Fatalf("after %d of %d streams ended: %v", ended, conc857h, err)
				}
				switch f := f.(type) {
				case *http2.DataFrame:
					if f.StreamEnded() {
						ended++
					}
				case *http2.HeadersFrame:
					if f.StreamEnded() {
						ended++
					}
				}
			}
			if got != conc857h {
				t.Fatalf("at most %d of %d concurrent h2 streams were in the Async handler at once", got, conc857h)
			}
		})
	}
}
