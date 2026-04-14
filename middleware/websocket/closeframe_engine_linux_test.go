//go:build linux

package websocket

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// TestNativeEngineCloseFrameEcho exercises the full WebSocket close
// handshake across every valid close code Autobahn fuzzingclient covers
// in section 7.7 (close code 1000–1011, 3000–4999). For each case the
// client sends a close frame, expects the server to echo the same code
// back, and then expects the server to drop the TCP connection — the
// latter is the RFC 6455 §7.1.1 requirement ("the server MUST close the
// underlying TCP connection immediately") that Autobahn encodes as
// requireClean=True. Without a prompt TCP close after the handshake
// the full fuzzingclient suite hangs on 7.7.13 under the engine path.
//
// Running this focused test takes ~0.5s on the engine path; the full
// fuzzingclient suite takes ~20 minutes, making this the bisect-speed
// harness for close-path regressions.
func TestNativeEngineCloseFrameEcho(t *testing.T) {
	cases := []struct {
		name string
		code int
	}{
		{"7.7.1-1000", 1000},
		{"7.7.2-1001", 1001},
		{"7.7.3-1002", 1002},
		{"7.7.4-1003", 1003},
		{"7.7.5-1007", 1007},
		{"7.7.6-1008", 1008},
		{"7.7.7-1009", 1009},
		{"7.7.8-1010", 1010},
		{"7.7.9-1011", 1011},
		{"7.7.10-3000", 3000},
		{"7.7.11-3999", 3999},
		{"7.7.12-4000", 4000},
		{"7.7.13-4999", 4999},
	}

	for _, kind := range engineKinds(t) {
		kind := kind
		t.Run(kind.String(), func(t *testing.T) {
			addr, shutdown := startNativeServer(t, kind, Config{
				Handler: func(c *Conn) {
					for {
						mt, msg, err := c.ReadMessageReuse()
						if err != nil {
							return
						}
						if err := c.WriteMessage(mt, msg); err != nil {
							return
						}
					}
				},
			})
			defer shutdown()

			for _, tc := range cases {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					assertCloseEchoAndTCPClose(t, addr, tc.code)
				})
			}
		})
	}
}

// assertCloseEchoAndTCPClose drives a single WS close-handshake round trip
// against addr: opens a fresh TCP connection, upgrades, sends a close
// frame with `code`, and asserts (a) the server echoes the same code back
// within 2s and (b) the server drops the TCP connection within 3s of the
// echo.
func assertCloseEchoAndTCPClose(t *testing.T, addr string, code int) {
	t.Helper()
	client := dialRaw(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	payload := make([]byte, 2+len("ws-close-test"))
	binary.BigEndian.PutUint16(payload, uint16(code))
	copy(payload[2:], "ws-close-test")

	if err := client.writeClientFrame(true, OpClose, payload); err != nil {
		t.Fatalf("write close(%d): %v", code, err)
	}

	_ = client.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	fin, op, echo := client.readServerFrame(t)
	if !fin {
		t.Fatalf("close(%d): echo frame not FIN", code)
	}
	if op != OpClose {
		t.Fatalf("close(%d): got opcode %d, want OpClose", code, op)
	}
	if len(echo) < 2 {
		t.Fatalf("close(%d): echo payload too short (%d bytes)", code, len(echo))
	}
	gotCode := int(binary.BigEndian.Uint16(echo[:2]))
	if gotCode != code {
		t.Fatalf("close(%d): echo code %d, want %d (payload=%q)", code, gotCode, code, echo[2:])
	}

	_ = client.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var buf [1]byte
	_, rerr := io.ReadFull(client.br, buf[:])
	if rerr == nil {
		t.Fatalf("close(%d): expected TCP EOF after echo, got a byte", code)
	}
	if !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
		if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
			t.Fatalf("close(%d): server did not close TCP within 3s of echo", code)
		}
	}
}
