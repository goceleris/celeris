//go:build linux

package websocket

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
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
	"github.com/goceleris/celeris/middleware/requestid"
	"github.com/goceleris/celeris/probe"
)

// TestConnCapturedRequestValuesSurviveFrames pins celeris#714, and the
// request ID the requestid middleware stores in the std context
// (EnableStdContext), which the Conn keeps through Conn.Context().
//
// On epoll and io_uring every request string (path, query, header) is a view
// of the engine's receive buffer. After the upgrade the engine keeps
// receiving into that buffer, now WebSocket frames, so any request string the
// Conn kept as a view is overwritten by the first frame. Conn.Query did keep
// views, so it returned the frame's bytes or "" once a message had arrived.
// The requestid middleware stored its view of X-Request-Id in the context
// the Conn's context derives from, so requestid.FromStdContext(c.Context())
// did the same.
//
// The handler reads Query, Header and the request ID and sends them to the
// client. Only then does the client send a frame longer than its whole
// upgrade request, so every byte of the request is overwritten, and the
// handler reads them again. The first reading is taken before any frame
// exists, so it is the control that the values were right to begin with.
// Header is a control too: the header slice is cloned in place by
// Context.Detach, so it must pass with or without the fixes.
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
	if ok, p := c714ProbeIOUring(); ok {
		arms = append(arms, arm{"io_uring", celeris.IOUring, false}, arm{"io_uring-async", celeris.IOUring, true})
	} else if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
		t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids dropping the io_uring arms", p.IOUringTier, p.KernelVersion)
	} else {
		t.Logf("io_uring tier=%s kernel=%s: io_uring arms not run", p.IOUringTier, p.KernelVersion)
	}

	for _, a := range arms {
		t.Run(a.name, func(t *testing.T) {
			addr, stop := startC714WSServer(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async})
				srv.GET("/ws", requestid.New(requestid.Config{EnableStdContext: true}), New(Config{
					CheckOrigin: func(*celeris.Context) bool { return true },
					Handler: func(c *Conn) {
						// The first reading goes out before the client
						// sends any frame.
						if err := c.WriteMessage(TextMessage, []byte(capturedView(c))); err != nil {
							return
						}
						if _, _, err := c.ReadMessage(); err != nil {
							return
						}
						_ = c.WriteMessage(TextMessage, []byte(capturedView(c)))
						_, _, _ = c.ReadMessage()
					},
				}))
				return srv
			})
			defer stop()

			const perEncoding = 50
			var wrong [3][2]int // [query|header|request ID][before a frame|after a frame]
			var samples []string
			total := 0
			for _, pct := range []bool{false, true} {
				for i := 0; i < perEncoding; i++ {
					total++
					cid := strconv.Itoa(100000 + total)
					before, after := roundTripCaptured(t, addr, cid, pct)
					want := splitView(expectedView(cid))
					for phase, got := range []string{before, after} {
						g := splitView(got)
						for part := range want {
							if g[part] != want[part] {
								wrong[part][phase]++
							}
						}
						if g != want && len(samples) < 4 {
							samples = append(samples, fmt.Sprintf("pct=%v phase=%d want %q got %q", pct, phase, want, g))
						}
					}
				}
			}
			t.Logf("C714 arm=%s conns=%d query_wrong_before=%d query_wrong_after=%d header_wrong_before=%d header_wrong_after=%d request_id_wrong_before=%d request_id_wrong_after=%d",
				a.name, total, wrong[0][0], wrong[0][1], wrong[1][0], wrong[1][1], wrong[2][0], wrong[2][1])
			for part, what := range []string{
				"Conn.Query does not return the upgrade request's query",
				"Conn.Header does not return the upgrade request's header",
				"requestid.FromStdContext(Conn.Context()) does not return the upgrade request's X-Request-Id",
			} {
				if wrong[part][0]+wrong[part][1] > 0 {
					t.Errorf("%s: wrong before a frame %d/%d, after a frame %d/%d; samples %q",
						what, wrong[part][0], total, wrong[part][1], total, samples)
				}
			}
		})
	}
}

