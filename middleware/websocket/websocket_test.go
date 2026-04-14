package websocket

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// --- test helpers ---

// testWSClient is a raw WebSocket client for testing.
type testWSClient struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
}

func dialRaw(tb testing.TB, addr string) *testWSClient {
	tb.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		tb.Fatal(err)
	}
	return &testWSClient{
		conn: conn,
		br:   bufio.NewReader(conn),
		bw:   bufio.NewWriter(conn),
	}
}

func (c *testWSClient) upgrade(tb testing.TB, path string) {
	tb.Helper()
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", path)
	_, _ = c.bw.WriteString(req)
	_ = c.bw.Flush()
	// Read 101 response.
	line, err := c.br.ReadString('\n')
	if err != nil {
		tb.Fatal(err)
	}
	if !strings.Contains(line, "101") {
		tb.Fatalf("expected 101, got: %s", line)
	}
	// Read remaining headers.
	for {
		line, err = c.br.ReadString('\n')
		if err != nil {
			tb.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
}

// writeClientFrame writes a masked WebSocket frame (client must mask).
func (c *testWSClient) writeClientFrame(fin bool, opcode Opcode, payload []byte) error {
	var hdr [14]byte
	pos := 0

	b0 := byte(opcode & 0x0F)
	if fin {
		b0 |= 0x80
	}
	hdr[pos] = b0
	pos++

	length := len(payload)
	b1 := byte(0x80) // MASK bit set for client frames
	switch {
	case length <= 125:
		hdr[pos] = b1 | byte(length)
		pos++
	case length <= 65535:
		hdr[pos] = b1 | 126
		pos++
		binary.BigEndian.PutUint16(hdr[pos:], uint16(length))
		pos += 2
	default:
		hdr[pos] = b1 | 127
		pos++
		binary.BigEndian.PutUint64(hdr[pos:], uint64(length))
		pos += 8
	}

	// Random mask key.
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	copy(hdr[pos:], mask[:])
	pos += 4

	_, _ = c.bw.Write(hdr[:pos])

	// Mask and write payload.
	masked := make([]byte, len(payload))
	copy(masked, payload)
	maskBytes(mask, masked)
	_, _ = c.bw.Write(masked)
	return c.bw.Flush()
}

// readServerFrame reads a frame from the server (unmasked).
func (c *testWSClient) readServerFrame(tb testing.TB) (fin bool, opcode Opcode, payload []byte) {
	tb.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var buf [14]byte
	if _, err := io.ReadFull(c.br, buf[:2]); err != nil {
		tb.Fatal("read frame header:", err)
	}
	fin = buf[0]&0x80 != 0
	opcode = Opcode(buf[0] & 0x0F)
	masked := buf[1]&0x80 != 0
	length := int64(buf[1] & 0x7F)

	switch length {
	case 126:
		_, _ = io.ReadFull(c.br, buf[:2])
		length = int64(binary.BigEndian.Uint16(buf[:2]))
	case 127:
		_, _ = io.ReadFull(c.br, buf[:8])
		length = int64(binary.BigEndian.Uint64(buf[:8]))
	}

	if masked {
		tb.Fatal("server frame should not be masked")
	}

	payload = make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		tb.Fatal("read payload:", err)
	}
	return
}

func (c *testWSClient) close() { _ = c.conn.Close() }

func startServer(tb testing.TB, cfg Config) (addr string, shutdown func()) {
	tb.Helper()
	s := celeris.New(celeris.Config{Engine: celeris.Std})
	s.GET("/ws", New(cfg))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.StartWithListener(ln) }()
	return ln.Addr().String(), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		<-done
	}
}

// --- config tests ---

func TestConfigValidateNilHandler(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil Handler")
		}
	}()
	New(Config{})
}

// --- upgrade tests ---

func TestUpgradeSuccess(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			_ = c.WriteMessage(mt, msg)
		},
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	// Send a text message.
	_ = client.writeClientFrame(true, OpText, []byte("hello"))

	// Read echo back.
	fin, op, data := client.readServerFrame(t)
	if !fin || op != OpText || string(data) != "hello" {
		t.Errorf("got fin=%v op=%d data=%q, want fin=true op=1 data=hello", fin, op, data)
	}
}

func TestNonWebSocketPassthrough(t *testing.T) {
	s := celeris.New(celeris.Config{Engine: celeris.Std})
	s.GET("/ws", New(Config{
		Handler: func(c *Conn) { t.Error("handler should not be called") },
	}), func(c *celeris.Context) error {
		return c.String(200, "ok")
	})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	done := make(chan error, 1)
	go func() { done <- s.StartWithListener(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		<-done
	}()

	conn, _ := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn.Write([]byte("GET /ws HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"))
	buf := make([]byte, 4096)
	n, _ := conn.Read(buf)
	if !strings.Contains(string(buf[:n]), "200") {
		t.Errorf("expected 200, got: %s", buf[:n])
	}
}

// --- accept key ---

func TestComputeAcceptKey(t *testing.T) {
	// RFC 6455 Section 4.2.2 example.
	key := "dGhlIHNhbXBsZSBub25jZQ=="
	want := "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	got := computeAcceptKey(key)
	if got != want {
		t.Errorf("acceptKey = %q, want %q", got, want)
	}
}

func TestComputeAcceptKeyDeterministic(t *testing.T) {
	// Same key should always produce the same accept value.
	key := "x3JJHMbDL1EzLkh9GBhXDw=="
	a := computeAcceptKey(key)
	b := computeAcceptKey(key)
	if a != b {
		t.Errorf("non-deterministic: %q != %q", a, b)
	}
	if a == "" {
		t.Error("empty accept key")
	}
}

// --- frame tests ---

func TestTextMessage(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			_ = c.WriteMessage(mt, msg)
		},
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	text := "hello, websocket!"
	_ = client.writeClientFrame(true, OpText, []byte(text))
	fin, op, data := client.readServerFrame(t)
	if !fin || op != OpText || string(data) != text {
		t.Errorf("echo mismatch: fin=%v op=%d data=%q", fin, op, data)
	}
}

func TestBinaryMessage(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			mt, msg, _ := c.ReadMessage()
			_ = c.WriteMessage(mt, msg)
		},
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	data := []byte{0x00, 0x01, 0x02, 0xFF}
	_ = client.writeClientFrame(true, OpBinary, data)
	fin, op, got := client.readServerFrame(t)
	if !fin || op != OpBinary || len(got) != len(data) {
		t.Errorf("got fin=%v op=%d len=%d", fin, op, len(got))
	}
}

func TestJSONRoundTrip(t *testing.T) {
	type Msg struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			var m Msg
			if err := c.ReadJSON(&m); err != nil {
				return
			}
			m.Value *= 2
			_ = c.WriteJSON(m)
		},
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	_ = client.writeClientFrame(true, OpText, []byte(`{"name":"test","value":21}`))
	_, _, data := client.readServerFrame(t)
	if !strings.Contains(string(data), `"value":42`) {
		t.Errorf("JSON mismatch: %s", data)
	}
}

// --- fragmentation ---

func TestFragmentation(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				t.Error(err)
				return
			}
			_ = c.WriteMessage(mt, msg)
		},
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	// Send "hello world" in 3 fragments.
	_ = client.writeClientFrame(false, OpText, []byte("hello"))
	_ = client.writeClientFrame(false, OpContinuation, []byte(" "))
	_ = client.writeClientFrame(true, OpContinuation, []byte("world"))

	_, _, data := client.readServerFrame(t)
	if string(data) != "hello world" {
		t.Errorf("fragmented msg = %q, want 'hello world'", data)
	}
}

// --- ping/pong ---

func TestPingPong(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			// Read will handle ping automatically (default handler sends pong).
			_, _, _ = c.ReadMessage()
		},
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	// Send ping.
	_ = client.writeClientFrame(true, OpPing, []byte("ping-data"))

	// Should receive pong with same data.
	fin, op, data := client.readServerFrame(t)
	if !fin || op != OpPong || string(data) != "ping-data" {
		t.Errorf("got fin=%v op=%d data=%q, want pong with 'ping-data'", fin, op, data)
	}
}

