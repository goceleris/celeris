//go:build linux

package websocket

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/probe"
)

// TestConnCapturedRequestValuesSurviveFrames pins celeris#714.
//
// On epoll and io_uring every request string (path, query, header) is a view
// of the engine's receive buffer. After the upgrade the engine keeps
// receiving into that buffer, now WebSocket frames, so any request string the
// Conn kept as a view is overwritten by the first frame. Conn.Query did keep
// views, so it returned the frame's bytes or "" once a message had arrived.
//
// The client sends a frame longer than its whole upgrade request, so every
// byte of the request is overwritten, and the handler reads Query and Header
// before and after receiving it. Header is here as the control: the header
// slice is materialized in place by Context.Detach, so it must pass with or
// without the Query fix.
func TestConnCapturedRequestValuesSurviveFrames(t *testing.T) {
	type arm struct {
		name   string
		engine celeris.EngineType
		async  bool
	}
	arms := []arm{
		{"std", celeris.Std, false},
		{"epoll", celeris.Epoll, false},
		{"epoll-async", celeris.Epoll, true},
	}
	if p := probe.Probe(); p.IOUringTier >= celerisengine.High && p.ProvidedBuffers {
		arms = append(arms, arm{"io_uring", celeris.IOUring, false}, arm{"io_uring-async", celeris.IOUring, true})
	} else if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
		t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids dropping the io_uring arms", p.IOUringTier, p.KernelVersion)
	} else {
		t.Logf("io_uring tier=%s kernel=%s: io_uring arms not run", p.IOUringTier, p.KernelVersion)
	}

	for _, a := range arms {
		t.Run(a.name, func(t *testing.T) {
			srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async})
			srv.GET("/ws", New(Config{
				CheckOrigin: func(*celeris.Context) bool { return true },
				Handler: func(c *Conn) {
					before := capturedView(c)
					if _, _, err := c.ReadMessage(); err != nil {
						return
					}
					after := capturedView(c)
					_ = c.WriteMessage(TextMessage, []byte(before+"\n"+after))
					_, _, _ = c.ReadMessage()
				},
			}))
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- srv.StartWithListenerAndContext(ctx, ln) }()
			defer func() { cancel(); <-done }()
			addr := waitForReady(t, srv, 30*time.Second)

			const perEncoding = 50
			var wrong [2][2]int // [query|header][before|after]
			var samples []string
			total := 0
			for _, pct := range []bool{false, true} {
				for i := 0; i < perEncoding; i++ {
					total++
					cid := strconv.Itoa(100000 + total)
					before, after := roundTripCaptured(t, addr, cid, pct)
					want := expectedView(cid)
					for phase, got := range []string{before, after} {
						gq, gh := splitView(got)
						wq, wh := splitView(want)
						if gq != wq {
							wrong[0][phase]++
						}
						if gh != wh {
							wrong[1][phase]++
						}
						if got != want && len(samples) < 4 {
							samples = append(samples, fmt.Sprintf("pct=%v phase=%d want %q got %q", pct, phase, want, got))
						}
					}
				}
			}
			t.Logf("C714 arm=%s conns=%d query_wrong_before=%d query_wrong_after=%d header_wrong_before=%d header_wrong_after=%d",
				a.name, total, wrong[0][0], wrong[0][1], wrong[1][0], wrong[1][1])
			if wrong[0][0]+wrong[0][1] > 0 {
				t.Errorf("Conn.Query does not return the upgrade request's query: wrong before a frame %d/%d, after a frame %d/%d; samples %q",
					wrong[0][0], total, wrong[0][1], total, samples)
			}
			if wrong[1][0]+wrong[1][1] > 0 {
				t.Errorf("Conn.Header does not return the upgrade request's header: wrong before a frame %d/%d, after a frame %d/%d; samples %q",
					wrong[1][0], total, wrong[1][1], total, samples)
			}
		})
	}
}

// capturedView renders what the Conn captured at upgrade: two query values
// (one under a plain key, one under a %-encoded key) and one header.
func capturedView(c *Conn) string {
	return "q:cid=" + c.Query("cid") + ",k ey=" + c.Query("k ey") + "|h:x-cid=" + c.Header("x-cid")
}

func expectedView(cid string) string {
	return "q:cid=" + cid + ",k ey=v" + cid + "|h:x-cid=" + cid
}

func splitView(s string) (query, header string) {
	if i := strings.IndexByte(s, '|'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// roundTripCaptured upgrades one connection, sends one masked text frame
// longer than the whole upgrade request, and returns the handler's two views.
func roundTripCaptured(t *testing.T, addr, cid string, pct bool) (before, after string) {
	t.Helper()
	enc := cid
	if pct {
		var b strings.Builder
		for i := 0; i < len(cid); i++ {
			fmt.Fprintf(&b, "%%%02X", cid[i])
		}
		enc = b.String()
	}
	req := "GET /ws?cid=" + enc + "&k%20ey=v" + cid + " HTTP/1.1\r\nHost: " + addr +
		"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nX-Cid: " + cid +
		"\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, " 101 ") {
		t.Fatalf("upgrade: status %q err %v", status, err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err := conn.Write(maskedTextFrame(len(req) + 64)); err != nil {
		t.Fatal(err)
	}
	var hdr [4]byte
	if _, err := io.ReadFull(br, hdr[:2]); err != nil {
		t.Fatalf("reply header: %v", err)
	}
	n := int(hdr[1] & 0x7f)
	if n == 126 {
		if _, err := io.ReadFull(br, hdr[2:4]); err != nil {
			t.Fatal(err)
		}
		n = int(binary.BigEndian.Uint16(hdr[2:4]))
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(br, payload); err != nil {
		t.Fatalf("reply payload: %v", err)
	}
	_, _ = conn.Write([]byte{0x88, 0x80, 1, 2, 3, 4}) // masked close, empty payload
	before, after, _ = strings.Cut(string(payload), "\n")
	return before, after
}

// maskedTextFrame builds a client text frame of n bytes of 'Z' (n < 65536).
func maskedTextFrame(n int) []byte {
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	out := []byte{0x81}
	if n <= 125 {
		out = append(out, 0x80|byte(n))
	} else {
		out = append(out, 0x80|126, byte(n>>8), byte(n))
	}
	out = append(out, mask[:]...)
	for j := 0; j < n; j++ {
		out = append(out, 'Z'^mask[j%4])
	}
	return out
}
