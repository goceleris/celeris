//go:build linux

package celeris_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// Silent or dead clients with a stream open (celeris#889, L19). ReadTimeout and
// IdleTimeout skip an HTTP/2 conn that has a stream open so that a stream the
// SERVER is writing to (an SSE stream) outlives a client that sends no frame.
// A client that opened a stream and owes the server something (the rest of its
// request, the WINDOW_UPDATE a response is waiting for) and then sends nothing
// is a different case, and main reaps it at ReadTimeout. These cells hold the
// engines to that: ReadTimeout 1 s, IdleTimeout off, WriteTimeout 2 s, routes
// sync and async, a client that sends no frame after its request. (A response
// that waits for the window and is buffered in the stream is bounded by
// WriteTimeout now, and keeps the conn until its stream is reset: the conn is
// closed after that, still within the bound below.)
//
// A client that sends a frame of any kind at least once per ReadTimeout is not
// reaped by the conn clock (it restarts it): the per-stream deadlines bound its
// streams. The last test pins that for a response.

// silentServer889 is the server of these cells: /upload889 (POST, answers ok
// once its body is whole), /big889 (a 1 MiB or, with ?n=8388608, an 8 MiB
// body), /hold889 (an SSE-like stream), all on sync or async routes.
func silentServer889(t *testing.T, eng celeris.EngineType, syncRoutes bool) string {
	t.Helper()
	var ended atomic.Bool
	cfg := celeris.Config{Engine: eng, ReadTimeout: to889, IdleTimeout: -1, WriteTimeout: 2 * time.Second}
	return startServerConfig761(t, cfg, func(s *celeris.Server) {
		holdRoute889(s, 30*time.Second, &ended)
		r1 := s.POST("/upload889", func(c *celeris.Context) error { return c.String(200, "ok") })
		r2 := s.GET("/big889", func(c *celeris.Context) error {
			n := 1 << 20
			if c.Query("n") == "8388608" {
				n = 8 << 20
			}
			return c.Blob(200, "application/octet-stream", make([]byte, n))
		})
		if !syncRoutes {
			r1.Async()
			r2.Async()
		}
	})
}

func nativeEngines889() []struct {
	name string
	eng  celeris.EngineType
} {
	var out []struct {
		name string
		eng  celeris.EngineType
	}
	for _, e := range engines761 {
		if e.name != "std" {
			out = append(out, e)
		}
	}
	return out
}

func routeMode889(syncRoutes bool) string {
	if syncRoutes {
		return "sync"
	}
	return "async"
}

// silentOwes889 are the ways a client owes the server something and then goes
// silent: HEADERS of a POST without END_STREAM (the body it never sends), and a
// response bigger than the 64 KiB window the client leaves open (1 MiB, and
// 8 MiB: over the 4 MiB the conn buffers) of which it grants no more.
var silentOwes889 = []struct {
	name string
	req  func(c *h2raw889)
}{
	{"request-open", func(c *h2raw889) { c.request(1, "POST", "/upload889", false, true) }},
	{"response-1MiB-untaken", func(c *h2raw889) { c.request(1, "GET", "/big889?n=1048576", true, true) }},
	{"response-8MiB-untaken", func(c *h2raw889) { c.request(1, "GET", "/big889?n=8388608", true, true) }},
}

// pingCells889 are the cells of a client that pings and never takes a response
// (see the last test): the request-open cell was flipped by the per-stream request
// deadline (TestH2PingingClientRequestIsAnsweredAndReset889), the untaken response
// by the per-stream write deadline.
var pingCells889 = []struct {
	name string
	req  func(c *h2raw889)
}{
	{"response-1MiB-untaken", func(c *h2raw889) { c.request(1, "GET", "/big889?n=1048576", true, true) }},
}

// TestH2SilentClientOwingTheServerIsReaped889: a client that has opened a stream
// whose request is not whole, or whose response waits for its WINDOW_UPDATE,
// and sends nothing more, is closed by ReadTimeout on the native engines, on
// sync and async routes, as main closes it.
func TestH2SilentClientOwingTheServerIsReaped889(t *testing.T) {
	for _, e := range nativeEngines889() {
		for _, syncRoutes := range []bool{false, true} {
			for _, sc := range silentOwes889 {
				t.Run(e.name+"/"+routeMode889(syncRoutes)+"/"+sc.name, func(t *testing.T) {
					t.Parallel()
					addr := silentServer889(t, e.eng, syncRoutes)
					c := dialH2raw889(t, addr, h2dial889{})
					sc.req(c)
					o := c.watch(0, to889+reap889)
					t.Logf("H2OWES889 engine=%s routes=%s cell=%s: %s", e.name, routeMode889(syncRoutes), sc.name, o)
					if o.open {
						t.Fatalf("the connection was still open %v after the client's request: it owes the server (%s) and sent nothing, want it closed by ReadTimeout %v within %v",
							o.at.Round(time.Millisecond), sc.name, to889, to889+reap889)
					}
				})
			}
		}
	}
}

// TestH2PingingClientHoldsAnUnfinishedStream889 (name kept) was the pinned cell
// of a client that opens a stream, asks for a response it never takes, and sends
// one PING every 500 ms (ReadTimeout is 1 s): on main it was not closed and its
// stream was never reset on the native engines (measured on main f4ae3ad, all
// three engines), where std ends such a stream at WriteTimeout (RST_STREAM
// INTERNAL_ERROR, 2.00 s). The request side was flipped by the per-stream
// request deadline (ReadTimeout, 408 and RST_STREAM(NO_ERROR)); this is the
// response side, FLIPPED by the per-stream write deadline (L19, D2): the stream
// is reset with INTERNAL_ERROR at WriteTimeout (2 s), and the conn, whose client
// pings, stays and serves the next request.
func TestH2PingingClientHoldsAnUnfinishedStream889(t *testing.T) {
	for _, e := range nativeEngines889() {
		for _, sc := range pingCells889 {
			t.Run(e.name+"/"+sc.name, func(t *testing.T) {
				t.Parallel()
				addr := silentServer889(t, e.eng, false)
				c := dialH2raw889(t, addr, h2dial889{})
				sc.req(c)
				stop := c.pingEvery889(500 * time.Millisecond)
				defer stop()
				ev := c.await889(1, 6*time.Second)
				t.Logf("H2PINGOWES889 engine=%s cell=%s: %s", e.name, sc.name, ev)
				wantWriteReset889(t, ev, 1800*time.Millisecond, 3500*time.Millisecond)
				c.serves889(3)
			})
		}
	}
}
