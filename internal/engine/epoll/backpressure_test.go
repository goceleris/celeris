//go:build linux

package epoll

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// bigResponseHandler replies with a fixed-size body, no keep-alive
// management. For the backpressure test we need the server to have a
// pile of bytes it wants to push but can't flush fully.
type bigResponseHandler struct {
	bodySize int
}

func (h *bigResponseHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	body := make([]byte, h.bodySize)
	for i := range body {
		body[i] = 'X'
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "application/octet-stream"}},
		body)
}

// TestWriteBufBackpressureClosesSlowConsumer is a best-effort
// end-to-end assertion of the timeout close path: a slow consumer must
// not hold server-side buffering for longer than the timeouts allow.
// Since celeris#761 a single response larger than maxPendingBytes is
// staged whole rather than closed at once (the cap is held per request,
// not per write), so it is the timeout sweep, here ReadTimeout, that
// closes a consumer that never reads it. The assertion is the server's:
// its conn count goes back to zero. What the client sees after the close is
// the kernel sending the send queue it still holds, into a 4 KiB window
// that the zero-window probes' backoff opens seconds apart, so a client
// deadline measured that backoff, not the engine.
//
// The test is marked t.Skip by default because reliably triggering
// the backpressure path on loopback TCP is hard — Linux's
// tcp_rmem/tcp_wmem auto-tuning plus loopback zero-copy make the
// window between "kernel absorbed the response" and "writeBuf
// exceeds 4 MiB cap" narrow and scheduler-dependent on CI runners.
// SetReadBuffer(4096) on the client is a hint the kernel often
// ignores in favor of its auto-tuned value, which makes the slow
// consumer not slow at all in practice.
//
// The defensive code paths are exercised in production (WebSocket
// tests hit the 64 MiB detached cap through Autobahn 9.1.6) and by
// manual inspection. Leaving this as a skipped smoke test so the
// shape is documented for anyone wanting to retry it with tuned
// sysctl settings.
//
// Run with: GOTEST_BACKPRESSURE=1 go test -run TestWriteBufBackpressure ./internal/engine/epoll/
func TestWriteBufBackpressureClosesSlowConsumer(t *testing.T) {
	if os.Getenv("GOTEST_BACKPRESSURE") != "1" {
		t.Skip("set GOTEST_BACKPRESSURE=1 to run; loopback TCP auto-tuning makes this non-deterministic on CI")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := resource.Config{
		Addr:         addr,
		Protocol:     engine.HTTP1,
		WriteTimeout: 200 * time.Millisecond,
		ReadTimeout:  200 * time.Millisecond,
		Resources: resource.Resources{
			Workers: 2,
		},
	}
	// 50 MiB body — an order of magnitude larger than typical kernel
	// send/recv buffers so server-side buffering is guaranteed to
	// accumulate past maxPendingBytes (4 MiB) even after the kernel
	// absorbs its share.
	e, err := New(cfg, &bigResponseHandler{bodySize: 50 << 20})
	if err != nil {
		t.Skipf("epoll engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	t.Cleanup(func() { cancel(); <-errCh })

	// Wait for bind.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && e.Addr() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind")
	}

	// Open the conn, shrink the recv buffer so the kernel can't
	// absorb the whole response — forces server-side queueing.
	tcp, err := net.DialTimeout("tcp", e.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = tcp.Close() }()
	if tcpConn, ok := tcp.(*net.TCPConn); ok {
		_ = tcpConn.SetReadBuffer(4096) // 4 KiB — kernel will cap above this but way below 5 MiB
	}

	// Send GET. Deliberately DO NOT read the response — we want the
	// server's writeBuf to fill and the timeout sweep to fire.
	// lastActivity on the server does not advance after the GET (no
	// further client→server data).
	req := "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"
	if _, err := fmt.Fprint(tcp, req); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Hold — don't read at all. The inline flush after the handler
	// runs attempts unix.Write of the full 50 MiB body; kernel
	// absorbs at most a few hundred KiB before EAGAIN. The rest waits
	// on EPOLLOUT until checkTimeouts converts the stalled conn into
	// closeConn.
	time.Sleep(500 * time.Millisecond)

	// Now the server should have closed the conn (the sweep runs at
	// least every few hundred ms under these timeouts).
	for end := time.Now().Add(3 * time.Second); e.Metrics().ActiveConnections != 0; {
		if time.Now().After(end) {
			t.Fatalf("the server still holds %d conn(s) 3.5s after the request; the timeout close did not fire", e.Metrics().ActiveConnections)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