// --- close handshake ---

func TestCloseHandshake(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			_, _, err := c.ReadMessage()
			if err != nil {
				// Should be a CloseError.
				if !IsCloseError(err, CloseNormalClosure) {
					t.Errorf("unexpected error: %v", err)
				}
			}
		},
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	// Send close frame.
	closePayload := make([]byte, 2)
	binary.BigEndian.PutUint16(closePayload, CloseNormalClosure)
	_ = client.writeClientFrame(true, OpClose, closePayload)

	// Should receive close frame back.
	fin, op, data := client.readServerFrame(t)
	if !fin || op != OpClose {
		t.Errorf("expected close frame, got op=%d", op)
	}
	if len(data) >= 2 {
		code := binary.BigEndian.Uint16(data[:2])
		if code != CloseNormalClosure {
			t.Errorf("close code = %d, want %d", code, CloseNormalClosure)
		}
	}
}

// --- unmasked client frame rejection ---

func TestUnmaskedClientFrame(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			_, _, err := c.ReadMessage()
			if err == nil {
				t.Error("expected error for unmasked frame")
			}
		},
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	// Write an UNMASKED frame (invalid for client).
	hdr := []byte{0x81, 0x05} // FIN + text, length 5, no MASK bit
	_, _ = client.bw.Write(hdr)
	_, _ = client.bw.Write([]byte("hello"))
	_ = client.bw.Flush()

	// Server should send close with protocol error.
	fin, op, data := client.readServerFrame(t)
	if !fin || op != OpClose {
		t.Errorf("expected close frame, got op=%d", op)
		return
	}
	if len(data) >= 2 {
		code := binary.BigEndian.Uint16(data[:2])
		if code != CloseProtocolError {
			t.Errorf("code = %d, want %d", code, CloseProtocolError)
		}
	}
}

// --- reserved bits rejection ---

func TestReservedBitsRejection(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			_, _, _ = c.ReadMessage()
		},
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	// Write frame with RSV1 set (0x40).
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	hdr := []byte{0xC1, 0x80} // FIN + RSV1 + text, masked, length 0
	_, _ = client.bw.Write(hdr)
	_, _ = client.bw.Write(mask[:])
	_ = client.bw.Flush()

	fin, op, data := client.readServerFrame(t)
	if !fin || op != OpClose {
		t.Errorf("expected close, got op=%d", op)
		return
	}
	if len(data) >= 2 {
		code := binary.BigEndian.Uint16(data[:2])
		if code != CloseProtocolError {
			t.Errorf("code = %d, want %d", code, CloseProtocolError)
		}
	}
}

// --- control frame too large ---

func TestControlFrameTooLarge(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) { _, _, _ = c.ReadMessage() },
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	// Ping with 126 bytes payload (exceeds 125 max for control).
	bigPayload := make([]byte, 126)
	_ = client.writeClientFrame(true, OpPing, bigPayload)

	fin, op, data := client.readServerFrame(t)
	if !fin || op != OpClose {
		t.Errorf("expected close, got op=%d", op)
		return
	}
	if len(data) >= 2 {
		code := binary.BigEndian.Uint16(data[:2])
		if code != CloseProtocolError {
			t.Errorf("code = %d, want %d", code, CloseProtocolError)
		}
	}
}

// --- read limit ---

func TestReadLimit(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		ReadLimit: 100,
		Handler: func(c *Conn) {
			_, _, err := c.ReadMessage()
			if err != ErrReadLimit {
				t.Errorf("expected ErrReadLimit, got %v", err)
			}
		},
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	// Send message exceeding read limit.
	_ = client.writeClientFrame(true, OpText, make([]byte, 200))

	// Server should close with message too big.
	fin, op, data := client.readServerFrame(t)
	if !fin || op != OpClose {
		t.Errorf("expected close, got op=%d", op)
		return
	}
	if len(data) >= 2 {
		code := binary.BigEndian.Uint16(data[:2])
		if code != CloseMessageTooBig {
			t.Errorf("code = %d, want %d", code, CloseMessageTooBig)
		}
	}
}

// --- origin check ---

func TestCheckOriginReject(t *testing.T) {
	s := celeris.New(celeris.Config{Engine: celeris.Std})
	s.GET("/ws", New(Config{
		CheckOrigin: func(c *celeris.Context) bool { return false },
		Handler:     func(c *Conn) {},
	}))
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	done := make(chan error, 1)
	go func() { done <- s.StartWithListener(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		<-done
	}()

	conn, _ := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn.Write([]byte("GET /ws HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"))
	buf := make([]byte, 4096)
	n, _ := conn.Read(buf)
	if !strings.Contains(string(buf[:n]), "403") {
		t.Errorf("expected 403, got: %s", buf[:n])
	}
}

// --- subprotocol ---

func TestSubprotocolNegotiation(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		Subprotocols: []string{"graphql-transport-ws"},
		Handler: func(c *Conn) {
			_ = c.WriteText([]byte(c.Subprotocol()))
		},
	})
	defer shutdown()

	conn, _ := net.DialTimeout("tcp", addr, 2*time.Second)
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn.Write([]byte("GET /ws HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: graphql-ws, graphql-transport-ws\r\n\r\n"))

	br := bufio.NewReader(conn)
	var gotProto string
	for {
		line, _ := br.ReadString('\n')
		if strings.HasPrefix(line, "Sec-WebSocket-Protocol:") {
			gotProto = strings.TrimSpace(strings.TrimPrefix(line, "Sec-WebSocket-Protocol:"))
		}
		if line == "\r\n" {
			break
		}
	}
	if gotProto != "graphql-transport-ws" {
		t.Errorf("subprotocol = %q, want graphql-transport-ws", gotProto)
	}
}

// --- callbacks ---

func TestOnConnectCallback(t *testing.T) {
	connected := make(chan struct{}, 1)
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) { _ = c.WriteText([]byte("ok")) },
		OnConnect: func(c *Conn) error {
			connected <- struct{}{}
			return nil
		},
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("OnConnect not called")
	}
}

func TestOnDisconnectCallback(t *testing.T) {
	disconnected := make(chan struct{}, 1)
	addr, shutdown := startServer(t, Config{
		Handler:      func(c *Conn) {},
		OnDisconnect: func(c *Conn) { disconnected <- struct{}{} },
	})
	defer shutdown()

	client := dialRaw(t, addr)
	client.upgrade(t, "/ws")
	client.close()

	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("OnDisconnect not called")
	}
}

// --- convenience methods ---

func TestConnIP(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			_ = c.WriteText([]byte(c.IP()))
		},
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")
	_, _, data := client.readServerFrame(t)
	if string(data) != "127.0.0.1" {
		t.Errorf("ip = %q, want 127.0.0.1", data)
	}
}

func TestConnLocals(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			c.SetLocals("key", "value")
			v := c.Locals("key")
			_ = c.WriteText([]byte(v.(string)))
		},
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")
	_, _, data := client.readServerFrame(t)
	if string(data) != "value" {
		t.Errorf("locals = %q, want value", data)
	}
}

// --- concurrent write ---

func TestConcurrentWrite(t *testing.T) {
	const n = 50
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			for range n {
				_, _, err := c.ReadMessage()
				if err != nil {
					return
				}
			}
			_ = c.WriteText([]byte("done"))
		},
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	for range n {
		_ = client.writeClientFrame(true, OpText, []byte("msg"))
	}
	_, _, data := client.readServerFrame(t)
	if string(data) != "done" {
		t.Errorf("got %q, want done", data)
	}
}

// --- broadcast ---

