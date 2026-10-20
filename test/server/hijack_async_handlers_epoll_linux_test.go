//go:build linux

package celeris_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestHijackWithAsyncHandlersOnEpoll is the user-facing face of the
// internal/engine/epoll TestInlineHijackOnAsyncLoop: with Config.AsyncHandlers, a
// route that inherits the default starts inline (celeris#356), so its
// handler runs on the event loop, and Hijack there released the connState
// that drainRead then dereferenced. The first hijack crashed the process.
//
// The test also asserts that /hj ran inline, which a hijacked conn that
// completed its session does not show by itself: no conn was promoted to a
// dispatch goroutine (EngineInfo's AsyncPromotedConns stays 0), so the
// hijacks took hijackConn's inline branch, the one that released the
// connState. Were /hj to start on a dispatch goroutine, the hijacks would
// pass through the other branch and this test would no longer cover the
// defect (celeris#774).
func TestHijackWithAsyncHandlersOnEpoll(t *testing.T) {
	var srv *celeris.Server // the server of the start that succeeded
	addr, stopServer := startC714DetachServer(t, func() *celeris.Server {
		srv = celeris.New(celeris.Config{Engine: celeris.Epoll, Workers: 2, AsyncHandlers: true})
		srv.GET("/hj", func(c *celeris.Context) error {
			conn, err := c.Hijack()
			if err != nil {
				return err
			}
			_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhj")
			return conn.Close()
		})
		srv.GET("/ok", func(c *celeris.Context) error { return c.String(200, "ok") })
		return srv
	})
	defer stopServer()

	get := func(path string) (string, error) {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			return "", err
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
			return "", err
		}
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			// celeris#936: say from the kernel's side where the request is.
			// Only a deadline gets the probes (about 30 s of fresh conns); an
			// immediate EOF or reset gets the socket's account alone.
			return "", fmt.Errorf("%w\n%s", err, c936Report(srv, addr, c, c936Cause(err, path, 5*time.Second), errors.Is(err, os.ErrDeadlineExceeded)))
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return string(b), err
	}
	for i := 0; i < 20; i++ {
		if got, err := get("/hj"); err != nil || got != "hj" {
			t.Fatalf("hijack %d: body %q, err %v", i, got, err)
		}
		if got, err := get("/ok"); err != nil || got != "ok" {
			t.Fatalf("request after hijack %d: body %q, err %v", i, got, err)
		}
	}
	// Read before the server stops, while the engine's counters are live.
	// (drainRead's second promotion site, a request split across two recvs,
	// would also count; get writes each request at once.)
	if n := srv.EngineInfo().Metrics.AsyncPromotedConns; n != 0 {
		t.Errorf("AsyncPromotedConns = %d after 20 hijacks, want 0: /hj did not run inline on the event loop", n)
	}
}
