//go:build linux

package iouring

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// celeris#722: on an async route, the worker read cs.h1State after it had
// handed the connection to its dispatch goroutine, and that goroutine sets
// cs.h1State to nil when the request is an h2c upgrade (switchToH2Local). The
// two sites are the header-timer re-arm in promoteConnToAsync (the first
// request of a connection) and the same re-arm in handleRecv's async feed (a
// later recv of a promoted connection). Each test below drives one of them to
// an h2c upgrade; under -race the unordered read and write fail the test.

// asyncEveryRouteHandler serves every request with "ok" and marks every route
// async, so a connection's first request promotes it to a dispatch goroutine.
type asyncEveryRouteHandler struct{}

func (asyncEveryRouteHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}
func (asyncEveryRouteHandler) RouteAsync(_, _ string) bool { return true }
func (asyncEveryRouteHandler) HasAsyncRoutes() bool        { return true }

func startAsyncH2CEngine722(t *testing.T) string {
	t.Helper()
	_, addr := startFDLEngine(t, asyncEveryRouteHandler{}, func(c *resource.Config) {
		c.AsyncHandlers = true
		c.EnableH2Upgrade = true
	})
	return addr
}

// h2cUpgradeHead722 is an h2c upgrade request head. body is the Content-Length
// it declares (0 for none).
func h2cUpgradeHead722(method string, body int) string {
	settings := base64.RawURLEncoding.EncodeToString([]byte{0, 3, 0, 0, 0, 100, 0, 4, 0, 0, 255, 255})
	h := method + " /u HTTP/1.1\r\nHost: x\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\n" +
		"HTTP2-Settings: " + settings + "\r\n"
	if body > 0 {
		h += "Content-Length: " + strconv.Itoa(body) + "\r\n"
	}
	return h + "\r\n"
}

func h2Frame722(typ, flags byte, streamID uint32, payload []byte) []byte {
	b := make([]byte, 9, 9+len(payload))
	b[0], b[1], b[2] = byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload))
	b[3], b[4] = typ, flags
	binary.BigEndian.PutUint32(b[5:], streamID)
	return append(b, payload...)
}

// finishH2CUpgrade722 reads the 101, sends the client preface, SETTINGS and a
// PING, and reads frames until the PING's ACK: the server has taken the
// connection over as HTTP/2.
func finishH2CUpgrade722(t *testing.T, c net.Conn, br *bufio.Reader) {
	t.Helper()
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, " 101 ") {
		t.Fatalf("upgrade status %q, err %v; want 101", status, err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read 101 head: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	out := []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
	out = append(out, h2Frame722(0x4, 0, 0, nil)...)
	out = append(out, h2Frame722(0x6, 0, 0, []byte("PING722!"))...)
	if _, err := c.Write(out); err != nil {
		t.Fatalf("write preface: %v", err)
	}
	for {
		var hdr [9]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			t.Fatalf("read frame: %v", err)
		}
		n := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
		if _, err := br.Discard(n); err != nil {
			t.Fatalf("discard frame: %v", err)
		}
		if hdr[3] == 0x6 && hdr[4]&0x1 != 0 {
			return
		}
	}
}

// TestAsyncH2CUpgradeOnPromotionLeavesH1StateToTheGoroutine: the upgrade is
// the connection's first request, so the worker promotes the connection and
// starts its dispatch goroutine with the request stashed (promoteConnToAsync).
// The goroutine runs the upgrade at once; the worker must not read
// cs.h1State after starting it.
func TestAsyncH2CUpgradeOnPromotionLeavesH1StateToTheGoroutine(t *testing.T) {
	addr := startAsyncH2CEngine722(t)
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte(h2cUpgradeHead722("GET", 0))); err != nil {
		t.Fatalf("write upgrade: %v", err)
	}
	finishH2CUpgrade722(t, c, bufio.NewReader(c))
}

// TestAsyncH2CUpgradeOnFeedLeavesH1StateToTheGoroutine: the upgrade request
// carries a body that arrives in two recvs. The first promotes the connection
// and its goroutine parks with the body half read; the second reaches the
// goroutine through handleRecv's async feed, and the goroutine then runs the
// upgrade. The worker must not read cs.h1State after that feed.
func TestAsyncH2CUpgradeOnFeedLeavesH1StateToTheGoroutine(t *testing.T) {
	addr := startAsyncH2CEngine722(t)
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte(h2cUpgradeHead722("POST", 8) + "half")); err != nil {
		t.Fatalf("write upgrade head: %v", err)
	}
	// Long enough for the first recv to be promoted and its goroutine to
	// park on the half-read body; the second write is then a separate recv.
	time.Sleep(100 * time.Millisecond)
	if _, err := c.Write([]byte("body")); err != nil {
		t.Fatalf("write rest of body: %v", err)
	}
	finishH2CUpgrade722(t, c, bufio.NewReader(c))
}
