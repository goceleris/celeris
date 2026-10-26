//go:build linux

package celeris_test

import (
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestHijackAfterDetachIsRefusedOnEveryEngine936: a handler that detaches and
// then hijacks gets an error from Hijack on every engine, and the connection
// stays the engine's. Nothing refused it before (the residual of #1105): after
// Detach the epoll dispatch goroutine no longer holds the connection's lock
// across the handler, so the hijack closed a descriptor the loop's flushes and
// re-arms still use.
//
// The control is the same handler without the Detach, which hijacks.
func TestHijackAfterDetachIsRefusedOnEveryEngine936(t *testing.T) {
	type arm struct {
		name   string
		engine celeris.EngineType
		async  bool // mark the route Async: the handler runs on the dispatch goroutine
		inline bool // Config.AsyncHandlers, route Sync: inline on an async loop
	}
	arms := []arm{
		{"std", celeris.Std, false, false},
		{"epoll", celeris.Epoll, false, false},
		{"epoll-async-route", celeris.Epoll, true, false},
		{"epoll-asynchandlers", celeris.Epoll, false, true},
	}
	if ok, p := c714ProbeIOUring(t); ok {
		arms = append(arms, arm{"io_uring", celeris.IOUring, false, false})
	} else if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
		t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids dropping the io_uring arm", p.IOUringTier, p.KernelVersion)
	}
	type result struct {
		conn net.Conn
		err  error
	}
	for _, a := range arms {
		for _, detach := range []bool{true, false} {
			name := a.name + "/hijack-only"
			if detach {
				name = a.name + "/detach-then-hijack"
			}
			t.Run(name, func(t *testing.T) {
				got := make(chan result, 1)
				addr, stop := startC714DetachServer(t, func() *celeris.Server {
					srv := celeris.New(celeris.Config{Engine: a.engine, Workers: 2, AsyncHandlers: a.inline})
					rt := srv.GET("/hj", func(c *celeris.Context) error {
						var done func()
						if detach {
							done = c.Detach()
						}
						conn, err := c.Hijack()
						got <- result{conn, err}
						if done != nil {
							done()
						}
						return nil
					})
					if a.async {
						rt.Async()
					}
					if a.inline {
						rt.Sync()
					}
					return srv
				})
				defer func() {
					// On a build that let the hijack through, the loop's
					// detached count never ends and the stop never returns.
					stopped := make(chan struct{})
					go func() { stop(); close(stopped) }()
					select {
					case <-stopped:
					case <-time.After(10 * time.Second):
						t.Error("the server did not stop within 10 s")
					}
				}()

				cl, err := net.DialTimeout("tcp", addr, 3*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = cl.Close() }()
				_ = cl.SetDeadline(time.Now().Add(10 * time.Second))
				if _, err := io.WriteString(cl, "GET /hj HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				var r result
				select {
				case r = <-got:
				case <-time.After(10 * time.Second):
					t.Fatal("the handler never reached Hijack")
				}
				if r.conn != nil {
					_ = r.conn.Close()
				}
				if detach {
					if r.err == nil {
						t.Errorf("Hijack after Detach returned a connection (%v), want an error", r.conn != nil)
					}
				} else if r.err != nil {
					t.Errorf("Hijack without Detach: %v", r.err)
				}
			})
		}
	}
}
