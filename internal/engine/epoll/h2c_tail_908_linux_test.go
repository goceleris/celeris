//go:build linux

package epoll

import (
	"bufio"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// celeris#884 item 37 / #885 item 16: drainRead's direct-body block hands the
// bytes that follow a body received straight into H1State.bodyBuf back to
// ProcessH1 (the "tail"). When those bytes are an h2c upgrade request, the
// tail's ProcessH1 returns ErrUpgradeH2C after queueing the 101; the block
// used to treat every tail verdict as an error, flush the 101 and close,
// where the verdict from DispatchBufferedBody itself switches the conn to
// HTTP/2. The client got its 101 and then a dead conn.
//
// The scenario needs the direct-body path to run for the second request: a
// request whose body is still arriving when drainRead reads (NextRecvBuf is
// non-nil), and a read that lands after the body's end, so the buffer it reads
// into is larger than the body still owed. bodyBuf keeps its capacity across
// the requests of a conn (internal/conn/h1.go), so a 20000-byte POST first
// leaves room for a 2000-byte one whose last write carries the rest of its
// body and the upgrade request.

// waitServerRead908 waits until the engine's byte counter reaches want. The
// counter is flushed after the loop has handled the bytes of an iteration, so
// once it has moved the previous write has been through drainRead: the next
// write is a separate read, not coalesced into the one before it. It replaces
// a sleep between the two writes, which on a loaded host may not cover the
// first read.
func waitServerRead908(t *testing.T, e *Engine, want uint64) {
	t.Helper()
	var got uint64
	for dl := time.Now().Add(10 * time.Second); time.Now().Before(dl); time.Sleep(2 * time.Millisecond) {
		if got = e.Metrics().BytesRead; got >= want {
			return
		}
	}
	t.Fatalf("the engine counted %d of %d bytes read after 10s", got, want)
}

// postHead908 is the head of a POST /path with a body of n bytes.
func postHead908(path string, n int) string {
	return "POST " + path + " HTTP/1.1\r\nHost: x\r\nContent-Length: " + strconv.Itoa(n) + "\r\n\r\n"
}

// testTailUpgrade908 runs the scenario against the engine e: the client must
// get the 200 of the second POST, then the 101, then working HTTP/2.
func testTailUpgrade908(t *testing.T, e *Engine) {
	t.Helper()
	c, err := net.DialTimeout("tcp", e.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	br := bufio.NewReader(c)
	base := e.Metrics().BytesRead
	sent := uint64(0)
	write := func(s string) {
		t.Helper()
		if _, err := c.Write([]byte(s)); err != nil {
			t.Fatalf("write: %v", err)
		}
		sent += uint64(len(s))
	}

	// A large body first, so that bodyBuf has room for the next one. The
	// body arrives in two reads.
	big := strings.Repeat("x", 20000)
	write(postHead908("/p", len(big)) + big[:10000])
	waitServerRead908(t, e, base+sent)
	write(big[10000:])
	if code, body, err := readBody543(c, br, 3*time.Second); err != nil || code != 200 || body != "/p" {
		t.Fatalf("the large POST: %d %q %v", code, body, err)
	}

	// The small POST: its head and half its body in one read, the other half
	// of its body and the upgrade request in the next, which is received
	// straight into bodyBuf and runs past the body's end into the upgrade
	// request.
	small := strings.Repeat("y", 2000)
	write(postHead908("/q", len(small)) + small[:1000])
	waitServerRead908(t, e, base+sent)
	write(small[1000:] + h2cUpgradeHead908())
	if code, body, err := readBody543(c, br, 3*time.Second); err != nil || code != 200 || body != "/q" {
		t.Fatalf("the small POST: %d %q %v", code, body, err)
	}
	_ = c.SetReadDeadline(time.Now().Add(6 * time.Second))
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, " 101 ") {
		t.Fatalf("the upgrade request pipelined behind a direct-body request: status %q, err %v, want a 101", status, err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read the 101 head: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	// The 101 alone is not the upgrade: the conn must go on as HTTP/2 (the
	// defect sent the 101 and closed).
	h2Streams908(t, c, br)
}

func TestEpollH2CUpgradeAfterDirectBodyTail908(t *testing.T) {
	e, _ := start908(t, false, true)
	testTailUpgrade908(t, e)
}

// TestEpollH2CUpgradeAfterDirectBodyTailAsync908 is the same client script on
// an AsyncHandlers loop. There drainRead reads into cs.buf only (the direct
// body recv is gated on !l.async), so this arm does not reach the tail the
// sync test covers (it passes on main): it guards the upgrade on the async
// path.
func TestEpollH2CUpgradeAfterDirectBodyTailAsync908(t *testing.T) {
	e, _ := start908(t, true, true)
	testTailUpgrade908(t, e)
}
