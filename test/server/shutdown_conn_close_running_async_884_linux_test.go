//go:build linux

package celeris_test

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestShutdownRunningAsyncHandlerAnswersCloseWithSlowDownloader884 pins the
// combination of celeris#884 (every HTTP/1 response after Shutdown carries
// Connection: close) and celeris#1165 / #1168 (a loop with no HTTP/2 conn
// closes its idle conns at the top of its shutdown, and its send drain waits
// for slow peers): an Async-route handler that is RUNNING when Shutdown begins,
// on loops with no HTTP/2 conn and a client downloading slowly (so the drain is
// held open and the handler's conn is not idle), answers with Connection: close
// and the conn is closed after it, on every engine. Before the flag was read
// where the response head is built, epoll answered it keep-alive: the gate's
// per-conn mark cannot reach a handler that holds its conn's lock.
func TestShutdownRunningAsyncHandlerAnswersCloseWithSlowDownloader884(t *testing.T) {
	noEnv := func(*testing.T) {}
	start := func(s string) func(*testing.T) {
		return func(t *testing.T) { t.Setenv("CELERIS_ADAPTIVE_START", s) }
	}
	for _, a := range []struct {
		name string
		eng  celeris.EngineType
		set  func(*testing.T)
		mode string
	}{
		{"std", celeris.Std, noEnv, "Shutdown"},
		{"epoll", celeris.Epoll, noEnv, "Shutdown"},
		{"io_uring", celeris.IOUring, noEnv, "Shutdown"},
		{"adaptive-epoll", celeris.Adaptive, start("epoll"), "cancel"},
		{"adaptive-iouring", celeris.Adaptive, start("iouring"), "cancel"},
	} {
		t.Run(a.name, func(t *testing.T) {
			a.set(t)
			const nGate, nSlow = 8, 16
			var entered atomic.Int64
			release := make(chan struct{})
			dl := strings.Repeat("d", 16<<20)
			srv := startH2PoolServer759(t, a.eng, 20*time.Second, func(s *celeris.Server) {
				s.GET("/dl", func(c *celeris.Context) error { return c.String(http.StatusOK, "%s", dl) })
				s.GET("/gate", func(c *celeris.Context) error {
					entered.Add(1)
					<-release
					return c.String(http.StatusOK, "ok")
				}).Async()
			})

			// Slow downloaders: responses queued for clients that read nothing,
			// on enough conns that every loop has some.
			var slow []net.Conn
			for range nSlow {
				c := dialSmallRcvbuf884(t, srv.addr)
				slow = append(slow, c)
				if _, err := io.WriteString(c, "GET /dl HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
			}
			// Async-route requests in their handlers.
			type gated struct {
				c  net.Conn
				br *bufio.Reader
			}
			var gs []gated
			for range nGate {
				c, err := net.Dial("tcp", srv.addr)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = c.Close() })
				if _, err := io.WriteString(c, "GET /gate HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				gs = append(gs, gated{c, bufio.NewReader(c)})
			}
			for dl := time.Now().Add(10 * time.Second); entered.Load() < nGate; {
				if time.Now().After(dl) {
					t.Fatalf("%d of %d /gate handlers entered", entered.Load(), nGate)
				}
				time.Sleep(5 * time.Millisecond)
			}

			srv.beginShutdown(a.mode, 20*time.Second)
			// No HTTP/2 conn to report the start: Engine.Shutdown is the first
			// thing Server.Shutdown does, and the loops are in their drain well
			// within this.
			time.Sleep(500 * time.Millisecond)
			close(release)

			var wg sync.WaitGroup
			for i, g := range gs {
				wg.Add(1)
				go func() {
					defer wg.Done()
					wantClosed884(t, "running Async handler "+string(rune('0'+i)), g.c, g.br, ccVariant884{name: "get"}, false)
				}()
			}
			wg.Wait()
			for _, c := range slow {
				_ = c.Close()
			}
			srv.waitStart(t, 25*time.Second)
		})
	}
}
