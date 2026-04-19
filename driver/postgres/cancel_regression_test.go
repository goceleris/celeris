package postgres

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestSendCancelRequestIgnoresExpiredCtx verifies that sendCancelRequest
// uses its own background context with a 5s timeout rather than the
// caller-provided (potentially already-cancelled) context. Bug 1: the old
// code passed ctx to DialContext, so an already-expired context caused the
// cancel request to fail immediately.
func TestSendCancelRequestIgnoresExpiredCtx(t *testing.T) {
	// Start a TCP listener that accepts one connection.
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	accepted := make(chan []byte, 1)
	go func() {
		c, err := ln.AcceptTCP()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		buf := make([]byte, 16)
		n, _ := c.Read(buf)
		accepted <- buf[:n]
	}()

	// Create an already-cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	addr := ln.Addr().(*net.TCPAddr)
	err = sendCancelRequest(ctx, addr, 42, 99)
	if err != nil {
		t.Fatalf("sendCancelRequest with expired ctx failed: %v", err)
	}

	select {
	case data := <-accepted:
		if len(data) != 16 {
			t.Fatalf("expected 16 bytes, got %d", len(data))
		}
		// Verify it's a valid cancel request.
		expected := buildCancelRequest(42, 99)
		for i := range expected {
			if data[i] != expected[i] {
				t.Fatalf("byte %d: got %x want %x", i, data[i], expected[i])
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never received cancel request")
	}
}
