//go:build linux

package iouring

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
)

// celeris#884 item 37 / #885 item 16 (the io_uring half): handleRecv's direct-body block hands the
// bytes that follow a body received straight into H1State.bodyBuf back to
// ProcessH1 (the "tail"). When those bytes are an h2c upgrade request, the
// tail's ProcessH1 returns ErrUpgradeH2C after queueing the 101; the block
// used to treat every tail verdict as an error, close without sending the preface,
// where the verdict from DispatchBufferedBody itself switches the conn to
// HTTP/2. The client got its 101 and then a dead conn.
//
// The scenario needs the direct-body path to run for the second request: a
// request whose body is still arriving when drainRead reads (NextRecvBuf is
// non-nil, so pickRecvTarget aims the recv at bodyBuf), and a read that lands after the body's end, so the buffer it reads
// into is larger than the body still owed. bodyBuf keeps its capacity across
// the requests of a conn (internal/conn/h1.go), so a 20000-byte POST first
// leaves room for a 2000-byte one whose last write carries the rest of its
// body and the upgrade request.

// waitServerRead908 waits until the engine's byte counter reaches want. The
// counter is flushed after the worker has handled the CQEs of an iteration, so
// once it has moved the previous write has been through handleRecv: the next
// write is a separate recv, not coalesced into the one before it. It replaces
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
// get the 200 of the second POST, then the 101, then working HTTP/2. With
// linked set (a single-shot recv, where flushSendLink chains a recv behind
// the send that carries the 101), it also checks that the upgrade arm does
// not arm a second recv beside the chained one: that is the job of the
// `!cs.recvLinked` guard, and prepareRecv declines (and counts the decline in
// RecvArmDeclined) a second arm while the first is outstanding, so it is the
// guard that keeps the counter still. A conn that took the arm twice would
// have split the H2 frames across two recvs (celeris#484, the flaky
// TestH2CUpgradeSubsequentStreams).
func testTailUpgrade908(t *testing.T, e *Engine, linked bool) {
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
	m0 := e.Metrics()
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
	if linked {
		// The client has the 101, so handleRecv has returned from the
		// upgrade arm: its arm decision is made. The chained recv is counted
		// in a per-iteration batch, so give it a moment.
		var m engine.EngineMetrics
		for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); time.Sleep(2 * time.Millisecond) {
			if m = e.Metrics(); m.RecvLinkedArms > m0.RecvLinkedArms {
				break
			}
		}
		if m.RecvLinkedArms <= m0.RecvLinkedArms {
			t.Fatalf("the 101 did not go out chained to a recv (RecvLinkedArms %d -> %d): the guard is not exercised", m0.RecvLinkedArms, m.RecvLinkedArms)
		}
		if m.RecvArmDeclined != m0.RecvArmDeclined {
			t.Errorf("the upgrade arm asked for a second recv beside the chained one: RecvArmDeclined %d -> %d, want no change", m0.RecvArmDeclined, m.RecvArmDeclined)
		}
	}
	// The 101 alone is not the upgrade: the conn must go on as HTTP/2 (the
	// defect sent the 101 and closed).
	h2Streams908(t, c, br)
}

// singleShot908 starts the engine with the single-shot recv, whatever the
// environment says: the direct-body recv is aimed by pickRecvTarget only
// when no provided-buffer ring is in use (CELERIS_IOURING_MULTISHOT_RECV=1
// builds one), so a test of that block must not depend on the variable and
// must not skip when it is set.
func singleShot908(t *testing.T) {
	t.Helper()
	t.Setenv("CELERIS_IOURING_MULTISHOT_RECV", "0")
}

func TestIouringH2CUpgradeAfterDirectBodyTail908(t *testing.T) {
	singleShot908(t)
	e, _ := start908(t, false, true)
	testTailUpgrade908(t, e, true)
}

// TestIouringH2CUpgradeAfterDirectBodyTailAsync908 is the same client script
// on an AsyncHandlers engine. A fixed-length body split across recvs does not
// promote the conn (HasPendingDispatchState excludes it: its continuation is
// non-async by construction), so the conn is still read inline-first and its
// second recv goes into bodyBuf: the same tail as the sync test.
func TestIouringH2CUpgradeAfterDirectBodyTailAsync908(t *testing.T) {
	singleShot908(t)
	e, _ := start908(t, true, true)
	testTailUpgrade908(t, e, true)
}

// TestIouringH2CUpgradeAfterDirectBodyTailMultishot908 is the script on an
// engine whose workers run the provided-buffer ring
// (CELERIS_IOURING_MULTISHOT_RECV=1): the direct-body recv is never taken
// there, so it covers the multishot path's handling of the same bytes.
func TestIouringH2CUpgradeAfterDirectBodyTailMultishot908(t *testing.T) {
	t.Setenv("CELERIS_IOURING_MULTISHOT_RECV", "1")
	e, _ := start908(t, false, true)
	testTailUpgrade908(t, e, false)
}

// TestIouringH2CUpgradeAfterDirectBodyTailBehindUnreadResponse908: the
// upgrade verdict comes while the conn's previous response is still being
// sent (the client has read none of it), so the 101 cannot be sent chained to
// a recv of its own (flushSendLink does not link behind a send in flight) and
// the recv that the direct-body CQE consumed must be replaced by the upgrade
// path itself, or the conn is deaf to the client's HTTP/2 preface. The client
// then reads everything and speaks HTTP/2.
func TestIouringH2CUpgradeAfterDirectBodyTailBehindUnreadResponse908(t *testing.T) {
	singleShot908(t)
	e, _ := start908(t, false, true)
	d := net.Dialer{Timeout: 3 * time.Second, Control: func(_, _ string, rc syscall.RawConn) error {
		return rc.Control(func(fd uintptr) { _ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 4096) })
	}}
	c, err := d.Dial("tcp", e.Addr().String())
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
	// The large POST widens bodyBuf and is answered with the large /mid
	// response, which the client does not read.
	big := strings.Repeat("x", 20000)
	write(postHead908("/mid", len(big)) + big[:10000])
	waitServerRead908(t, e, base+sent)
	write(big[10000:])
	waitPipeSettled908(t, c) // the response is on its way and the pipe is full
	small := strings.Repeat("y", 2000)
	write(postHead908("/q", len(small)) + small[:1000])
	waitServerRead908(t, e, base+sent)
	write(small[1000:] + h2cUpgradeHead908())

	_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read the /mid response head: %v", err)
	}
	if n, err := io.Copy(io.Discard, resp.Body); err != nil || n != mid908 {
		t.Fatalf("read %d of %d body bytes of /mid: %v", n, mid908, err)
	}
	_ = resp.Body.Close()
	if code, body, err := readBody543(c, br, 5*time.Second); err != nil || code != 200 || body != "/q" {
		t.Fatalf("the small POST: %d %q %v", code, body, err)
	}
	_ = c.SetReadDeadline(time.Now().Add(6 * time.Second))
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, " 101 ") {
		t.Fatalf("the upgrade request got status %q, err %v, want a 101", status, err)
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
	h2Streams908(t, c, br)
}