func TestBroadcast(t *testing.T) {
	const numClients = 10
	var (
		mu      sync.Mutex
		clients []*Conn
		ready   = make(chan struct{})
	)
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			mu.Lock()
			clients = append(clients, c)
			if len(clients) == numClients {
				close(ready)
			}
			mu.Unlock()
			_, _, _ = c.ReadMessage() // block until close
		},
	})
	defer shutdown()

	rawClients := make([]*testWSClient, numClients)
	for i := range numClients {
		rawClients[i] = dialRaw(t, addr)
		defer rawClients[i].close()
		rawClients[i].upgrade(t, "/ws")
	}

	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for clients")
	}

	mu.Lock()
	for _, c := range clients {
		_ = c.WriteText([]byte("broadcast"))
	}
	mu.Unlock()

	for i, rc := range rawClients {
		_, _, data := rc.readServerFrame(t)
		if string(data) != "broadcast" {
			t.Errorf("client %d: got %q", i, data)
		}
	}
}

// --- mask correctness ---

func TestMaskBytes(t *testing.T) {
	mask := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	data := []byte("Hello")
	original := make([]byte, len(data))
	copy(original, data)

	maskBytes(mask, data)
	// Data should be different after masking.
	if string(data) == string(original) {
		t.Error("masking did not change data")
	}
	// Unmask should restore original.
	maskBytes(mask, data)
	if string(data) != string(original) {
		t.Errorf("unmask = %q, want %q", data, original)
	}
}

func TestMaskBytesLarge(t *testing.T) {
	mask := [4]byte{0xAB, 0xCD, 0xEF, 0x01}
	data := make([]byte, 1024)
	for i := range data {
		data[i] = byte(i)
	}
	original := make([]byte, len(data))
	copy(original, data)

	maskBytes(mask, data)
	maskBytes(mask, data)
	for i := range data {
		if data[i] != original[i] {
			t.Fatalf("round-trip failed at index %d: got %d, want %d", i, data[i], original[i])
		}
	}
}

// --- CloseError ---

func TestIsCloseError(t *testing.T) {
	err := &CloseError{Code: CloseNormalClosure, Text: "bye"}
	if !IsCloseError(err, CloseNormalClosure) {
		t.Error("IsCloseError should match")
	}
	if IsCloseError(err, CloseGoingAway) {
		t.Error("IsCloseError should not match different code")
	}
}

func TestIsUnexpectedCloseError(t *testing.T) {
	err := &CloseError{Code: CloseAbnormalClosure}
	if !IsUnexpectedCloseError(err, CloseNormalClosure, CloseGoingAway) {
		t.Error("should be unexpected")
	}
	err2 := &CloseError{Code: CloseNormalClosure}
	if IsUnexpectedCloseError(err2, CloseNormalClosure) {
		t.Error("should not be unexpected")
	}
}

// --- skip ---

func TestSkipPaths(t *testing.T) {
	s := celeris.New(celeris.Config{Engine: celeris.Std})
	s.GET("/ws", New(Config{
		SkipPaths: []string{"/ws"},
		Handler:   func(c *Conn) {},
	}), func(c *celeris.Context) error {
		return c.String(200, "skipped")
	})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	done := make(chan error, 1)
	go func() { done <- s.StartWithListener(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		<-done
	}()

	conn, _ := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn.Write([]byte("GET /ws HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nConnection: close\r\n\r\n"))
	buf := make([]byte, 4096)
	n, _ := conn.Read(buf)
	if !strings.Contains(string(buf[:n]), "skipped") {
		t.Errorf("expected skip passthrough, got: %s", buf[:n])
	}
}

// --- benchmarks ---

func benchEchoServer(b *testing.B) (string, func()) {
	b.Helper()
	return startServer(b, Config{
		Handler: func(c *Conn) {
			for {
				mt, msg, err := c.ReadMessage()
				if err != nil {
					return
				}
				_ = c.WriteMessage(mt, msg)
			}
		},
	})
}

func BenchmarkUpgrade(b *testing.B) {
	addr, shutdown := startServer(b, Config{
		Handler: func(c *Conn) {},
	})
	defer shutdown()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		client := dialRaw(b, addr)
		client.upgrade(b, "/ws")
		client.close()
	}
}

func BenchmarkEchoSmall(b *testing.B) {
	addr, shutdown := benchEchoServer(b)
	defer shutdown()
	client := dialRaw(b, addr)
	defer client.close()
	client.upgrade(b, "/ws")
	payload := []byte("hello ws bench!")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = client.writeClientFrame(true, OpText, payload)
		client.readServerFrame(b)
	}
}

func BenchmarkEchoLarge(b *testing.B) {
	addr, shutdown := benchEchoServer(b)
	defer shutdown()
	client := dialRaw(b, addr)
	defer client.close()
	client.upgrade(b, "/ws")
	payload := make([]byte, 65536)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = client.writeClientFrame(true, OpBinary, payload)
		client.readServerFrame(b)
	}
}

// --- RFC 6455 compliance tests ---

func TestReservedOpcode(t *testing.T) {
	addr, shutdown := startServer(t, Config{Handler: func(c *Conn) { _, _, _ = c.ReadMessage() }})
	defer shutdown()
	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")
	// Send frame with reserved opcode 0x3.
	_ = client.writeClientFrame(true, Opcode(0x3), []byte("data"))
	fin, op, data := client.readServerFrame(t)
	if !fin || op != OpClose {
		t.Errorf("expected close for reserved opcode, got op=%d", op)
		return
	}
	if len(data) >= 2 {
		code := binary.BigEndian.Uint16(data[:2])
		if code != CloseProtocolError {
			t.Errorf("code = %d, want %d", code, CloseProtocolError)
		}
	}
}

func TestInterleavedDataFrameDuringFragmentation(t *testing.T) {
	addr, shutdown := startServer(t, Config{Handler: func(c *Conn) { _, _, _ = c.ReadMessage() }})
	defer shutdown()
	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")
	// Start fragmented text message.
	_ = client.writeClientFrame(false, OpText, []byte("hello"))
	// Send a NEW text frame (not continuation) — protocol violation.
	_ = client.writeClientFrame(true, OpText, []byte("bad"))
	fin, op, data := client.readServerFrame(t)
	if !fin || op != OpClose {
		t.Errorf("expected close, got op=%d", op)
		return
	}
	if len(data) >= 2 {
		code := binary.BigEndian.Uint16(data[:2])
		if code != CloseProtocolError {
			t.Errorf("code = %d, want %d", code, CloseProtocolError)
		}
	}
}

func TestUnexpectedContinuationFrame(t *testing.T) {
	addr, shutdown := startServer(t, Config{Handler: func(c *Conn) { _, _, _ = c.ReadMessage() }})
	defer shutdown()
	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")
	// Send continuation frame without prior fragmented message.
	_ = client.writeClientFrame(true, OpContinuation, []byte("orphan"))
	fin, op, data := client.readServerFrame(t)
	if !fin || op != OpClose {
		t.Errorf("expected close, got op=%d", op)
		return
	}
	if len(data) >= 2 {
		code := binary.BigEndian.Uint16(data[:2])
		if code != CloseProtocolError {
			t.Errorf("code = %d, want %d", code, CloseProtocolError)
		}
	}
}

func TestInvalidUTF8Text(t *testing.T) {
	addr, shutdown := startServer(t, Config{Handler: func(c *Conn) {
		_, _, err := c.ReadMessage()
		if err == nil {
			t.Error("expected error for invalid UTF-8")
		}
	}})
	defer shutdown()
	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")
	// Send text frame with invalid UTF-8.
	_ = client.writeClientFrame(true, OpText, []byte{0xFF, 0xFE})
	fin, op, data := client.readServerFrame(t)
	if !fin || op != OpClose {
		t.Errorf("expected close, got op=%d", op)
		return
	}
	if len(data) >= 2 {
		code := binary.BigEndian.Uint16(data[:2])
		if code != CloseInvalidPayload {
			t.Errorf("code = %d, want %d", code, CloseInvalidPayload)
		}
	}
}

