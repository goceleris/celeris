//go:build linux

package epoll

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/mono"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#936 follow-up (maintainer decision U-15), the engine's side of
// PR #1111: a reply an earlier pipelined request left queued goes out before a
// later request's Hijack hands the descriptor over, or the hijack is refused
// with the conn untouched; and a detached conn is not hijacked at all.

// flush936Handler: /a answers "reply-A", /hj hijacks (the error goes to hj)
// and writes the marker on the raw conn, /dh detaches through the engine's
// hook and then hijacks, reporting the error. The routes in async run on the
// conn's dispatch goroutine.
type flush936Handler struct {
	async map[string]bool
	hj    chan error
}

const flush936Marker = "HIJACKED-BY-THE-HANDLER"

func (h *flush936Handler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	switch s.Path {
	case "/hj", "/dh":
		if s.Path == "/dh" && s.OnDetach != nil {
			s.OnDetach()
		}
		hj, ok := s.ResponseWriter.(stream.Hijacker)
		if !ok {
			return fmt.Errorf("response writer %T cannot hijack", s.ResponseWriter)
		}
		c, err := hj.Hijack(s)
		h.hj <- err
		if err != nil {
			if s.Path == "/dh" {
				return nil // detached: the engine ends the conn
			}
			return s.ResponseWriter.WriteResponse(s, 503,
				[][2]string{{"content-length", "7"}}, []byte("refused"))
		}
		_, _ = io.WriteString(c, flush936Marker)
		return c.Close()
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "7"}}, []byte("reply-A"))
}

func (h *flush936Handler) RouteAsync(_, path string) bool { return h.async[path] }
func (h *flush936Handler) HasAsyncRoutes() bool           { return len(h.async) > 0 }

func startFlush936Engine(t *testing.T, asyncHandlers bool, h stream.Handler) (e *Engine, addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr = ln.Addr().String()
	_ = ln.Close()
	e, err = New(resource.Config{
		Addr:          addr,
		Protocol:      engine.HTTP1,
		Resources:     resource.Resources{Workers: 2},
		AsyncHandlers: asyncHandlers,
	}, h)
	if err != nil {
		t.Fatalf("epoll engine: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(10 * time.Second):
			t.Error("the engine did not stop")
		}
	})
	for dl := time.Now().Add(10 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind")
	}
	return e, addr
}

