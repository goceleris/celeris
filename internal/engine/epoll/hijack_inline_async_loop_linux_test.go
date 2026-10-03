//go:build linux

package epoll

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// A loop with async dispatch on (Config.AsyncHandlers, or any route marked
// Async) runs a connection's requests inline, in InlineMode, until one of
// them reaches an async route. A handler that runs inline there and calls
// Hijack takes hijackConn's inline branch, which returns the connState to the
// pool inside the Hijack call, h1State included (nil after release). drainRead
// then cleared InlineMode through cs.h1State, before it looked at
// ErrHijacked: a nil dereference on the loop goroutine, which took the whole
// process down on the first such request. On a loop whose released connState
// had already been taken by another worker's accept, the same line wrote
// that other connection's parser state.

// inlineHijackHandler: /hj hijacks and answers on the raw conn, /async is the
// async route (when asyncRoutes is set), anything else answers normally.
type inlineHijackHandler struct {
	asyncRoutes bool
	hijacked    *atomic.Int64
}

func (h inlineHijackHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	if s.Path != "/hj" {
		return s.ResponseWriter.WriteResponse(s, 200,
			[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
	}
	hj, ok := s.ResponseWriter.(stream.Hijacker)
	if !ok {
		return fmt.Errorf("response writer %T cannot hijack", s.ResponseWriter)
	}
	c, err := hj.Hijack(s)
	if err != nil {
		return err
	}
	h.hijacked.Add(1)
	_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhj")
	return c.Close()
}
func (h inlineHijackHandler) RouteAsync(_, path string) bool {
	return h.asyncRoutes && path == "/async"
}
func (h inlineHijackHandler) HasAsyncRoutes() bool { return h.asyncRoutes }

func TestInlineHijackOnAsyncLoop(t *testing.T) {
	for _, tc := range []struct {
		name        string
		asyncRoutes bool
	}{
		// Config.AsyncHandlers with no route marked: no resolver is wired,
		// and every request runs inline in InlineMode.
		{"async-loop-no-async-routes", false},
		// A server with one Async route: /hj is a sync route, run inline.
		{"async-loop-sync-route", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hijacked atomic.Int64
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("pick port: %v", err)
			}
			addr := ln.Addr().String()
			_ = ln.Close()
			e, err := New(resource.Config{
				Addr:          addr,
				Protocol:      engine.HTTP1,
				Resources:     resource.Resources{Workers: 2},
				AsyncHandlers: true,
			}, inlineHijackHandler{asyncRoutes: tc.asyncRoutes, hijacked: &hijacked})
			if err != nil {
				t.Fatalf("epoll engine: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() { errCh <- e.Listen(ctx) }()
			defer func() {
				cancel()
				select {
				case <-errCh:
				case <-time.After(5 * time.Second):
				}
			}()
			for dl := time.Now().Add(10 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
				time.Sleep(10 * time.Millisecond)
			}
			if e.Addr() == nil {
				t.Fatal("engine did not bind")
			}
			if !e.loops[0].async {
				t.Fatal("precondition: the loop is not in async mode")
			}

			get := func(path string) (string, error) {
				c, err := net.DialTimeout("tcp", addr, 3*time.Second)
				if err != nil {
					return "", err
				}
				defer func() { _ = c.Close() }()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", path); err != nil {
					return "", err
				}
				resp, err := http.ReadResponse(bufio.NewReader(c), nil)
				if err != nil {
					return "", err
				}
				b, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				return string(b), err
			}

			const n = 20
			for i := 0; i < n; i++ {
				if got, err := get("/hj"); err != nil || got != "hj" {
					t.Fatalf("hijack %d: body %q, err %v", i, got, err)
				}
				if got, err := get("/ok"); err != nil || got != "ok" {
					t.Fatalf("request after hijack %d: body %q, err %v", i, got, err)
				}
			}
			if got := hijacked.Load(); got != n {
				t.Errorf("hijacked %d connections, want %d", got, n)
			}
		})
	}
}