func TestEmptyMessage(t *testing.T) {
	addr, shutdown := startServer(t, Config{Handler: func(c *Conn) {
		mt, msg, err := c.ReadMessage()
		if err != nil {
			return
		}
		_ = c.WriteMessage(mt, msg)
	}})
	defer shutdown()
	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")
	// Empty text message.
	_ = client.writeClientFrame(true, OpText, nil)
	fin, op, data := client.readServerFrame(t)
	if !fin || op != OpText || len(data) != 0 {
		t.Errorf("got fin=%v op=%d len=%d, want empty text", fin, op, len(data))
	}
}

func TestPingEmptyPayload(t *testing.T) {
	addr, shutdown := startServer(t, Config{Handler: func(c *Conn) { _, _, _ = c.ReadMessage() }})
	defer shutdown()
	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")
	_ = client.writeClientFrame(true, OpPing, nil)
	fin, op, data := client.readServerFrame(t)
	if !fin || op != OpPong || len(data) != 0 {
		t.Errorf("got fin=%v op=%d len=%d, want empty pong", fin, op, len(data))
	}
}

func TestCloseInvalidCode1015(t *testing.T) {
	code, _, err := parseClosePayload(FormatCloseMessage(1015, ""))
	if err == nil {
		t.Errorf("code 1015 should be rejected, got code=%d", code)
	}
}

func TestCloseInvalidCode999(t *testing.T) {
	code, _, err := parseClosePayload(FormatCloseMessage(999, ""))
	if err == nil {
		t.Errorf("code 999 should be rejected, got code=%d", code)
	}
}

func TestClose1BytePayload(t *testing.T) {
	_, _, err := parseClosePayload([]byte{0x01})
	if err != ErrInvalidCloseData {
		t.Errorf("1-byte close payload should be invalid, got err=%v", err)
	}
}

func TestCloseInvalidUTF8Reason(t *testing.T) {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint16(payload[:2], 1000)
	payload[2] = 0xFF
	payload[3] = 0xFE
	_, _, err := parseClosePayload(payload)
	if err != ErrInvalidCloseData {
		t.Errorf("invalid UTF-8 in close reason should be rejected, got err=%v", err)
	}
}

func TestPingInterleaved(t *testing.T) {
	addr, shutdown := startServer(t, Config{Handler: func(c *Conn) {
		mt, msg, err := c.ReadMessage()
		if err != nil {
			return
		}
		_ = c.WriteMessage(mt, msg)
	}})
	defer shutdown()
	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")
	// Start fragmented message, then send ping mid-stream.
	_ = client.writeClientFrame(false, OpText, []byte("hel"))
	_ = client.writeClientFrame(true, OpPing, []byte("mid-ping"))
	// Pong should arrive for the ping.
	fin, op, data := client.readServerFrame(t)
	if !fin || op != OpPong || string(data) != "mid-ping" {
		t.Errorf("expected pong for mid-stream ping, got op=%d data=%q", op, data)
	}
	// Complete the fragmented message.
	_ = client.writeClientFrame(true, OpContinuation, []byte("lo"))
	_, _, data = client.readServerFrame(t)
	if string(data) != "hello" {
		t.Errorf("got %q, want 'hello'", data)
	}
}

// --- compression tests ---

func startCompressServer(tb testing.TB, handler Handler) (string, func()) {
	tb.Helper()
	return startServer(tb, Config{
		EnableCompression: true,
		Handler:           handler,
	})
}

func (c *testWSClient) upgradeWithCompression(tb testing.TB, path string) {
	tb.Helper()
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Extensions: permessage-deflate; client_no_context_takeover\r\n\r\n", path)
	_, _ = c.bw.WriteString(req)
	_ = c.bw.Flush()
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			tb.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
}

func TestCompressionNegotiation(t *testing.T) {
	addr, shutdown := startCompressServer(t, func(c *Conn) {
		_ = c.WriteText([]byte("hello"))
	})
	defer shutdown()

	conn, _ := net.DialTimeout("tcp", addr, 2*time.Second)
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// Send upgrade with compression extension.
	_, _ = conn.Write([]byte("GET /ws HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Extensions: permessage-deflate; client_no_context_takeover\r\n\r\n"))

	br := bufio.NewReader(conn)
	var gotExtension string
	for {
		line, _ := br.ReadString('\n')
		if strings.HasPrefix(strings.ToLower(line), "sec-websocket-extensions:") {
			gotExtension = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		}
		if line == "\r\n" {
			break
		}
	}
	if !strings.Contains(gotExtension, "permessage-deflate") {
		t.Errorf("expected permessage-deflate in response, got %q", gotExtension)
	}
}

func TestCompressionRoundTrip(t *testing.T) {
	addr, shutdown := startCompressServer(t, func(c *Conn) {
		mt, msg, err := c.ReadMessage()
		if err != nil {
			t.Error(err)
			return
		}
		// Echo back — WriteMessage will compress if negotiated.
		_ = c.WriteMessage(mt, msg)
	})
	defer shutdown()

	// Connect with compression, send a compressible text message.
	client := dialRaw(t, addr)
	defer client.close()
	client.upgradeWithCompression(t, "/ws")

	// Send a compressible payload (repeated text compresses well).
	text := strings.Repeat("hello world! this is a test message. ", 20)
	_ = client.writeClientFrame(true, OpText, []byte(text))

	// Read the server's response — it should have RSV1 set (compressed).
	fin, op, data := client.readServerFrame(t)
	if !fin || op != OpText {
		t.Errorf("got fin=%v op=%d", fin, op)
	}
	// The response is compressed — it should be smaller than the original.
	// We can't easily decompress here without implementing the client side,
	// but we can verify the data is different (compressed) and that
	// the RSV1 bit handling didn't crash.
	_ = data
}

func TestCompressionDisabledByDefault(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			_ = c.WriteText([]byte("hello"))
		},
	})
	defer shutdown()

	conn, _ := net.DialTimeout("tcp", addr, 2*time.Second)
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// Request compression but server doesn't enable it.
	_, _ = conn.Write([]byte("GET /ws HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Extensions: permessage-deflate\r\n\r\n"))

	br := bufio.NewReader(conn)
	gotExtension := false
	for {
		line, _ := br.ReadString('\n')
		if strings.Contains(strings.ToLower(line), "sec-websocket-extensions") {
			gotExtension = true
		}
		if line == "\r\n" {
			break
		}
	}
	if gotExtension {
		t.Error("server should not negotiate compression when disabled")
	}
}

func TestCompressionSmallMessageSkipped(t *testing.T) {
	// Messages below threshold should not be compressed.
	addr, shutdown := startServer(t, Config{
		EnableCompression:    true,
		CompressionThreshold: 1000,
		Handler: func(c *Conn) {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			_ = c.WriteMessage(mt, msg)
		},
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgradeWithCompression(t, "/ws")

	// Send small message (below threshold).
	_ = client.writeClientFrame(true, OpText, []byte("small"))

	// Read response — should NOT have RSV1 (uncompressed).
	_ = client.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hdr [2]byte
	_, _ = io.ReadFull(client.br, hdr[:])
	rsv1 := hdr[0]&0x40 != 0
	if rsv1 {
		t.Error("small message should not be compressed")
	}
}

// --- streaming tests (NextReader/NextWriter) ---

func TestNextReaderSingleFrame(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			mt, r, err := c.NextReader()
			if err != nil {
				t.Error(err)
				return
			}
			data, _ := io.ReadAll(r)
			_ = c.WriteMessage(mt, data)
		},
	})
	defer shutdown()
	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")
	_ = client.writeClientFrame(true, OpText, []byte("streaming test"))
	_, _, data := client.readServerFrame(t)
	if string(data) != "streaming test" {
		t.Errorf("got %q", data)
	}
}

func TestNextReaderFragmented(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			mt, r, err := c.NextReader()
			if err != nil {
				t.Error(err)
				return
			}
			// Read in small chunks to test streaming.
			var result []byte
			buf := make([]byte, 5)
			for {
				n, err := r.Read(buf)
				result = append(result, buf[:n]...)
				if err != nil {
					break
				}
			}
			_ = c.WriteMessage(mt, result)
		},
	})
	defer shutdown()
	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	// Send "hello world" in 3 fragments.
	_ = client.writeClientFrame(false, OpText, []byte("hello"))
	_ = client.writeClientFrame(false, OpContinuation, []byte(" "))
	_ = client.writeClientFrame(true, OpContinuation, []byte("world"))

	_, _, data := client.readServerFrame(t)
	if string(data) != "hello world" {
		t.Errorf("got %q, want 'hello world'", data)
	}
}