// TestHijackFindsTheReplyQueuedAndSendsIt936 records, in the engine, that the
// first reply was still queued when the hijack came (so the test cannot pass
// because the reply happened to go out earlier), and then that the client reads
// it whole before the hijacker's bytes.
func TestHijackFindsTheReplyQueuedAndSendsIt936(t *testing.T) {
	for _, tc := range []struct {
		name  string
		async bool            // Config.AsyncHandlers
		route map[string]bool // routes marked Async
	}{
		{"sync", false, nil},
		{"asynchandlers-inline", true, nil},
		{"async-hijack-route", false, map[string]bool{"/hj": true}},
		{"async-all-routes", false, map[string]bool{"/a": true, "/hj": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var queued, passes atomic.Int64
			hook := func(_ *connState, pass, pending int) {
				if pass == 0 && pending > 0 {
					queued.Add(1)
				}
				if pass > 0 {
					passes.Add(1)
				}
			}
			testHookHijackFlush.Store(&hook)
			defer testHookHijackFlush.Store(nil)

			h := &flush936Handler{async: tc.route, hj: make(chan error, 2)}
			_, addr := startFlush936Engine(t, tc.async, h)
			c, err := net.DialTimeout("tcp", addr, 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.WriteString(c, "GET /a HTTP/1.1\r\nHost: x\r\n\r\nGET /hj HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(c)
			resp, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("the reply to /a never arrived: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(body) != "reply-A" {
				t.Fatalf("reply to /a = %q", body)
			}
			rest, _ := io.ReadAll(br)
			if err := <-h.hj; err != nil {
				t.Fatalf("Hijack: %v", err)
			}
			if string(rest) != flush936Marker {
				t.Fatalf("after the reply the hijacker's bytes were %q, want %q", rest, flush936Marker)
			}
			if queued.Load() != 1 {
				t.Errorf("the engine found the first reply queued at the hijack %d times, want 1: the test did not exercise the flush", queued.Load())
			}
			if passes.Load() < 1 {
				t.Errorf("no flush pass ran")
			}
		})
	}
}

// TestHijackOfADetachedConnIsRefused936: the handler detaches through the
// engine's own hook and then asks the engine to hijack, the way a caller of the
// internal Hijacker could. The conn is the detaching middleware's; the engine
// refuses, whatever is or is not queued.
func TestHijackOfADetachedConnIsRefused936(t *testing.T) {
	for _, tc := range []struct {
		name  string
		async bool
		route map[string]bool
	}{
		{"sync", false, nil},
		{"asynchandlers-inline", true, nil},
		{"async-route", false, map[string]bool{"/dh": true}},
	} {
		for _, queuedFirst := range []bool{false, true} {
			name := tc.name
			req := "GET /dh HTTP/1.1\r\nHost: x\r\n\r\n"
			if queuedFirst {
				name += "/reply-queued"
				req = "GET /a HTTP/1.1\r\nHost: x\r\n\r\n" + req
			}
			t.Run(name, func(t *testing.T) {
				h := &flush936Handler{async: tc.route, hj: make(chan error, 2)}
				_, addr := startFlush936Engine(t, tc.async, h)
				c, err := net.DialTimeout("tcp", addr, 3*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = c.Close() }()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				if _, err := io.WriteString(c, req); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-h.hj:
					if err == nil {
						t.Fatal("the engine hijacked a detached conn")
					}
					t.Logf("refused: %v", err)
				case <-time.After(10 * time.Second):
					t.Fatal("the handler never ran")
				}
			})
		}
	}
}

// pair936 is a connected socketpair whose first end is nonblocking and has a
// small send buffer, and a connState on it, as flushBeforeHijack sees one.
func pair936(t *testing.T) (cs *connState, peer int) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Skipf("socketpair: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(fds[0]); _ = unix.Close(fds[1]) })
	_ = unix.SetsockoptInt(fds[0], unix.SOL_SOCKET, unix.SO_SNDBUF, 4096)
	return &connState{fd: fds[0]}, fds[1]
}

// drain936 reads everything the peer has, without blocking.
func drain936(peer int) []byte {
	var out []byte
	buf := make([]byte, 64<<10)
	for {
		n, err := unix.Read(peer, buf)
		if err != nil || n <= 0 {
			return out
		}
		out = append(out, buf[:n]...)
	}
}

// TestFlushBeforeHijackSucceedsAfterSeveralPasses936: the socket takes part of
// the queue, stops, and the peer reads between two passes: the second pass
// takes the rest and the hijack goes on. Reply bytes arrive in order, once.
func TestFlushBeforeHijackSucceedsAfterSeveralPasses936(t *testing.T) {
	for _, onLoop := range []bool{true, false} {
		t.Run(fmt.Sprintf("onLoopThread=%v", onLoop), func(t *testing.T) {
			cs, peer := pair936(t)
			reply := bytes.Repeat([]byte("0123456789abcdef"), 1<<10) // 16 KiB, over what 4 KiB buffers take
			cs.writeBuf = append(cs.writeBuf, reply...)
			cs.pendingBytes = len(reply)
			var got []byte
			var mu sync.Mutex
			var passes []int
			hook := func(c *connState, pass, pending int) {
				if c != cs || pass == 0 {
					return
				}
				mu.Lock()
				passes = append(passes, pending)
				got = append(got, drain936(peer)...) // the peer reads between the passes
				mu.Unlock()
			}
			testHookHijackFlush.Store(&hook)
			defer testHookHijackFlush.Store(nil)

			l := &Loop{bytesWritten: new(atomic.Uint64)}
			if err := l.flushBeforeHijack(cs, onLoop); err != nil {
				t.Fatalf("flushBeforeHijack: %v (pending after each pass %v)", err, passes)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(passes) < 2 {
				t.Fatalf("the flush took %d passes, want at least 2 (the socket's buffer is smaller than the reply)", len(passes))
			}
			got = append(got, drain936(peer)...)
			if !bytes.Equal(got, reply) {
				t.Fatalf("the peer read %d bytes, want the %d of the reply, in order", len(got), len(reply))
			}
			if csWritePending(cs) || cs.pendingBytes != 0 {
				t.Errorf("queue not empty after a successful flush: pending=%v pendingBytes=%d", csWritePending(cs), cs.pendingBytes)
			}
		})
	}
}

// TestFlushBeforeHijackWriteErrorClosesTheConn936: the peer is gone, so the
// write fails. The error is returned (the hijack is refused), the conn is
// marked refused so that it is closed, and nothing is left queued to go out
// later to a descriptor that may have been reissued.
func TestFlushBeforeHijackWriteErrorClosesTheConn936(t *testing.T) {
	cs, peer := pair936(t)
	if err := unix.Close(peer); err != nil {
		t.Fatal(err)
	}
	cs.writeBuf = append(cs.writeBuf, "reply"...)
	cs.pendingBytes = 5
	l := &Loop{bytesWritten: new(atomic.Uint64)}
	err := l.flushBeforeHijack(cs, true)
	if err == nil {
		t.Fatal("flushBeforeHijack succeeded with the peer gone")
	}
	if !cs.writeRefused {
		t.Error("writeRefused not set: the conn would be left open with its reply lost")
	}
	if csWritePending(cs) {
		t.Error("the failed flush left bytes queued")
	}
}

// TestFlushBeforeHijackKeepsTheWriteClock936: the inline branch with a write
// clock running (a reply parked by an earlier recv, writeSince != 0). A flush
// that empties the queue ends the clock; one that only moves it refreshes it
// (the sweep must not cut a peer that is reading).
func TestFlushBeforeHijackKeepsTheWriteClock936(t *testing.T) {
	t.Run("emptied", func(t *testing.T) {
		cs, peer := pair936(t)
		cs.writeBuf = append(cs.writeBuf, "small reply"...)
		cs.pendingBytes = len(cs.writeBuf)
		old := (mono.Now() + int64(-time.Hour))
		cs.writeSince, cs.lastActivity = old, old
		l := &Loop{bytesWritten: new(atomic.Uint64)}
		if err := l.flushBeforeHijack(cs, true); err != nil {
			t.Fatal(err)
		}
		if cs.writeSince != 0 {
			t.Errorf("writeSince = %d after the queue emptied, want 0", cs.writeSince)
		}
		if cs.lastActivity == old {
			t.Error("lastActivity not folded in by endWrite")
		}
		if got := drain936(peer); string(got) != "small reply" {
			t.Errorf("peer read %q", got)
		}
	})
	t.Run("progress", func(t *testing.T) {
		cs, peer := pair936(t)
		reply := bytes.Repeat([]byte("x"), 64<<10) // far more than 4 KiB buffers: refused after progress
		cs.writeBuf = append(cs.writeBuf, reply...)
		cs.pendingBytes = len(reply)
		old := (mono.Now() + int64(-time.Hour))
		cs.writeSince = old
		l := &Loop{bytesWritten: new(atomic.Uint64)}
		err := l.flushBeforeHijack(cs, true)
		if err == nil || !strings.Contains(err.Error(), "cannot hijack") {
			t.Fatalf("flushBeforeHijack = %v, want a refusal", err)
		}
		sent := len(drain936(peer))
		if sent == 0 || sent >= len(reply) {
			t.Fatalf("the socket took %d of %d bytes: the test needs a partial flush", sent, len(reply))
		}
		if cs.writeSince == old {
			t.Errorf("writeSince not refreshed after the flush moved %d bytes: the sweep would cut a reading peer", sent)
		}
		if cs.pendingBytes != len(reply)-sent {
			t.Errorf("pendingBytes = %d, want %d", cs.pendingBytes, len(reply)-sent)
		}
	})
}
