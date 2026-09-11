package std

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

// TestStdEngineAnswersH2CUpgradeWith101 is the regression guard for the hole
// celeris#440 fell through.
//
// That change moved this engine off x/net/http2/h2c and onto
// http.Server.Protocols. The stdlib serves prior-knowledge h2c but performs
// no RFC 7540 §3.2 Upgrade handshake, so the engine silently stopped
// answering upgrade requests -- while epoll and io_uring, which implement
// the handshake themselves via Config.EnableH2Upgrade, carried on. Three
// engines, two behaviours.
//
// Nothing in this repo caught it. test/spec/h2c_upgrade_test.go skips std
// for every upgrade case, and its comment says why: "std engine uses
// x/net/http2/h2c middleware; its upgrade path is separate. These tests
// target the custom engines." The coverage had been delegated to the
// middleware, so removing the middleware removed the coverage with it. The
// cluster nightly found it instead -- 3,597 upgrade preambles against
// kitchen_sink/std on both architectures and not one 101.
//
// This test is that missing case, on the engine that actually owns the
// behaviour.
func TestStdEngineAnswersH2CUpgradeWith101(t *testing.T) {
	addr := startH2CEngine(t)

	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	// RFC 7540 §3.2: an HTTP/1.1 request carrying Upgrade: h2c plus a
	// base64url SETTINGS payload. An empty SETTINGS frame is valid.
	settings := base64.RawURLEncoding.EncodeToString(nil)
	req := fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\n"+
		"Connection: Upgrade, HTTP2-Settings\r\n"+
		"Upgrade: h2c\r\n"+
		"HTTP2-Settings: %s\r\n\r\n", addr, settings)
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}

	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 101") {
		t.Fatalf("h2c upgrade got %q, want a 101 Switching Protocols.\n"+
			"The std engine is not performing the RFC 7540 §3.2 handshake, so it "+
			"disagrees with epoll and io_uring, which both honour Config.EnableH2Upgrade.",
			strings.TrimSpace(line))
	}
}

// TestStdEngineStillServesPlainHTTP1OnAnH2CListener guards the other half of
// the contract, which is easy to break while fixing the first: an H2C
// listener must still answer an ordinary HTTP/1.1 request. h2c.NewHandler
// gets this right by falling through to the wrapped handler, and a
// hand-rolled upgrade path is exactly where it would be lost.
func TestStdEngineStillServesPlainHTTP1OnAnH2CListener(t *testing.T) {
	addr := startH2CEngine(t)

	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: " + addr + "\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 2") {
		t.Fatalf("plain HTTP/1.1 on an H2C listener got %q, want a 2xx", strings.TrimSpace(line))
	}
}

// startH2CEngine brings up an H2C std engine on a free port and returns its
// address, shutting it down when the test ends.
func startH2CEngine(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close probe listener: %v", err)
	}

	e, err := New(resource.Config{
		Addr:     addr,
		Engine:   engine.Std,
		Protocol: engine.H2C,
	}, &echoHandler{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = e.Listen(ctx) }()

	// Wait for the listener rather than sleeping a fixed amount.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, derr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if derr == nil {
			_ = c.Close()
			return addr
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("engine did not accept on %s within the deadline", addr)
	return ""
}
