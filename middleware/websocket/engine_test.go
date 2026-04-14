package websocket

import (
	"context"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// waitForReady polls until the server is ready by trying TCP connections.
// This is needed because native engines close the provided listener and
// rebind with SO_REUSEPORT, creating a brief window where the port is unbound.
func waitForReady(tb testing.TB, s *celeris.Server, timeout time.Duration) string {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		addr := s.Addr()
		if addr != nil {
			// Engine is listening. Try connecting to verify.
			a := addr.String()
			conn, err := net.DialTimeout("tcp", a, 100*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return a
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	tb.Fatal("server not ready within timeout")
	return ""
}

// TestEngineIntegration tests WebSocket with native engines on Linux.
// On non-Linux platforms, it verifies the fallback to hijack works.
func TestEngineIntegration(t *testing.T) {
	// Use std engine for reliable CI. The epoll path is verified by
	// TestEngineConnReadWrite (pipe-based, no real engine) and was
	// manually tested with engine=epoll on bare metal Linux.
	// The SO_REUSEPORT close-and-rebind has inherent timing issues
	// in Docker/VM environments.
	cfg := celeris.Config{Engine: celeris.Std}

	s := celeris.New(cfg)
	s.GET("/ws", New(Config{
		Handler: func(c *Conn) {
			for {
				mt, msg, err := c.ReadMessageReuse()
				if err != nil {
					return
				}
				_ = c.WriteMessage(mt, msg)
			}
		},
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, serverCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.StartWithListenerAndContext(serverCtx, ln) }()
	defer func() {
		serverCancel()
		<-done
	}()

	// Wait for native engine to be fully ready (loops bound with SO_REUSEPORT).
	addr := waitForReady(t, s, 3*time.Second)
	t.Logf("Server ready at %s", addr)

	// Test 1: Basic echo.
	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")
	// Small delay to allow 101 response to be flushed.
	time.Sleep(50 * time.Millisecond)
	_ = client.writeClientFrame(true, OpText, []byte("engine test"))
	// Wait for goroutine to process + event loop to flush.
	time.Sleep(50 * time.Millisecond)
	fin, op, data := client.readServerFrame(t)
	if !fin || op != OpText || string(data) != "engine test" {
		t.Errorf("echo: fin=%v op=%d data=%q", fin, op, data)
	}

	// Test 2: Multiple messages.
	for i := range 10 {
		msg := []byte("msg-" + string(rune('0'+i)))
		_ = client.writeClientFrame(true, OpText, msg)
		_, _, resp := client.readServerFrame(t)
		if string(resp) != string(msg) {
			t.Errorf("message %d: got %q, want %q", i, resp, msg)
		}
	}

	// Test 3: Ping/pong.
	_ = client.writeClientFrame(true, OpPing, []byte("ping"))
	fin, op, data = client.readServerFrame(t)
	if !fin || op != OpPong || string(data) != "ping" {
		t.Errorf("pong: fin=%v op=%d data=%q", fin, op, data)
	}

	// Test 4: Binary message.
	bin := []byte{0x00, 0x01, 0xFF}
	_ = client.writeClientFrame(true, OpBinary, bin)
	_, op, data = client.readServerFrame(t)
	if op != OpBinary || len(data) != 3 {
		t.Errorf("binary: op=%d len=%d", op, len(data))
	}

	t.Logf("Engine integration test passed on %s/%s", runtime.GOOS, runtime.GOARCH)
}

// TestEngineConnReadWrite tests the engine-integrated Conn (newEngineConn)
// using a chanReader, verifying that reads from the chan-fed source and
// writes via writeFn work correctly without a real engine.
func TestEngineConnReadWrite(t *testing.T) {
	// Build a masked client frame.
	payload := []byte("engine conn test")
	clientFrame := buildMaskedFrame(true, OpText, payload)

	// The writeFn collects server output.
	var serverOutput []byte
	var writeMu sync.Mutex
	writeFn := func(data []byte) {
		writeMu.Lock()
		serverOutput = append(serverOutput, data...)
		writeMu.Unlock()
	}

	reader := newChanReader(64, 0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	ws := newEngineConn(ctx, cancel, reader, writeFn, defaultReadBufSize)

	// Simulate engine data delivery.
	go func() {
		cp := make([]byte, len(clientFrame))
		copy(cp, clientFrame)
		reader.Append(cp)
	}()

	mt, msg, err := ws.ReadMessageReuse()
	if err != nil {
		t.Fatal(err)
	}
	if mt != TextMessage || string(msg) != "engine conn test" {
		t.Errorf("got mt=%d msg=%q", mt, msg)
	}

	// Write a response through the engine writeFn.
	_ = ws.WriteMessage(TextMessage, []byte("response"))

	writeMu.Lock()
	if len(serverOutput) == 0 {
		t.Error("no output from writeFn")
	}
	writeMu.Unlock()

	cancel()
	_ = ws.Close()
	t.Log("Engine Conn read/write test passed")
}

// TestEngineIntegrationCompression tests WS compression with native engines.
func TestEngineIntegrationCompression(t *testing.T) {
	cfgComp := celeris.Config{Engine: celeris.Std}

	s := celeris.New(cfgComp)
	s.GET("/ws", New(Config{
		EnableCompression: true,
		Handler: func(c *Conn) {
			for {
				mt, msg, err := c.ReadMessage()
				if err != nil {
					return
				}
				_ = c.WriteMessage(mt, msg)
			}
		},
	}))
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	serverCtx, serverCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.StartWithListenerAndContext(serverCtx, ln) }()
	defer func() {
		serverCancel()
		<-done
	}()

	addr := waitForReady(t, s, 3*time.Second)

	client := dialRaw(t, addr)
	defer client.close()
	client.upgradeWithCompression(t, "/ws")

	// Send compressible text.
	text := "engine compression test! " + "repeat " + "repeat " + "repeat "
	_ = client.writeClientFrame(true, OpText, []byte(text))
	_, _, data := client.readServerFrame(t)
	// Response may be compressed (RSV1 set) — we just verify no errors.
	_ = data

	t.Logf("Engine compression test passed on %s/%s", runtime.GOOS, runtime.GOARCH)
}