// capturedView renders what the Conn captured at upgrade: two query values
// (one under a plain key, one under a %-encoded key), one header, and the
// request ID in the Conn's context.
func capturedView(c *Conn) string {
	return "q:cid=" + c.Query("cid") + ",k ey=" + c.Query("k ey") + "|h:x-cid=" + c.Header("x-cid") +
		"|r:rid=" + requestid.FromStdContext(c.Context())
}

func expectedView(cid string) string {
	return "q:cid=" + cid + ",k ey=v" + cid + "|h:x-cid=" + cid + "|r:rid=rid" + cid
}

// splitView splits a view into its query, header and request ID parts.
func splitView(s string) [3]string {
	var out [3]string
	for i := range out {
		part, rest, _ := strings.Cut(s, "|")
		out[i], s = part, rest
	}
	return out
}

// roundTripCaptured upgrades one connection, reads the handler's first view,
// then sends one masked text frame longer than the whole upgrade request and
// reads the second view.
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
		"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nX-Cid: " + cid + "\r\nX-Request-Id: rid" + cid +
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
	before = readTextPayload(t, br)
	if _, err := conn.Write(maskedTextFrame(len(req) + 64)); err != nil {
		t.Fatal(err)
	}
	after = readTextPayload(t, br)
	_, _ = conn.Write([]byte{0x88, 0x80, 1, 2, 3, 4}) // masked close, empty payload
	return before, after
}

// readTextPayload reads one unmasked server frame and returns its payload.
func readTextPayload(t *testing.T, br *bufio.Reader) string {
	t.Helper()
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
	return string(payload)
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

// startC714WSServer starts the server mk builds on a fresh loopback listener
// and returns its address and a shutdown closure.
//
// An io_uring start that fails only with ENOMEM is retried, with a new
// server, for up to 10 s. The kernel charges ring memory to RLIMIT_MEMLOCK
// per UID and gives it back 12-23 ms after a ring closes
// (engine/iouring/ring_budget_linux_test.go), so at the CI runner's 8 MiB a
// start made right after the previous arm stopped, or while another
// package's test binary holds rings, can fail although nothing leaked.
func startC714WSServer(t *testing.T, mk func() *celeris.Server) (string, func()) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for tries := 1; ; tries++ {
		s := mk()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
		addr, err := c714WaitReady(s, done)
		if err == nil {
			if tries > 1 {
				t.Logf("server start retried on ring ENOMEM: %d tries", tries)
			}
			return addr, func() { cancel(); <-done }
		}
		cancel()
		_ = ln.Close()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		t.Fatalf("server did not start: %v", err)
	}
}

// c714WaitReady waits until s accepts connections, or its start returns.
func c714WaitReady(s *celeris.Server, done <-chan error) (string, error) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			if err == nil {
				err = errors.New("start returned before the server was ready")
			}
			return "", err
		default:
		}
		if a := s.Addr(); a != nil {
			if c, err := net.DialTimeout("tcp", a.String(), 100*time.Millisecond); err == nil {
				_ = c.Close()
				return a.String(), nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "", errors.New("server not ready within 30s")
}

// c714ProbeIOUring probes the kernel's io_uring support. With
// CELERIS_REQUIRE_IOURING_WORKERS=1 a probe that finds no usable ring is
// retried for up to 10 s before the io_uring arms count as missing: the
// probe's ring can fail with ENOMEM against RLIMIT_MEMLOCK while the rings
// of engines stopped moments ago, or of another test binary run by the same
// user, are still charged (engine/iouring/ring_budget_linux_test.go).
func c714ProbeIOUring() (usable bool, p celerisengine.CapabilityProfile) {
	p = probe.Probe()
	usable = p.IOUringTier >= celerisengine.High && p.ProvidedBuffers
	if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") != "1" {
		return usable, p
	}
	for deadline := time.Now().Add(10 * time.Second); !usable && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		p = probe.Probe()
		usable = p.IOUringTier >= celerisengine.High && p.ProvidedBuffers
	}
	return usable, p
}
