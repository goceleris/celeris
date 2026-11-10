//go:build linux

package epoll

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// start884 runs an epoll engine on a free loopback port and returns it with its
// address and the cancel of the context Listen runs under.
func start884(t *testing.T, async bool, h stream.Handler) (*Engine, string, context.CancelFunc) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, err := New(resource.Config{
		Addr:          addr,
		Protocol:      engine.HTTP1,
		AsyncHandlers: async,
		Resources:     resource.Resources{Workers: 2},
	}, h)
	if err != nil {
		t.Fatalf("epoll engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(30 * time.Second):
			t.Error("Listen did not return within 30s")
		}
	})
	for dl := time.Now().Add(15 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind")
	}
	return e, e.Addr().String(), cancel
}

// closeHandler884 answers "ok"; /gate first reports it has entered and waits to
// be released.
type closeHandler884 struct {
	entered chan struct{}
	release chan struct{}
}

func (h *closeHandler884) HandleStream(_ context.Context, s *stream.Stream) error {
	for _, hd := range s.GetHeaders() {
		if hd[0] == ":path" && hd[1] == "/gate" {
			h.entered <- struct{}{}
			<-h.release
		}
	}
	return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}}, []byte("ok"))
}

// RouteAsync and HasAsyncRoutes make /gate an Async route: its handler runs on
// the conn's dispatch goroutine, not on the loop's own thread. On a thread that
// the gated handler held, an engine with a single worker (the memlock limit of a
// CI runner allows one ring) could not turn the cancelled context into the flag.
func (h *closeHandler884) RouteAsync(_, path string) bool { return path == "/gate" }

func (h *closeHandler884) HasAsyncRoutes() bool { return true }

// close884 sends a GET and reports the Connection header lines of the response
// head and whether the conn is closed (EOF) once the response is read.
func close884(t *testing.T, c net.Conn, br *bufio.Reader, path string, send, probeEOF bool) (lines []string, eof bool) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if send {
		if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", path); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	var head strings.Builder
	for {
		line, err := br.ReadString('\n')
		head.WriteString(line)
		if err != nil {
			t.Fatalf("head: %v after %q", err, head.String())
		}
		if line == "\r\n" {
			break
		}
	}
	for _, l := range strings.Split(head.String(), "\r\n") {
		if len(l) >= 11 && strings.EqualFold(l[:11], "connection:") {
			lines = append(lines, l)
		}
	}
	if _, err := io.ReadFull(br, make([]byte, 2)); err != nil { // the body, "ok"
		t.Fatalf("body: %v", err)
	}
	if !probeEOF { // a keep-alive conn: nothing to wait for
		return lines, false
	}
	_, err := br.ReadByte()
	return lines, errors.Is(err, io.EOF)
}

func wantClose884(t *testing.T, what string, lines []string, eof bool) {
	t.Helper()
	if len(lines) != 1 || !strings.EqualFold(strings.TrimSpace(lines[0][len("connection:"):]), "close") {
		t.Errorf("%s: Connection lines %q, want exactly [connection: close]", what, lines)
	}
	if !eof {
		t.Errorf("%s: the conn is not closed after the response", what)
	}
}

// TestShutdownCallRaisesTheFlag884: Engine.Shutdown alone (Listen's context
// live: the engines drain only when it is cancelled) is the start of the
// shutdown for the contract of celeris#884 / #885: a response written after it
// carries Connection: close and ends its conn, on a conn that was keep-alive
// before it and on one opened after. This is what kills a Shutdown that does not
// raise the flag, which the server-level tests cannot see: there the cancel
// that follows raises it too.
func TestShutdownCallRaisesTheFlag884(t *testing.T) {
	for _, async := range []bool{false, true} {
		name := "inline"
		if async {
			name = "async"
		}
		t.Run(name, func(t *testing.T) {
			h := &closeHandler884{entered: make(chan struct{}, 1), release: make(chan struct{})}
			e, addr, _ := start884(t, async, h)
			dial := func() (net.Conn, *bufio.Reader) {
				c, err := net.DialTimeout("tcp", addr, 3*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = c.Close() })
				return c, bufio.NewReader(c)
			}
			c1, br1 := dial()
			if lines, eof := close884(t, c1, br1, "/x", true, false); len(lines) != 0 || eof {
				t.Fatalf("before Shutdown: Connection lines %q, closed %v", lines, eof)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := e.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			lines, eof := close884(t, c1, br1, "/x", true, true)
			wantClose884(t, "a conn opened before Shutdown", lines, eof)
			c2, br2 := dial()
			lines, eof = close884(t, c2, br2, "/x", true, true)
			wantClose884(t, "a conn opened after Shutdown", lines, eof)
		})
	}
}

// TestListenCancelRaisesTheFlag884: a Listen context cancelled with no
// Engine.Shutdown (StartWithContext cancels the context its Listen was derived
// from, which the loops see at the same time as the watcher that calls Shutdown)
// is the start of the shutdown all the same: the loop raises the flag itself. A
// request that is in its handler (an Async route, on its goroutine) when the
// context is cancelled is released once the flag is up, which is read from the
// engine, and its response carries Connection: close.
func TestListenCancelRaisesTheFlag884(t *testing.T) {
	h := &closeHandler884{entered: make(chan struct{}, 1), release: make(chan struct{})}
	e, addr, cancel := start884(t, true, h)
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	br := bufio.NewReader(c)
	if _, err := fmt.Fprint(c, "GET /gate HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler was not entered")
	}
	cancel()
	for dl := time.Now().Add(10 * time.Second); !e.shutdownBegun.Load(); {
		if time.Now().After(dl) {
			t.Fatal("the flag is not up 10s after Listen's context was cancelled (Engine.Shutdown was never called)")
		}
		time.Sleep(time.Millisecond)
	}
	close(h.release)
	lines, eof := close884(t, c, br, "/gate", false, true)
	wantClose884(t, "a request in its handler when Listen's context was cancelled", lines, eof)
}
