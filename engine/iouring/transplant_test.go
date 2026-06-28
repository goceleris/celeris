//go:build linux

package iouring

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// transplantTestHandler returns a tiny canned 200 — the test exercises the adopt
// path, not handler logic.
type transplantTestHandler struct{}

func (transplantTestHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}},
		[]byte("ok"))
}

// TestAdoptConnServesTransplantedFD proves the #383 io_uring adopt path: a real,
// already-connected socket the engine NEVER accepted is handed to AdoptConn and
// then served as a normal HTTP/1 keep-alive connection by io_uring (two requests,
// proving the conn is fully owned, not a one-shot). It also checks the transplant
// counter and the active-connection gauge — the metric that read 0 post-switch in
// the hollow-promotion bug (#396).
func TestAdoptConnServesTransplantedFD(t *testing.T) {
	// Start an io_uring engine.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := resource.Config{
		Addr:      addr,
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
	}
	e, err := New(cfg, transplantTestHandler{})
	if err != nil {
		t.Skipf("iouring engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	defer func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
		}
	}()

	// Wait until the engine is actually listening.
	listening := false
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		if c, derr := net.DialTimeout("tcp", addr, 200*time.Millisecond); derr == nil {
			_ = c.Close()
			listening = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !listening {
		t.Skipf("engine did not start listening on %s (constrained runner)", addr)
	}

	// Create a TCP connection the engine has NEVER seen, via our own listener.
	extLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ext listen: %v", err)
	}
	defer func() { _ = extLn.Close() }()
	clientConn, err := net.Dial("tcp", extLn.Addr().String())
	if err != nil {
		t.Fatalf("dial ext: %v", err)
	}
	defer func() { _ = clientConn.Close() }()
	srvConn, err := extLn.Accept()
	if err != nil {
		t.Fatalf("accept ext: %v", err)
	}

	// Extract a clean dup'd fd from the server side, independent of Go's poller,
	// then drop the Go-managed conn. The dup stays open and connected — exactly
	// the live fd the epoll engine would hand off after EPOLL_CTL_DEL.
	sc, err := srvConn.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatalf("syscallconn: %v", err)
	}
	var rawFD, dupErr = -1, error(nil)
	if cerr := sc.Control(func(fd uintptr) { rawFD, dupErr = unix.Dup(int(fd)) }); cerr != nil {
		t.Fatalf("control: %v", cerr)
	}
	if dupErr != nil {
		t.Fatalf("dup: %v", dupErr)
	}
	_ = srvConn.Close()
	if err := unix.SetNonblock(rawFD, true); err != nil {
		t.Fatalf("setnonblock: %v", err)
	}

	// Hand the live fd to io_uring.
	if err := e.AdoptConn(rawFD, engine.Carryover{RemoteAddr: "127.0.0.1:0"}); err != nil {
		t.Fatalf("AdoptConn: %v", err)
	}

	// Two keep-alive requests on the adopted conn must both be served by io_uring.
	br := bufio.NewReader(clientConn)
	for i := 0; i < 2; i++ {
		if _, werr := clientConn.Write([]byte("GET /simple HTTP/1.1\r\nHost: x\r\n\r\n")); werr != nil {
			t.Fatalf("write req %d: %v", i, werr)
		}
		_ = clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		resp, rerr := http.ReadResponse(br, nil)
		if rerr != nil {
			t.Fatalf("read resp %d: %v", i, rerr)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("req %d: status %d, want 200", i, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}

	// The engine must report exactly one transplanted, active connection.
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && e.TransplantCount() == 0; {
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.TransplantCount(); got != 1 {
		t.Fatalf("TransplantCount = %d, want 1", got)
	}
	if got := e.Metrics().ActiveConnections; got != 1 {
		t.Fatalf("ActiveConnections = %d, want 1", got)
	}
}

// TestAdoptConnReplaysBufferedRequest covers the #383 pipelined re-injection
// path: a conn is adopted with Carryover.Buffered holding a request the source
// engine had already read off the socket. io_uring must replay it through the
// fresh parser and respond WITHOUT the client sending anything new, then keep
// serving the conn normally.
func TestAdoptConnReplaysBufferedRequest(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	e, err := New(resource.Config{
		Addr: addr, Protocol: engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
	}, transplantTestHandler{})
	if err != nil {
		t.Skipf("iouring engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	defer func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
		}
	}()

	listening := false
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		if c, derr := net.DialTimeout("tcp", addr, 200*time.Millisecond); derr == nil {
			_ = c.Close()
			listening = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !listening {
		t.Skipf("engine did not start listening on %s (constrained runner)", addr)
	}

	extLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ext listen: %v", err)
	}
	defer func() { _ = extLn.Close() }()
	clientConn, err := net.Dial("tcp", extLn.Addr().String())
	if err != nil {
		t.Fatalf("dial ext: %v", err)
	}
	defer func() { _ = clientConn.Close() }()
	srvConn, err := extLn.Accept()
	if err != nil {
		t.Fatalf("accept ext: %v", err)
	}
	sc, err := srvConn.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatalf("syscallconn: %v", err)
	}
	var rawFD, dupErr = -1, error(nil)
	if cerr := sc.Control(func(fd uintptr) { rawFD, dupErr = unix.Dup(int(fd)) }); cerr != nil {
		t.Fatalf("control: %v", cerr)
	}
	if dupErr != nil {
		t.Fatalf("dup: %v", dupErr)
	}
	_ = srvConn.Close()
	if err := unix.SetNonblock(rawFD, true); err != nil {
		t.Fatalf("setnonblock: %v", err)
	}

	// Adopt with a pre-read pipelined request in the carry-over.
	carry := engine.Carryover{
		RemoteAddr: "127.0.0.1:0",
		Buffered:   []byte("GET /simple HTTP/1.1\r\nHost: x\r\n\r\n"),
	}
	if err := e.AdoptConn(rawFD, carry); err != nil {
		t.Fatalf("AdoptConn: %v", err)
	}

	br := bufio.NewReader(clientConn)
	// First response must arrive from the REPLAYED buffered request — no client write.
	_ = clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read replayed resp: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("replayed request: status %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// The conn must keep serving normally afterwards.
	if _, werr := clientConn.Write([]byte("GET /simple HTTP/1.1\r\nHost: x\r\n\r\n")); werr != nil {
		t.Fatalf("write follow-up: %v", werr)
	}
	_ = clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp2, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read follow-up resp: %v", err)
	}
	if resp2.StatusCode != 200 {
		t.Fatalf("follow-up: status %d, want 200", resp2.StatusCode)
	}
	_ = resp2.Body.Close()
}
