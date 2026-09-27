//go:build linux

package celeris_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/probe"
)

// TestDetachedContextKeepsRequestValues pins celeris#718, the Context.Detach
// twin of celeris#714.
//
// On epoll and io_uring every request string is a view of the engine's
// receive buffer, and the engine keeps receiving into that buffer after a
// handler detaches. Detach therefore clones what it knows refers to the
// buffer. It cloned the headers, method, path and raw query, but not the
// route params, the parsed query and cookie caches, the Host it serves from
// Stream.Authority, or the strings middleware store on the Context (request
// ID, client-IP/host/scheme overrides, SetString values). The handler below
// sets each of those the way the in-tree middleware does, detaches, and reads
// them back from its goroutine after the peer has sent more bytes (on which
// the native engines close the connection). Odd streams carry an
// X-Forwarded-Host, so Host is served from the SetHost override there and
// from Stream.Authority on even streams. Path, RawQuery and Header were
// already cloned and are the controls.
func TestDetachedContextKeepsRequestValues(t *testing.T) {
	type arm struct {
		name   string
		engine celeris.EngineType
		async  bool
	}
	arms := []arm{
		{"std", celeris.Std, false},
		{"epoll", celeris.Epoll, false},
		{"epoll-async", celeris.Epoll, true},
	}
	if p := probe.Probe(); p.IOUringTier >= celerisengine.High && p.ProvidedBuffers {
		arms = append(arms, arm{"io_uring", celeris.IOUring, false}, arm{"io_uring-async", celeris.IOUring, true})
	} else if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
		t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids dropping the io_uring arms", p.IOUringTier, p.KernelVersion)
	} else {
		t.Logf("io_uring tier=%s kernel=%s: io_uring arms not run", p.IOUringTier, p.KernelVersion)
	}
	fields := []string{"param", "query", "cookie", "host", "scheme", "requestid", "clientip", "setstring", "path", "rawquery", "header"}

	for _, a := range arms {
		t.Run(a.name, func(t *testing.T) {
			got := make(chan map[string]string, 1)
			// std does not end a detached stream when the peer sends bytes,
			// so its handler stops waiting sooner; it holds copies anyway.
			wait := 3 * time.Second
			if a.engine == celeris.Std {
				wait = 300 * time.Millisecond
			}
			srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async})
			srv.GET("/d/:id", func(c *celeris.Context) error {
				// What an ordinary middleware chain leaves on the Context
				// before a streaming handler detaches.
				_ = c.QueryParams()                             // a query validator
				_, _ = c.Cookie("sid")                          // session
				c.SetRequestID(c.Header("x-request-id"))        // requestid
				c.SetClientIP(c.Header("x-real-ip"))            // proxy
				c.SetScheme(c.Header("x-forwarded-proto"))      // proxy
				c.SetString("principal", c.Header("x-api-key")) // keyauth
				if h := c.Header("x-forwarded-host"); h != "" {
					c.SetHost(h) // proxy
				}

				closed := make(chan struct{})
				var once sync.Once
				stop := func() { once.Do(func() { close(closed) }) }
				c.SetWSErrorHandler(func(error) { stop() })
				c.SetWSDetachClose(stop)
				done := c.Detach()
				sw := c.StreamWriter()
				_ = sw.WriteHeader(200, [][2]string{{"content-type", "text/plain"}})
				_ = sw.Flush()
				finish := func() {
					defer done()
					select {
					case <-closed:
					case <-time.After(wait):
					}
					got <- detachedView(c)
					_ = sw.Close()
				}
				if c.EngineSupportsAsyncDetach() {
					go finish()
					return nil
				}
				finish()
				return nil
			})
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- srv.StartWithListenerAndContext(ctx, ln) }()
			defer func() { cancel(); <-done }()
			addr := waitDetachServer(t, srv)

			n := 20
			if a.engine == celeris.Std {
				n = 3 // copies on std; a few streams are enough for the control
			}
			wrong := map[string]int{}
			var samples []string
			for i := 0; i < n; i++ {
				want := expectedDetachedView(i)
				fwd := ""
				if want["fwdhost"] != "" {
					fwd = "X-Forwarded-Host: " + want["fwdhost"] + "\r\n"
				}
				req := fmt.Sprintf("GET /d/%s?q=%s HTTP/1.1\r\nHost: %s\r\n%sCookie: sid=%s\r\nX-Request-Id: %s\r\nX-Real-Ip: %s\r\nX-Forwarded-Proto: %s\r\nX-Api-Key: %s\r\nX-Id: %s\r\n\r\n",
					want["param"], want["query"], want["authority"], fwd, want["cookie"], want["requestid"], want["clientip"], want["scheme"], want["setstring"], want["header"])
				conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				if _, err := conn.Write([]byte(req)); err != nil {
					t.Fatal(err)
				}
				br := bufio.NewReader(conn)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						t.Fatalf("read response head: %v", err)
					}
					if line == "\r\n" {
						break
					}
				}
				// More bytes than the whole request: every request byte in
				// the receive buffer is overwritten.
				_, _ = conn.Write([]byte(strings.Repeat("Z", 4*len(req))))
				_ = conn.SetReadDeadline(time.Now().Add(wait))
				_, _ = io.Copy(io.Discard, br)
				_ = conn.Close()
				var view map[string]string
				select {
				case view = <-got:
				case <-time.After(10 * time.Second):
					t.Fatalf("conn %d: detached goroutine did not report", i)
				}
				for _, f := range fields {
					if view[f] != want[f] {
						wrong[f]++
						if len(samples) < 6 {
							samples = append(samples, fmt.Sprintf("%s: want %q got %q", f, want[f], view[f]))
						}
					}
				}
			}
			var tally []string
			bad := false
			for _, f := range fields {
				tally = append(tally, fmt.Sprintf("%s=%d", f, wrong[f]))
				bad = bad || wrong[f] > 0
			}
			t.Logf("C714DETACH arm=%s streams=%d wrong: %s", a.name, n, strings.Join(tally, " "))
			if bad {
				t.Errorf("a detached Context does not return the request's values once the peer has sent more bytes (wrong per field over %d streams: %s); samples %q",
					n, strings.Join(tally, " "), samples)
			}
		})
	}
}