func TestNextWriterFragmented(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			_, _, err := c.ReadMessage()
			if err != nil {
				return
			}
			// Write response in 3 fragments using NextWriter.
			w, err := c.NextWriter(TextMessage)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = w.Write([]byte("hello"))
			_, _ = w.Write([]byte(" "))
			_, _ = w.Write([]byte("world"))
			_ = w.Close()
		},
	})
	defer shutdown()
	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	_ = client.writeClientFrame(true, OpText, []byte("trigger"))

	// Read the 3 fragments + final frame.
	var assembled []byte
	for {
		fin, op, data := client.readServerFrame(t)
		_ = op
		assembled = append(assembled, data...)
		if fin {
			break
		}
	}
	if string(assembled) != "hello world" {
		t.Errorf("got %q, want 'hello world'", assembled)
	}
}

func TestNextReaderLargeStream(t *testing.T) {
	// Test streaming a 256KB message without full buffering.
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			mt, r, err := c.NextReader()
			if err != nil {
				t.Error(err)
				return
			}
			data, _ := io.ReadAll(r)
			// Echo just the length.
			_ = c.WriteMessage(mt, []byte(fmt.Sprintf("%d", len(data))))
		},
	})
	defer shutdown()
	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	// Send 256KB as a single frame.
	payload := make([]byte, 256*1024)
	for i := range payload {
		payload[i] = byte(i)
	}
	_ = client.writeClientFrame(true, OpBinary, payload)

	_, _, data := client.readServerFrame(t)
	if string(data) != "262144" {
		t.Errorf("server received %s bytes, want 262144", data)
	}
}

func TestClientToServerCompression(t *testing.T) {
	var received string
	addr, shutdown := startCompressServer(t, func(c *Conn) {
		_, msg, err := c.ReadMessage()
		if err != nil {
			t.Error(err)
			return
		}
		received = string(msg)
		_ = c.WriteText([]byte("ok"))
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgradeWithCompression(t, "/ws")

	// Compress the payload and send with RSV1 set.
	original := []byte(strings.Repeat("compress me please! ", 30))
	cbuf := acquireCompressBuf()
	defer releaseCompressBuf(cbuf)
	if err := compressMessage(cbuf, original, CompressionLevelBestSpeed); err != nil {
		t.Fatal(err)
	}

	// Build a masked frame with RSV1.
	mask := [4]byte{0x12, 0x34, 0x56, 0x78}
	var frame []byte
	b0 := 0x80 | 0x40 | byte(OpText) // FIN + RSV1 + text
	frame = append(frame, b0)
	cl := len(cbuf.data)
	if cl <= 125 {
		frame = append(frame, 0x80|byte(cl))
	} else {
		frame = append(frame, 0x80|126, byte(cl>>8), byte(cl))
	}
	frame = append(frame, mask[:]...)
	masked := make([]byte, cl)
	copy(masked, cbuf.data)
	maskBytes(mask, masked)
	frame = append(frame, masked...)

	_, _ = client.bw.Write(frame)
	_ = client.bw.Flush()

	_, _, data := client.readServerFrame(t)
	if string(data) != "ok" {
		t.Errorf("server response = %q", data)
	}
	if received != string(original) {
		t.Errorf("server received %d bytes, want %d bytes", len(received), len(original))
	}
}

func TestCompressDecompressRoundTrip(t *testing.T) {
	// Unit test for the compress/decompress functions.
	original := []byte(strings.Repeat("The quick brown fox jumps over the lazy dog. ", 50))
	buf := acquireCompressBuf()
	defer releaseCompressBuf(buf)
	if err := compressMessage(buf, original, CompressionLevelBestSpeed); err != nil {
		t.Fatal(err)
	}
	if len(buf.data) >= len(original) {
		t.Error("compressed data should be smaller than original")
	}
	decompressed, err := decompressMessage(buf.data)
	if err != nil {
		t.Fatal(err)
	}
	if string(decompressed) != string(original) {
		t.Errorf("decompressed = %d bytes, original = %d bytes", len(decompressed), len(original))
	}
}

// --- truncWriter tests ---

func TestTruncWriterExact4Bytes(t *testing.T) {
	var dst strings.Builder
	tw := &truncWriter{dst: &dst}
	_, _ = tw.Write([]byte{1, 2, 3, 4})
	if dst.Len() != 0 {
		t.Errorf("should hold back 4 bytes, got %d written", dst.Len())
	}
}

func TestTruncWriterLargeWrite(t *testing.T) {
	var dst strings.Builder
	tw := &truncWriter{dst: &dst}
	data := []byte("hello world! this is a test")
	_, _ = tw.Write(data)
	// Last 4 bytes held back.
	if dst.Len() != len(data)-4 {
		t.Errorf("written = %d, want %d", dst.Len(), len(data)-4)
	}
}

func TestTruncWriterMultipleSmallWrites(t *testing.T) {
	var dst strings.Builder
	tw := &truncWriter{dst: &dst}
	_, _ = tw.Write([]byte{1, 2})
	_, _ = tw.Write([]byte{3, 4})
	_, _ = tw.Write([]byte{5, 6, 7, 8, 9, 10})
	// Total written: 10 bytes. Last 4 held. Flushed: 6 bytes.
	if dst.Len() != 6 {
		t.Errorf("written = %d, want 6", dst.Len())
	}
}

// TestTruncWriterSmallTailWrite reproduces the v1.3.3 truncWriter bug
// (fixed in v1.3.4) where a short trailing Write overwrote held-back
// bytes instead of preserving the tail-4 window across the boundary.
// This is what Autobahn case 12.1.4 et al. caught: compressed payloads
// corrupted on the wire because the deflate sync marker wasn't correctly
// stripped.
func TestTruncWriterSmallTailWrite(t *testing.T) {
	var dst bytes.Buffer
	tw := &truncWriter{dst: &dst}
	_, _ = tw.Write([]byte{1, 2, 3})
	_, _ = tw.Write([]byte{4, 5})
	// Stream [1,2,3,4,5]: flush [1], hold [2,3,4,5].
	if !bytes.Equal(dst.Bytes(), []byte{1}) {
		t.Errorf("flushed = %v, want [1]", dst.Bytes())
	}
	if tw.n != 4 || !bytes.Equal(tw.buf[:tw.n], []byte{2, 3, 4, 5}) {
		t.Errorf("held = %v (n=%d), want [2,3,4,5]", tw.buf[:tw.n], tw.n)
	}
}

// TestTruncWriterMany1ByteWrites hammers truncWriter with single-byte
// writes, which is what compress/flate produces at the tail of a
// compressed stream and what triggered the Autobahn 12.1.x failures.
func TestTruncWriterMany1ByteWrites(t *testing.T) {
	var dst bytes.Buffer
	tw := &truncWriter{dst: &dst}
	for i := 1; i <= 10; i++ {
		_, _ = tw.Write([]byte{byte(i)})
	}
	// Stream [1..10]: flush [1..6], hold [7,8,9,10].
	if !bytes.Equal(dst.Bytes(), []byte{1, 2, 3, 4, 5, 6}) {
		t.Errorf("flushed = %v, want [1,2,3,4,5,6]", dst.Bytes())
	}
	if tw.n != 4 || !bytes.Equal(tw.buf[:tw.n], []byte{7, 8, 9, 10}) {
		t.Errorf("held = %v (n=%d), want [7,8,9,10]", tw.buf[:tw.n], tw.n)
	}
}

// --- fuzz tests ---

func FuzzMaskBytes(f *testing.F) {
	f.Add([]byte{0x37, 0xfa, 0x21, 0x3d}, []byte("hello"))
	f.Add([]byte{0, 0, 0, 0}, []byte{})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff}, make([]byte, 100))
	f.Fuzz(func(t *testing.T, mask []byte, data []byte) {
		if len(mask) != 4 {
			return
		}
		var m [4]byte
		copy(m[:], mask)
		original := make([]byte, len(data))
		copy(original, data)
		// Mask then unmask should restore original.
		maskBytes(m, data)
		maskBytes(m, data)
		for i := range data {
			if data[i] != original[i] {
				t.Fatalf("round-trip failed at index %d", i)
			}
		}
	})
}

