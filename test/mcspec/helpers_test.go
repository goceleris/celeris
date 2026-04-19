//go:build memcached

// Package mcspec exercises the memcached text and binary wire protocols by
// framing packets directly on a raw TCP socket. It is the memcached analogue
// of the redisspec and pgspec suites and is gated behind the `memcached`
// build tag + CELERIS_MEMCACHED_ADDR env var.
package mcspec

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/memcached/protocol"
)

const (
	envAddr = "CELERIS_MEMCACHED_ADDR"

	defaultReadTimeout  = 5 * time.Second
	defaultWriteTimeout = 3 * time.Second
)

// keySeq gives each test a unique key even when tests run in parallel.
var keySeq uint64

// addrFromEnv returns the memcached address or skips the test.
func addrFromEnv(t *testing.T) string {
	t.Helper()
	addr := strings.TrimSpace(os.Getenv(envAddr))
	if addr == "" {
		t.Skipf("skipping: %s not set", envAddr)
	}
	return addr
}

// uniqueKey returns a collision-free ASCII-safe key for the test. memcached
// disallows whitespace/control bytes inside keys, so we use hex digits and
// underscores only.
func uniqueKey(t *testing.T, label string) string {
	t.Helper()
	n := atomic.AddUint64(&keySeq, 1)
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	label = sanitizeLabel(label)
	return fmt.Sprintf("mcspec_%s_%d_%s", label, n, hex.EncodeToString(b))
}

func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "k"
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Text protocol raw connection
// ---------------------------------------------------------------------------

// textRawConn wraps a net.Conn with a TextReader and read/write helpers that
// map directly onto the memcached text protocol.
type textRawConn struct {
	conn   net.Conn
	reader *protocol.TextReader
	buf    [16384]byte
}

// dialText opens a raw TCP connection for text-protocol tests.
func dialText(t *testing.T) *textRawConn {
	t.Helper()
	addr := addrFromEnv(t)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	rc := &textRawConn{conn: conn, reader: protocol.NewTextReader()}
	t.Cleanup(func() { _ = conn.Close() })
	return rc
}

// WriteRaw writes arbitrary bytes to the connection.
func (c *textRawConn) WriteRaw(data []byte) {
	_ = c.conn.SetWriteDeadline(time.Now().Add(defaultWriteTimeout))
	if _, err := c.conn.Write(data); err != nil {
		panic("write: " + err.Error())
	}
}

// WriteLine writes a line followed by CRLF.
func (c *textRawConn) WriteLine(s string) {
	c.WriteRaw([]byte(s + "\r\n"))
}

// WriteStorage encodes "<cmd> <key> <flags> <exptime> <bytes>\r\n<data>\r\n".
func (c *textRawConn) WriteStorage(cmd, key string, flags uint32, exptime int64, data []byte, casID uint64) {
	w := protocol.NewTextWriter()
	w.AppendStorage(cmd, key, flags, exptime, data, casID, false)
	c.WriteRaw(w.Bytes())
}

// ReadReply reads one logical text-protocol reply from the server. On a short
// read it continues to pull bytes until a complete frame is available.
func (c *textRawConn) ReadReply(t *testing.T) protocol.TextReply {
	t.Helper()
	for {
		reply, err := c.reader.Next()
		if err == nil {
			return reply
		}
		if !errors.Is(err, protocol.ErrIncomplete) {
			t.Fatalf("protocol error: %v", err)
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(defaultReadTimeout))
		n, err := c.conn.Read(c.buf[:])
		if err != nil {
			if errors.Is(err, io.EOF) {
				t.Fatalf("unexpected EOF from server")
			}
			t.Fatalf("read: %v", err)
		}
		c.reader.Feed(c.buf[:n])
	}
}

// ExpectKind asserts that the next reply has the given Kind. Returns the reply
// for follow-up field assertions.
func (c *textRawConn) ExpectKind(t *testing.T, k protocol.Kind) protocol.TextReply {
	t.Helper()
	r := c.ReadReply(t)
	if r.Kind != k {
		t.Fatalf("expected %s, got %s (data=%q)", k, r.Kind, r.Data)
	}
	return r
}

// ---------------------------------------------------------------------------
// Binary protocol raw connection
// ---------------------------------------------------------------------------

// binRawConn wraps a net.Conn with a BinaryReader for binary-protocol tests.
type binRawConn struct {
	conn   net.Conn
	reader *protocol.BinaryReader
	writer *protocol.BinaryWriter
	buf    [16384]byte
	opaque uint32
}

// dialBinary opens a raw TCP connection for binary-protocol tests.
func dialBinary(t *testing.T) *binRawConn {
	t.Helper()
	addr := addrFromEnv(t)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	rc := &binRawConn{
		conn:   conn,
		reader: protocol.NewBinaryReader(),
		writer: protocol.NewBinaryWriter(),
	}
	t.Cleanup(func() { _ = conn.Close() })
	return rc
}

// nextOpaque returns a unique opaque token the caller pairs with its request.
func (c *binRawConn) nextOpaque() uint32 {
	c.opaque++
	return c.opaque
}

// WriteRaw writes raw bytes.
func (c *binRawConn) WriteRaw(data []byte) {
	_ = c.conn.SetWriteDeadline(time.Now().Add(defaultWriteTimeout))
	if _, err := c.conn.Write(data); err != nil {
		panic("write: " + err.Error())
	}
}

// ReadPacket reads one binary-protocol packet.
func (c *binRawConn) ReadPacket(t *testing.T) protocol.BinaryPacket {
	t.Helper()
	for {
		pkt, err := c.reader.Next()
		if err == nil {
			return pkt
		}
		if !errors.Is(err, protocol.ErrIncomplete) {
			t.Fatalf("binary protocol error: %v", err)
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(defaultReadTimeout))
		n, err := c.conn.Read(c.buf[:])
		if err != nil {
			if errors.Is(err, io.EOF) {
				t.Fatalf("unexpected EOF from server")
			}
			t.Fatalf("read: %v", err)
		}
		c.reader.Feed(c.buf[:n])
	}
}

// ExpectStatus asserts that the next packet matches the given opcode and
// status. Returns the packet for follow-up inspection.
func (c *binRawConn) ExpectStatus(t *testing.T, opcode byte, status uint16) protocol.BinaryPacket {
	t.Helper()
	pkt := c.ReadPacket(t)
	if pkt.Header.Magic != protocol.MagicResponse {
		t.Fatalf("expected response magic 0x%02x, got 0x%02x", protocol.MagicResponse, pkt.Header.Magic)
	}
	if pkt.Header.Opcode != opcode {
		t.Fatalf("expected opcode 0x%02x, got 0x%02x", opcode, pkt.Header.Opcode)
	}
	if pkt.Status() != status {
		t.Fatalf("opcode 0x%02x: expected status 0x%04x, got 0x%04x (value=%q)",
			opcode, status, pkt.Status(), pkt.Value)
	}
	return pkt
}
