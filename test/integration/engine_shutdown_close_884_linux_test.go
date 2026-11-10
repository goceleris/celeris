//go:build linux

package integration

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/epoll"
	"github.com/goceleris/celeris/internal/engine/iouring"
)

// TestEngineShutdownCallAloneSaysClose884: Engine.Shutdown on the native
// engines does nothing but record the budget (the drain runs when Listen's
// context is cancelled), and the contract of celeris#884 / #885 starts at the
// call: with Listen's context still live, every response written after it
// carries Connection: close and ends its conn, on a keep-alive conn opened
// before it and on one opened after. Before the call the same conn is
// keep-alive.
func TestEngineShutdownCallAloneSaysClose884(t *testing.T) {
	for _, tc := range []struct {
		name  string
		async bool
		mk    func(port int, async bool) (engine.Engine, error)
	}{
		{"epoll/inline", false, newEpoll884},
		{"epoll/async", true, newEpoll884},
		{"io_uring/inline", false, newIOUring884},
		{"io_uring/async", true, newIOUring884},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port := freePort(t)
			e := startEngineRetry(t, func() (engine.Engine, error) { return tc.mk(port, tc.async) }, func(err error) { t.Fatalf("new: %v", err) })
			addr := e.(addrGetter).Addr().String()

			dial := func() (net.Conn, *bufio.Reader) {
				c, err := net.Dial("tcp", addr)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = c.Close() })
				return c, bufio.NewReader(c)
			}
			get := func(c net.Conn, br *bufio.Reader, wantClose bool, what string) {
				t.Helper()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := io.WriteString(c, "GET /x HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
					t.Fatalf("%s: write: %v", what, err)
				}
				var head strings.Builder
				for {
					line, err := br.ReadString('\n')
					head.WriteString(line)
					if err != nil {
						t.Fatalf("%s: head: %v after %q", what, err, head.String())
					}
					if line == "\r\n" {
						break
					}
				}
				var lines []string
				for _, l := range strings.Split(head.String(), "\r\n") {
					if len(l) >= 11 && strings.EqualFold(l[:11], "connection:") {
						lines = append(lines, l)
					}
				}
				rd := bufio.NewReader(io.MultiReader(strings.NewReader(head.String()), br))
				resp, err := http.ReadResponse(rd, nil)
				if err != nil {
					t.Fatalf("%s: %v", what, err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				if wantClose {
					if len(lines) != 1 || !strings.EqualFold(strings.TrimSpace(lines[0][len("connection:"):]), "close") || !resp.Close {
						t.Errorf("%s: Connection lines %q, close=%v, want exactly one connection: close", what, lines, resp.Close)
					}
					if _, err := rd.ReadByte(); !errors.Is(err, io.EOF) {
						t.Errorf("%s: the conn read %v after the response, want EOF", what, err)
					}
				} else if len(lines) != 0 || resp.Close {
					t.Errorf("%s: Connection lines %q, close=%v before any shutdown", what, lines, resp.Close)
				}
			}

			c1, br1 := dial()
			get(c1, br1, false, "before Shutdown")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := e.Shutdown(ctx); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			get(c1, br1, true, "a conn opened before Shutdown, after it")
			c2, br2 := dial()
			get(c2, br2, true, "a conn opened after Shutdown")
		})
	}
}

func newEpoll884(port int, async bool) (engine.Engine, error) {
	cfg := defaultTestConfig(port, engine.HTTP1)
	cfg.AsyncHandlers = async
	return epoll.New(cfg, &echoHandler{})
}

func newIOUring884(port int, async bool) (engine.Engine, error) {
	cfg := defaultTestConfig(port, engine.HTTP1)
	cfg.AsyncHandlers = async
	return iouring.New(cfg, &echoHandler{})
}