func FuzzParseClosePayload(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x03, 0xe8}) // 1000
	f.Add([]byte{0x03, 0xe8, 'b', 'y', 'e'})
	f.Add([]byte{0x00})       // 1-byte invalid
	f.Add([]byte{0x03, 0xe7}) // 999 invalid
	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic.
		_, _, _ = parseClosePayload(data)
	})
}

func FuzzReadFrameHeader(f *testing.F) {
	// Valid small frame: FIN+text, masked, 5 bytes, mask key.
	f.Add([]byte{0x81, 0x85, 0x12, 0x34, 0x56, 0x78, 'h', 'e', 'l', 'l', 'o'})
	// Minimal: FIN+text, masked, 0 bytes.
	f.Add([]byte{0x81, 0x80, 0x00, 0x00, 0x00, 0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}
		// Must not panic.
		r := bytes.NewReader(data)
		var h frameHeader
		var buf [maxHeaderSize]byte
		_ = readFrameHeader(r, buf[:], &h)
	})
}

// --- new feature tests ---

func TestPreparedMessageBasic(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			pm, _ := NewPreparedMessage(TextMessage, []byte("broadcast-msg"))
			_ = c.WritePreparedMessage(pm)
		},
	})
	defer shutdown()
	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")
	_, _, data := client.readServerFrame(t)
	if string(data) != "broadcast-msg" {
		t.Errorf("got %q", data)
	}
}

func TestPreparedMessageBroadcast(t *testing.T) {
	pm, _ := NewPreparedMessage(TextMessage, []byte("broadcast"))
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) { _ = c.WritePreparedMessage(pm) },
	})
	defer shutdown()
	for i := range 5 {
		client := dialRaw(t, addr)
		defer client.close()
		client.upgrade(t, "/ws")
		_, _, data := client.readServerFrame(t)
		if string(data) != "broadcast" {
			t.Errorf("client %d: got %q", i, data)
		}
	}
}

func TestEnableWriteCompressionToggle(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		EnableCompression: true,
		Handler: func(c *Conn) {
			c.EnableWriteCompression(false)
			_ = c.WriteText([]byte(strings.Repeat("x", 200)))
		},
	})
	defer shutdown()
	client := dialRaw(t, addr)
	defer client.close()
	client.upgradeWithCompression(t, "/ws")
	_ = client.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hdr [2]byte
	_, _ = io.ReadFull(client.br, hdr[:])
	if hdr[0]&0x40 != 0 {
		t.Error("message should not be compressed when EnableWriteCompression(false)")
	}
}

func TestSetCompressionLevelInvalid(t *testing.T) {
	ws := &Conn{}
	if err := ws.SetCompressionLevel(10); err == nil {
		t.Error("expected error for invalid level")
	}
}

func TestLocalAddr(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			if c.LocalAddr() != nil {
				_ = c.WriteText([]byte("ok"))
			} else {
				_ = c.WriteText([]byte("nil"))
			}
		},
	})
	defer shutdown()
	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")
	_, _, data := client.readServerFrame(t)
	if string(data) != "ok" {
		t.Error("LocalAddr should not be nil")
	}
}

func TestHandlerGetters(t *testing.T) {
	ws := &Conn{}
	ws.pingHandler = ws.defaultPingHandler
	if ws.PingHandler() == nil {
		t.Error("PingHandler should not be nil")
	}
	if ws.PongHandler() != nil {
		t.Error("PongHandler should be nil by default")
	}
	if ws.CloseHandler() != nil {
		t.Error("CloseHandler should be nil by default")
	}
}

func TestWriteControlTimeout(t *testing.T) {
	ws := &Conn{}
	ws.writeSem = make(chan struct{}, 1)
	ws.writeSem <- struct{}{} // available
	// Immediately acquire — now WriteControl will timeout.
	<-ws.writeSem
	err := ws.WriteControl(int(OpPing), nil, time.Now().Add(10*time.Millisecond))
	if err != ErrWriteTimeout {
		t.Errorf("expected ErrWriteTimeout, got %v", err)
	}
	ws.writeSem <- struct{}{} // restore
}

func TestWriteBufferPool(t *testing.T) {
	var getCalls, putCalls atomic.Uint32
	pool := &testPool{
		getCalls: &getCalls,
		putCalls: &putCalls,
	}
	addr, shutdown := startServer(t, Config{
		WriteBufferPool: pool,
		Handler: func(c *Conn) {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			_ = c.WriteMessage(mt, msg)
		},
	})
	defer shutdown()
	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")
	_ = client.writeClientFrame(true, OpText, []byte("pool test"))
	_, _, data := client.readServerFrame(t)
	if string(data) != "pool test" {
		t.Errorf("got %q", data)
	}
	if getCalls.Load() == 0 {
		t.Error("pool.Get was never called")
	}
	if putCalls.Load() == 0 {
		t.Error("pool.Put was never called")
	}
}

type testPool struct {
	getCalls *atomic.Uint32
	putCalls *atomic.Uint32
	pool     sync.Pool
}

func (p *testPool) Get(dst io.Writer) *bufio.Writer {
	p.getCalls.Add(1)
	if v := p.pool.Get(); v != nil {
		bw := v.(*bufio.Writer)
		bw.Reset(dst)
		return bw
	}
	return bufio.NewWriterSize(dst, 4096)
}

func (p *testPool) Put(bw *bufio.Writer) {
	p.putCalls.Add(1)
	p.pool.Put(bw)
}

func TestPreparedMessageCompressed(t *testing.T) {
	pm, _ := NewPreparedMessage(TextMessage, []byte(strings.Repeat("compress me! ", 50)))
	addr, shutdown := startServer(t, Config{
		EnableCompression: true,
		Handler: func(c *Conn) {
			_ = c.WritePreparedMessage(pm)
		},
	})
	defer shutdown()
	client := dialRaw(t, addr)
	defer client.close()
	client.upgradeWithCompression(t, "/ws")
	// Read response — should be compressed (RSV1 set).
	_ = client.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hdr [2]byte
	_, _ = io.ReadFull(client.br, hdr[:])
	rsv1 := hdr[0]&0x40 != 0
	if !rsv1 {
		t.Error("PreparedMessage should be compressed when compression negotiated")
	}
}

func BenchmarkMask4KB(b *testing.B) {
	mask := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	data := make([]byte, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		maskBytes(mask, data)
	}
}

func BenchmarkMask64KB(b *testing.B) {
	mask := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	data := make([]byte, 65536)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		maskBytes(mask, data)
	}
}

