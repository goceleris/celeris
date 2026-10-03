//go:build linux

package epoll_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/sse"
	"github.com/goceleris/celeris/middleware/websocket"
)

// startRDHUPServer boots an epoll server carrying one SSE and one WebSocket
// route and returns its address. Both routes are SYNC (no .Async() route
// anywhere), so the handler — and therefore Context.Detach — runs INLINE on
// the event-loop thread inside drainRead. That is what makes h1State.Detached
// already true when the EPOLLRDHUP branch of the same event batch runs.
func startRDHUPServer(t *testing.T, sseHandler func(*sse.Client), wsHandler func(*websocket.Conn)) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	srv := celeris.New(celeris.Config{Engine: celeris.Epoll})
	srv.GET("/events", sse.New(sse.Config{
		HeartbeatInterval: -1, // no heartbeat: the handler never writes again
		Handler:           sseHandler,
	}))
	srv.GET("/ws", websocket.New(websocket.Config{
		CheckOrigin: func(c *celeris.Context) bool { return true },
		Handler:     wsHandler,
	}))

	var startErr atomic.Pointer[error]
	startCtx, startCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if e := srv.StartWithListenerAndContext(startCtx, ln); e != nil {
			startErr.Store(&e)
		}
	}()
	time.Sleep(500 * time.Millisecond)
	if p := startErr.Load(); p != nil {
		t.Fatalf("server start: %v", *p)
	}
	t.Cleanup(func() {
		startCancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Log("server goroutine did not exit within 3s")
		}
	})
	return addr
}

// dialRequestThenFIN sends req and half-closes the write side so that the
// request bytes and the FIN reach the server in ONE segment — the server then
// sees EPOLLIN|EPOLLRDHUP in a single epoll event.
//
// TCP_CORK is what makes that deterministic: the corked write stays in the
// send queue until shutdown(SHUT_WR), which appends the FIN to the pending
// skb and pushes it. Without the cork the request goes out on its own and the
// server usually wakes on it before the FIN lands, taking the (already
// working) "FIN arrives on its own edge → drainRead reads EOF" path instead
// of the one under test.
//
// The read half stays open, so the peer never sends RST and the ONLY signal
// the server gets that the client is gone is the half-close.
func dialRequestThenFIN(t *testing.T, addr, req string) *net.TCPConn {
	t.Helper()

	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		_ = c.Close()
		t.Fatalf("dial returned %T, want *net.TCPConn", c)
	}
	t.Cleanup(func() { _ = tcp.Close() })

	raw, err := tcp.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var sockErr error
	if cerr := raw.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_CORK, 1)
	}); cerr != nil {
		t.Fatal(cerr)
	}
	if sockErr != nil {
		t.Skipf("TCP_CORK unavailable: %v", sockErr)
	}

	if _, err := tcp.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	if err := tcp.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	return tcp
}

// TestSSEDetachedPeerRDHUPNotifiesMiddleware is the regression pin for the
// celeris#498 follow-up to celeris#494: a detached SSE stream must learn that
// its peer went away even when the FIN rode the same readable edge as the
// request.
//
// Sequence (all of it required):
//
//   - The client's request bytes and its FIN arrive in one segment, so the
//     server's epoll batch carries EPOLLIN|EPOLLRDHUP for that fd at once.
//   - drainRead reads the request, the SSE middleware detaches inline, and
//     the short-read fast path (n < len(cs.buf)) returns WITHOUT the trailing
//     read that would have seen the EOF. With EPOLLET no further edge fires,
//     so the FIN stays unread for the life of the connection.
//   - The EPOLLRDHUP branch used to skip every Detached conn, so nothing was
//     left to report the half-close.
//
// The handler never writes after the headers (HeartbeatInterval: -1), so no
// EPIPE ever materialises either: pre-fix the SSE handler, its Context and
// its connState leak until the process exits.
func TestSSEDetachedPeerRDHUPNotifiesMiddleware(t *testing.T) {
	cancelled := make(chan struct{})
	entered := make(chan struct{})
	addr := startRDHUPServer(t,
		func(client *sse.Client) {
			close(entered)
			<-client.Context().Done()
			close(cancelled)
		},
		func(c *websocket.Conn) {
			for {
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
			}
		})

	dialRequestThenFIN(t, addr,
		"GET /events HTTP/1.1\r\nHost: "+addr+"\r\nAccept: text/event-stream\r\n\r\n")

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("SSE handler never ran — the request itself was not delivered")
	}

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("SSE stream never observed the peer half-close: " +
			"client.Context() still live 5s after the peer sent FIN")
	}
}

// TestWSDetachedPeerRDHUPStillOwnsLifecycle guards the other side of the
// EPOLLRDHUP change: a WebSocket peer that half-closes its write side is NOT
// gone — it is still reading, and its middleware genuinely owns the close
// lifecycle. Reporting the half-close as an error there would close the
// chanReader and kill a live conn.
//
// This is a NON-regression guard, not a fail-first test: it passes both
// before and after the fix. It exists so the SSE fix cannot be widened to all
// detached conns without turning red.
func TestWSDetachedPeerRDHUPStillOwnsLifecycle(t *testing.T) {
	wsErr := make(chan error, 1)
	addr := startRDHUPServer(t,
		func(client *sse.Client) { <-client.Context().Done() },
		func(c *websocket.Conn) {
			// Write AFTER the peer's FIN has had time to land, so the send
			// exercises a conn the RDHUP branch has already seen.
			time.Sleep(300 * time.Millisecond)
			wsErr <- c.WriteMessage(websocket.TextMessage, []byte("hi"))
			for {
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
			}
		})

	tcp := dialRequestThenFIN(t, addr,
		"GET /ws HTTP/1.1\r\nHost: "+addr+"\r\n"+
			"Connection: Upgrade\r\nUpgrade: websocket\r\n"+
			"Sec-WebSocket-Version: 13\r\n"+
			"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	_ = tcp.SetReadDeadline(time.Now().Add(5 * time.Second))

	br := bufio.NewReader(tcp)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read 101 status line: %v", err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 101") {
		t.Fatalf("expected 101 Switching Protocols, got %q", strings.TrimSpace(line))
	}
	for {
		h, herr := br.ReadString('\n')
		if herr != nil {
			t.Fatalf("read upgrade headers: %v", herr)
		}
		if strings.TrimSpace(h) == "" {
			break
		}
	}

	// The server-side write must succeed and its frame must reach us: both
	// prove the engine did not tear the conn down on the half-close.
	select {
	case werr := <-wsErr:
		if werr != nil {
			t.Fatalf("server-side WriteMessage after peer half-close: %v", werr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WebSocket handler never reached its write")
	}
	frame := make([]byte, 4) // FIN|text, len 2, "hi" — unmasked (server → client)
	if _, err := io.ReadFull(br, frame); err != nil {
		t.Fatalf("read server frame after peer half-close: %v", err)
	}
	if frame[0] != 0x81 || frame[1] != 0x02 || string(frame[2:]) != "hi" {
		t.Fatalf("unexpected frame % x", frame)
	}
}
