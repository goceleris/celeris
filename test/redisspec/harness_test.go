//go:build redisspec

package redisspec

import (
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis/protocol"
)

const (
	envAddr     = "CELERIS_REDIS_ADDR"
	envPassword = "CELERIS_REDIS_PASSWORD"

	defaultReadTimeout  = 5 * time.Second
	defaultWriteTimeout = 3 * time.Second
)

// redisRawConn wraps a raw TCP connection with RESP read/write helpers.
type redisRawConn struct {
	conn   net.Conn
	reader *protocol.Reader
	writer *protocol.Writer
	buf    [65536]byte
}

// addrFromEnv returns the Redis address or skips the test.
func addrFromEnv(t *testing.T) string {
	t.Helper()
	addr := strings.TrimSpace(os.Getenv(envAddr))
	if addr == "" {
		t.Skipf("skipping: %s not set", envAddr)
	}
	return addr
}

// passwordFromEnv returns the Redis password or skips the test.
func passwordFromEnv(t *testing.T) string {
	t.Helper()
	pw := os.Getenv(envPassword)
	if pw == "" {
		t.Skipf("skipping: %s not set", envPassword)
	}
	return pw
}

// dialRedis opens a raw TCP connection to the Redis server, verifies it with
// PING, and registers cleanup.
func dialRedis(t *testing.T) *redisRawConn {
	t.Helper()
	addr := addrFromEnv(t)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	rc := &redisRawConn{
		conn:   conn,
		reader: protocol.NewReader(),
		writer: protocol.NewWriter(),
	}
	t.Cleanup(func() { _ = conn.Close() })

	// If auth is configured, authenticate first.
	if pw := os.Getenv(envPassword); pw != "" {
		rc.Send("AUTH", pw)
		v := rc.ReadValue(t)
		if v.Type == protocol.TyError {
			t.Logf("AUTH warning: %s", v.Str)
		}
	}

	return rc
}

// dialRedisRESP3 opens a connection and negotiates RESP3 via HELLO 3.
func dialRedisRESP3(t *testing.T) *redisRawConn {
	t.Helper()
	rc := dialRedis(t)
	rc.Send("HELLO", "3")
	v := rc.ReadValue(t)
	if v.Type == protocol.TyError {
		t.Skipf("server does not support HELLO 3: %s", v.Str)
	}
	// HELLO 3 response should be a map in RESP3.
	if v.Type != protocol.TyMap && v.Type != protocol.TyArray {
		t.Fatalf("HELLO 3: unexpected type %v", v.Type)
	}
	return rc
}

// dialRedisNoAuth opens a raw TCP connection without authentication.
func dialRedisNoAuth(t *testing.T) *redisRawConn {
	t.Helper()
	addr := addrFromEnv(t)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	rc := &redisRawConn{
		conn:   conn,
		reader: protocol.NewReader(),
		writer: protocol.NewWriter(),
	}
	t.Cleanup(func() { _ = conn.Close() })
	return rc
}

// Send writes a RESP command to the connection.
func (c *redisRawConn) Send(args ...string) {
	c.writer.Reset()
	c.writer.AppendCommand(args...)
	_ = c.conn.SetWriteDeadline(time.Now().Add(defaultWriteTimeout))
	_, err := c.conn.Write(c.writer.Bytes())
	if err != nil {
		panic("write: " + err.Error())
	}
}

// SendRaw writes raw bytes to the connection.
func (c *redisRawConn) SendRaw(data []byte) {
	_ = c.conn.SetWriteDeadline(time.Now().Add(defaultWriteTimeout))
	_, err := c.conn.Write(data)
	if err != nil {
		panic("write raw: " + err.Error())
	}
}

// ReadValue reads one RESP value from the connection.
func (c *redisRawConn) ReadValue(t *testing.T) protocol.Value {
	t.Helper()
	for {
		v, err := c.reader.Next()
		if err == nil {
			return v
		}
		if !errors.Is(err, protocol.ErrIncomplete) {
			t.Fatalf("protocol error: %v", err)
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(defaultReadTimeout))
		n, err := c.conn.Read(c.buf[:])
		if err != nil {
			if errors.Is(err, io.EOF) {
				t.Fatal("unexpected EOF from server")
			}
			t.Fatalf("read: %v", err)
		}
		c.reader.Feed(c.buf[:n])
	}
}

// ReadValueTimeout reads one RESP value with a custom timeout.
func (c *redisRawConn) ReadValueTimeout(t *testing.T, timeout time.Duration) (protocol.Value, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		v, err := c.reader.Next()
		if err == nil {
			return v, true
		}
		if !errors.Is(err, protocol.ErrIncomplete) {
			t.Fatalf("protocol error: %v", err)
		}
		_ = c.conn.SetReadDeadline(deadline)
		n, readErr := c.conn.Read(c.buf[:])
		if readErr != nil {
			if ne, ok := readErr.(net.Error); ok && ne.Timeout() {
				return protocol.Value{}, false
			}
			if errors.Is(readErr, io.EOF) {
				return protocol.Value{}, false
			}
			t.Fatalf("read: %v", readErr)
		}
		c.reader.Feed(c.buf[:n])
	}
}