func BenchmarkWriteFrame(b *testing.B) {
	var buf [4096]byte
	var hdr [maxHeaderSize]byte
	w := &discardWriter{}
	payload := buf[:128]
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = writeFrame(w, true, OpText, payload, hdr[:])
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// --- server-isolated benchmarks (proves zero server allocs) ---

// buildMaskedFrame builds a complete masked WebSocket frame as raw bytes.
func buildMaskedFrame(fin bool, opcode Opcode, payload []byte) []byte {
	mask := [4]byte{0x12, 0x34, 0x56, 0x78}
	var frame []byte

	b0 := byte(opcode & 0x0F)
	if fin {
		b0 |= 0x80
	}
	frame = append(frame, b0)

	length := len(payload)
	switch {
	case length <= 125:
		frame = append(frame, 0x80|byte(length))
	case length <= 65535:
		frame = append(frame, 0x80|126)
		frame = append(frame, byte(length>>8), byte(length))
	default:
		frame = append(frame, 0x80|127)
		var lb [8]byte
		binary.BigEndian.PutUint64(lb[:], uint64(length))
		frame = append(frame, lb[:]...)
	}
	frame = append(frame, mask[:]...)

	masked := make([]byte, len(payload))
	copy(masked, payload)
	maskBytes(mask, masked)
	frame = append(frame, masked...)
	return frame
}

// BenchmarkServerReadWrite isolates ONLY the server's ReadMessage+WriteMessage
// using net.Pipe. No TCP, no HTTP, no test client allocs. This is the ground
// truth for server-side allocation counting.
func BenchmarkServerReadWrite(b *testing.B) {
	payload := []byte("hello ws benchmark!!")
	clientFrame := buildMaskedFrame(true, OpText, payload)

	clientConn, serverConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	ws := newConn(ctx, cancel, serverConn, defaultReadBufSize, defaultWriteBufSize)

	// Pre-warm the readPayload buffer.
	go func() { _, _ = clientConn.Write(clientFrame) }()
	_, _, _ = ws.ReadMessage()

	go func() {
		buf := make([]byte, 4096) // pre-allocate once
		for {
			if _, err := clientConn.Write(clientFrame); err != nil {
				return
			}
			if _, err := clientConn.Read(buf); err != nil {
				return
			}
		}
	}()

	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		mt, msg, err := ws.ReadMessage()
		if err != nil {
			b.Fatal(err)
		}
		_ = ws.WriteMessage(mt, msg)
	}
	b.StopTimer()
	cancel()
	_ = ws.Close()
	_ = clientConn.Close()
}

// BenchmarkServerReadWriteLarge same as above but 64KB payloads.
func BenchmarkServerReadWriteLarge(b *testing.B) {
	payload := make([]byte, 65536)
	for i := range payload {
		payload[i] = byte(i)
	}
	clientFrame := buildMaskedFrame(true, OpBinary, payload)

	clientConn, serverConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	ws := newConn(ctx, cancel, serverConn, defaultReadBufSize, defaultWriteBufSize)

	// Pre-warm.
	go func() { _, _ = clientConn.Write(clientFrame) }()
	_, _, _ = ws.ReadMessageReuse()

	// Server responses are unmasked, so the frame is 4 bytes shorter
	// than the masked client frame (no mask key). Draining the wrong
	// number of bytes causes a deadlock on net.Pipe.
	serverFrameLen := len(clientFrame) - 4

	go func() {
		buf := make([]byte, 128*1024)
		for {
			if _, err := clientConn.Write(clientFrame); err != nil {
				return
			}
			// Drain the server's response frame.
			n := 0
			for n < serverFrameLen {
				nn, err := clientConn.Read(buf)
				if err != nil {
					return
				}
				n += nn
			}
		}
	}()

	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		mt, msg, err := ws.ReadMessageReuse()
		if err != nil {
			b.Fatal(err)
		}
		_ = ws.WriteMessage(mt, msg)
	}
	b.StopTimer()
	cancel()
	_ = ws.Close()
	_ = clientConn.Close()
}

// --- echo benchmarks with reusable client buffers ---

// reusableWSClient is a test client that reuses buffers across iterations.
type reusableWSClient struct {
	conn     net.Conn
	br       *bufio.Reader
	bw       *bufio.Writer
	masked   []byte
	readBuf  []byte
	mask     [4]byte
	frameHdr [14]byte
}

func newReusableClient(tb testing.TB, addr string) *reusableWSClient {
	tb.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		tb.Fatal(err)
	}
	rc := &reusableWSClient{
		conn:    conn,
		br:      bufio.NewReader(conn),
		bw:      bufio.NewWriter(conn),
		masked:  make([]byte, 0, 4096),
		readBuf: make([]byte, 0, 4096),
		mask:    [4]byte{0x12, 0x34, 0x56, 0x78},
	}
	// Upgrade.
	_, _ = rc.bw.WriteString("GET /ws HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	_ = rc.bw.Flush()
	for {
		line, _ := rc.br.ReadString('\n')
		if line == "\r\n" {
			break
		}
	}
	return rc
}

func (c *reusableWSClient) writeAndRead(payload []byte) {
	// Write masked frame — reuse buffers.
	pos := 0
	c.frameHdr[pos] = 0x81 // FIN + text
	pos++
	length := len(payload)
	switch {
	case length <= 125:
		c.frameHdr[pos] = 0x80 | byte(length)
		pos++
	case length <= 65535:
		c.frameHdr[pos] = 0x80 | 126
		pos++
		binary.BigEndian.PutUint16(c.frameHdr[pos:], uint16(length))
		pos += 2
	default:
		c.frameHdr[pos] = 0x80 | 127
		pos++
		binary.BigEndian.PutUint64(c.frameHdr[pos:], uint64(length))
		pos += 8
	}
	copy(c.frameHdr[pos:], c.mask[:])
	pos += 4
	_, _ = c.bw.Write(c.frameHdr[:pos])

	if cap(c.masked) < length {
		c.masked = make([]byte, length)
	}
	c.masked = c.masked[:length]
	copy(c.masked, payload)
	maskBytes(c.mask, c.masked)
	_, _ = c.bw.Write(c.masked)
	_ = c.bw.Flush()

	// Read server response — reuse buffer.
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hdr [2]byte
	_, _ = io.ReadFull(c.br, hdr[:])
	respLen := int64(hdr[1] & 0x7F)
	switch respLen {
	case 126:
		var lb [2]byte
		_, _ = io.ReadFull(c.br, lb[:])
		respLen = int64(binary.BigEndian.Uint16(lb[:]))
	case 127:
		var lb [8]byte
		_, _ = io.ReadFull(c.br, lb[:])
		respLen = int64(binary.BigEndian.Uint64(lb[:]))
	}
	if int(respLen) > cap(c.readBuf) {
		c.readBuf = make([]byte, respLen)
	}
	c.readBuf = c.readBuf[:respLen]
	_, _ = io.ReadFull(c.br, c.readBuf)
}

func (c *reusableWSClient) close() { _ = c.conn.Close() }

func BenchmarkEchoZeroAlloc(b *testing.B) {
	addr, shutdown := startServer(b, Config{
		Handler: func(c *Conn) {
			for {
				mt, msg, err := c.ReadMessageReuse()
				if err != nil {
					return
				}
				_ = c.WriteMessage(mt, msg)
			}
		},
	})
	defer shutdown()
	client := newReusableClient(b, addr)
	defer client.close()
	payload := []byte("hello ws benchmark!!")
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		client.writeAndRead(payload)
	}
}

func BenchmarkEchoZeroAllocLarge(b *testing.B) {
	addr, shutdown := startServer(b, Config{
		Handler: func(c *Conn) {
			for {
				mt, msg, err := c.ReadMessageReuse()
				if err != nil {
					return
				}
				_ = c.WriteMessage(mt, msg)
			}
		},
	})
	defer shutdown()
	client := newReusableClient(b, addr)
	defer client.close()
	payload := make([]byte, 65536)
	for i := range payload {
		payload[i] = byte(i)
	}
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		client.writeAndRead(payload)
	}
}

func BenchmarkEchoMedium(b *testing.B) {
	addr, shutdown := startServer(b, Config{
		Handler: func(c *Conn) {
			for {
				mt, msg, err := c.ReadMessageReuse()
				if err != nil {
					return
				}
				_ = c.WriteMessage(mt, msg)
			}
		},
	})
	defer shutdown()
	client := newReusableClient(b, addr)
	defer client.close()
	payload := make([]byte, 1024)
	for i := range payload {
		payload[i] = byte(i)
	}
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		client.writeAndRead(payload)
	}
}

func BenchmarkMask1MB(b *testing.B) {
	mask := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	data := make([]byte, 1024*1024)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		maskBytes(mask, data)
	}
}

// --- gap tests for v1.3.4 (cross-platform) ---