// expectedDetachedView returns what the detached goroutine must read on
// stream i, plus the two request-only entries "authority" (the Host header)
// and "fwdhost" (the X-Forwarded-Host header, odd streams only).
func expectedDetachedView(i int) map[string]string {
	id := fmt.Sprintf("id%06d", i)
	v := map[string]string{
		"param":     id,
		"query":     "q" + id,
		"cookie":    "sid" + id,
		"authority": "h" + id + ".example",
		"host":      "h" + id + ".example",
		"scheme":    "https",
		"requestid": "rid" + id,
		"clientip":  fmt.Sprintf("10.0.%d.%d", i/250, i%250+1),
		"setstring": "key" + id,
		"path":      "/d/" + id,
		"rawquery":  "q=q" + id,
		"header":    id,
	}
	if i%2 == 1 {
		v["fwdhost"] = "fh" + id + ".example"
		v["host"] = v["fwdhost"]
	}
	return v
}

func detachedView(c *celeris.Context) map[string]string {
	cookie, _ := c.Cookie("sid")
	principal, _ := c.GetString("principal")
	return map[string]string{
		"param":     c.Param("id"),
		"query":     c.Query("q"),
		"cookie":    cookie,
		"host":      c.Host(),
		"scheme":    c.Scheme(),
		"requestid": c.RequestID(),
		"clientip":  c.ClientIP(),
		"setstring": principal,
		"path":      c.Path(),
		"rawquery":  c.RawQuery(),
		"header":    c.Header("x-id"),
	}
}

func waitDetachServer(t *testing.T, s *celeris.Server) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if a := s.Addr(); a != nil {
			if c, err := net.DialTimeout("tcp", a.String(), 100*time.Millisecond); err == nil {
				_ = c.Close()
				return a.String()
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server not ready within 30s")
	return ""
}
