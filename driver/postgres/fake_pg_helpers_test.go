package postgres

import (
	"encoding/binary"
	"net"
	"testing"
)

// startFakePGBench spins up a minimal TCP listener on a free loopback port
// and hands each accepted connection to handler in its own goroutine. The
// listener is closed via t.Cleanup. Returns host:port.
func startFakePGBench(tb testing.TB, handler func(net.Conn)) string {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handler(c)
		}
	}()
	return ln.Addr().String()
}

// fakePGTrustStartupB reads the startup message, replies with
// AuthenticationOk + BackendKeyData + ReadyForQuery, then dispatches to post.
// Shared between benchmark harnesses and fake-extended test helpers.
func fakePGTrustStartupB(tb testing.TB, c net.Conn, pid, secret int32, post func(net.Conn)) {
	tb.Helper()
	defer func() { _ = c.Close() }()
	lenBuf := make([]byte, 4)
	if _, err := readFull(c, lenBuf); err != nil {
		return
	}
	n := int(binary.BigEndian.Uint32(lenBuf))
	if n < 4 {
		return
	}
	body := make([]byte, n-4)
	if _, err := readFull(c, body); err != nil {
		return
	}
	if err := writeAuthOK(c); err != nil {
		return
	}
	if err := writeBackendKeyData(c, pid, secret); err != nil {
		return
	}
	if err := writeReadyForQuery(c, 'I'); err != nil {
		return
	}
	if post != nil {
		post(c)
	}
}

func readFull(c net.Conn, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		k, err := c.Read(buf[n:])
		if err != nil {
			return n, err
		}
		n += k
	}
	return n, nil
}