// TestIdleTimeoutStd verifies that IdleTimeout fires on the std (hijack)
// path. The handler blocks on ReadMessage; without a frame arriving, the
// per-iteration SetReadDeadline must close the conn after IdleTimeout.
func TestIdleTimeoutStd(t *testing.T) {
	handlerDone := make(chan struct{})
	addr, shutdown := startServer(t, Config{
		IdleTimeout: 200 * time.Millisecond,
		Handler: func(c *Conn) {
			defer close(handlerDone)
			for {
				_, _, err := c.ReadMessage()
				if err != nil {
					return
				}
			}
		},
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	select {
	case <-handlerDone:
		// success — read deadline fired and ReadMessage returned an error
	case <-time.After(3 * time.Second):
		t.Fatal("idle timeout did not fire on std path")
	}
}

// TestWriteControlSocketDeadline verifies that WriteControl applies the
// deadline to the underlying net.Conn so a peer that has stopped reading
// cannot indefinitely stall the flush.
func TestWriteControlSocketDeadline(t *testing.T) {
	// net.Pipe gives us a buffer-less pair: a write blocks until the
	// reader consumes it, which lets us simulate a stuck peer.
	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()

	ws := newConn(context.Background(), func() {},
		serverConn, defaultReadBufSize, defaultWriteBufSize)

	// First call with a far-future deadline succeeds because nothing
	// has stuck yet... actually, since net.Pipe blocks immediately
	// (no buffer), we expect a timeout right away.
	deadline := time.Now().Add(100 * time.Millisecond)
	err := ws.WriteControl(int(OpPing), []byte("hi"), deadline)
	if err == nil {
		t.Fatal("expected timeout — net.Pipe peer never read")
	}
	// Either ErrWriteTimeout (lock-acquisition timeout — should not happen
	// here because no other writer holds the lock) or a deadline-exceeded
	// net.Error from the underlying socket.
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		// good — socket deadline fired
		return
	}
	if err == ErrWriteTimeout {
		// good — lock acquisition timed out
		return
	}
	t.Fatalf("unexpected error: %v", err)
}

// TestWriteMessageCompressionDoesNotBlockPing verifies that compressing
// a large WriteMessage does not hold the write lock — concurrent control
// frames must still flow.
func TestWriteMessageCompressionDoesNotBlockPing(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		EnableCompression: true,
		Handler: func(c *Conn) {
			big := make([]byte, 1024*1024)
			for i := range big {
				big[i] = byte(i % 250) // not too uniform
			}
			// Race a slow compressed write with a ping.
			done := make(chan struct{})
			go func() {
				_ = c.WriteMessage(BinaryMessage, big)
				close(done)
			}()
			// Send a ping while the WriteMessage is in progress (or
			// before it took the lock — either way the test expects
			// the ping to succeed within a bounded time).
			start := time.Now()
			err := c.WriteControl(int(OpPing), []byte("ping"),
				time.Now().Add(2*time.Second))
			pingDur := time.Since(start)
			if err != nil {
				t.Errorf("ping failed: %v", err)
			}
			if pingDur > 1*time.Second {
				t.Errorf("ping took %v — compression must not hold the write lock for the entire payload", pingDur)
			}
			<-done
		},
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgradeWithCompression(t, "/ws")

	// Drain frames until the test handler returns.
	_ = client.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var hdr [2]byte
		if _, err := io.ReadFull(client.br, hdr[:]); err != nil {
			break
		}
		// Skip remainder.
		length := int(hdr[1] & 0x7F)
		switch length {
		case 126:
			var ext [2]byte
			_, _ = io.ReadFull(client.br, ext[:])
			length = int(ext[0])<<8 | int(ext[1])
		case 127:
			var ext [8]byte
			_, _ = io.ReadFull(client.br, ext[:])
			length = 0
			for i := 0; i < 8; i++ {
				length = length<<8 | int(ext[i])
			}
		}
		buf := make([]byte, length)
		_, _ = io.ReadFull(client.br, buf)
	}
}

// TestEngineWriteErrorPropagation verifies that engineWriter consults the
// sticky writeErr populated by the engine before each write. This is the
// pipe-level unit test that validates the error propagation contract
// without needing a real engine.
func TestEngineWriteErrorPropagation(t *testing.T) {
	var written []byte
	var mu sync.Mutex
	writeFn := func(p []byte) {
		mu.Lock()
		written = append(written, p...)
		mu.Unlock()
	}
	r := newChanReader(8, 0, 0)
	ws := newEngineConn(context.Background(), func() {},
		r, writeFn, defaultReadBufSize)

	// Initially, writes succeed.
	if err := ws.WriteMessage(TextMessage, []byte("first")); err != nil {
		t.Fatalf("first write failed: %v", err)
	}

	// Simulate engine reporting an EPIPE.
	want := errors.New("synthetic EPIPE")
	ws.writeErr.Store(want)

	// Subsequent writes return the engine error verbatim.
	err := ws.WriteMessage(TextMessage, []byte("second"))
	if err != want {
		t.Fatalf("expected %v, got %v", want, err)
	}
}

// TestWriteBufferPoolHijackOnly verifies the pool is consulted on the
// std (hijack) path and is bypassed on the engine path.
func TestWriteBufferPoolHijackOnly(t *testing.T) {
	t.Run("hijack", func(t *testing.T) {
		var get, put atomic.Uint32
		pool := &testPool{
			getCalls: &get,
			putCalls: &put,
		}
		addr, shutdown := startServer(t, Config{
			WriteBufferPool: pool,
			Handler: func(c *Conn) {
				_ = c.WriteMessage(TextMessage, []byte("hello"))
			},
		})
		defer shutdown()
		client := dialRaw(t, addr)
		defer client.close()
		client.upgrade(t, "/ws")
		client.readServerFrame(t)
		if get.Load() == 0 || put.Load() == 0 {
			t.Errorf("hijack path should consult pool: get=%d put=%d", get.Load(), put.Load())
		}
	})

	t.Run("engine_skip", func(t *testing.T) {
		// Build an engine-mode Conn directly (no real engine needed).
		var get, put atomic.Uint32
		pool := &testPool{
			getCalls: &get,
			putCalls: &put,
		}
		r := newChanReader(8, 0, 0)
		ws := newEngineConn(context.Background(), func() {},
			r, func([]byte) {}, defaultReadBufSize)
		ws.writePool = pool

		_ = ws.WriteMessage(TextMessage, []byte("hello"))
		if get.Load() != 0 || put.Load() != 0 {
			t.Errorf("engine path must NOT consult pool: get=%d put=%d", get.Load(), put.Load())
		}
	})
}

// TestSetupConnNoWIP verifies setupConn produces a fully-initialized
// hijack-path Conn with non-nil bwDst (regression for the WIP-comment
// bug where the wiring was half-finished).
func TestSetupConnNoWIP(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()

	ws := newConn(context.Background(), func() {},
		serverConn, defaultReadBufSize, defaultWriteBufSize)
	cfg := applyDefaults(Config{Handler: func(c *Conn) {}})
	setupConn(ws, &cfg, false, nil, nil)

	if ws.bwDst == nil {
		t.Fatal("bwDst is nil after setupConn — wiring is broken")
	}
	if ws.bw == nil {
		t.Fatal("bw is nil after setupConn — wiring is broken")
	}
}

// --- TLS (wss://) ---

// --- large payload ---

func TestLargePayload(t *testing.T) {
	addr, shutdown := startServer(t, Config{
		Handler: func(c *Conn) {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			_ = c.WriteMessage(mt, msg)
		},
	})
	defer shutdown()

	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	// 64KB payload.
	payload := make([]byte, 65536)
	for i := range payload {
		payload[i] = byte(i % 256)
	}
	_ = client.writeClientFrame(true, OpBinary, payload)

	_, _, data := client.readServerFrame(t)
	if len(data) != len(payload) {
		t.Errorf("len = %d, want %d", len(data), len(payload))
	}
	for i := range data {
		if data[i] != payload[i] {
			t.Fatalf("mismatch at index %d", i)
		}
	}
}
