//go:build linux

package celeris_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestHijackThenDetachOnEveryEngine runs a handler that takes the connection
// with Context.Hijack and then calls Context.Detach (a streaming helper that
// detaches on its own, say) through celeris.Server on every engine.
//
// The connection is the caller's once hijacked, so Detach must neither panic
// nor touch it: on epoll it panicked with a nil pointer dereference (its
// OnDetach hook ran against the connection state the hijack had already
// released, and counted a detached connection that never ends), and on std it
// armed the WriteTimeout watchdog on the hijacked connection, which then
// failed the caller's late write (PR #1004 review, #889 item 35).
//
// The late write comes three WriteTimeouts after the hijack. The server must
// then still serve, and stop within the helper's wait (a leaked detached
// count would hold the shutdown).
func TestHijackThenDetachOnEveryEngine(t *testing.T) {
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
	// io_uring has no async arm: it refuses Hijack on the async dispatch path
	// (celeris#539, TestHijackRefusedOnAsyncDispatchPath), so the handler's
	// Hijack fails there before any Detach.
	if ok, p := c714ProbeIOUring(t); ok {
		arms = append(arms, arm{"io_uring", celeris.IOUring, false})
	} else if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
		t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids dropping the io_uring arms", p.IOUringTier, p.KernelVersion)
	} else {
		t.Logf("io_uring tier=%s kernel=%s: io_uring arms not run", p.IOUringTier, p.KernelVersion)
	}
	const writeTimeout = 150 * time.Millisecond
	for _, a := range arms {
		t.Run(a.name, func(t *testing.T) {
			var panicked atomic.Value
			lateErr := make(chan error, 8)
			addr, stop := startC714DetachServer(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: a.engine, Workers: 2, AsyncHandlers: a.async, WriteTimeout: writeTimeout})
				srv.GET("/hj", func(c *celeris.Context) error {
					conn, err := c.Hijack()
					if err != nil {
						return err
					}
					var done func()
					func() {
						defer func() {
							if r := recover(); r != nil {
								panicked.Store(fmt.Sprint(r) + "\n" + string(debug.Stack()))
							}
						}()
						done = c.Detach()
					}()
					_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n")
					go func() {
						defer func() { _ = conn.Close() }()
						time.Sleep(3 * writeTimeout)
						_, err := io.WriteString(conn, "late")
						lateErr <- err
						if done != nil {
							done()
						}
					}()
					return nil
				})
				srv.GET("/ok", func(c *celeris.Context) error { return c.String(200, "ok") })
				return srv
			})
			defer func() {
				stopped := make(chan struct{})
				go func() { stop(); close(stopped) }()
				select {
				case <-stopped:
				case <-time.After(20 * time.Second):
					t.Error("the server did not stop within 20 s after a Detach on a hijacked connection")
				}
			}()

			for i := 0; i < 3; i++ {
				c, err := net.DialTimeout("tcp", addr, 3*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				_, _ = io.WriteString(c, "GET /hj HTTP/1.1\r\nHost: x\r\n\r\n")
				all, rerr := io.ReadAll(c)
				_ = c.Close()
				var le error
				select {
				case le = <-lateErr:
				case <-time.After(5 * time.Second):
					t.Fatalf("iteration %d: the hijacked connection's late write never ran", i)
				}
				if p := panicked.Load(); p != nil {
					t.Fatalf("iteration %d: Detach after Hijack panicked: %v", i, p)
				}
				if le != nil || !strings.HasSuffix(string(all), "late") {
					t.Fatalf("iteration %d: hijacked connection cut: late write err %v, client read %q (err %v)", i, le, all, rerr)
				}
				c2, err := net.DialTimeout("tcp", addr, 3*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				_ = c2.SetDeadline(time.Now().Add(5 * time.Second))
				_, _ = io.WriteString(c2, "GET /ok HTTP/1.1\r\nHost: x\r\n\r\n")
				resp, err := http.ReadResponse(bufio.NewReader(c2), nil)
				if err != nil {
					t.Fatalf("iteration %d: request after the hijack: %v", i, err)
				}
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				_ = c2.Close()
				if string(b) != "ok" {
					t.Fatalf("iteration %d: /ok body %q", i, b)
				}
			}
		})
	}
}
