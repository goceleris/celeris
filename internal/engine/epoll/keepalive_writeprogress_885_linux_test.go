//go:build linux

package epoll

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#885 items 35 and 45, end to end on the engine, for a KEEP-ALIVE
// response (no Connection: close). A conn whose response the kernel did not
// take whole is parked on EPOLLOUT (armEpollOut), which removes it from the
// dirty list; checkTimeouts then measured only IdleTimeout and ReadTimeout from
// lastActivity, the last READ, and WriteTimeout not at all.
//
//   - item 35: a client that takes the response steadily, for longer than
//     ReadTimeout, was cut at ReadTimeout from its request.
//   - item 45: a client that takes nothing was not bounded by WriteTimeout (a
//     response of 8, 16 or 48 MiB was sent whole to a client that read nothing
//     for 5 s, with WriteTimeout 1 s).
//
// Both run on a sync conn and on an async one (AsyncHandlers: the response
// reaches the loop through the dispatch goroutine's write hook and the detach
// queue, so the loop flushes it from the dirty pass).

// pathHandler885 answers /big with a fixed body and anything else with "small".
type pathHandler885 struct{ big []byte }

func newPathHandler885(n int) *pathHandler885 {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'X'
	}
	return &pathHandler885{big: b}
}

func (h *pathHandler885) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	body := []byte("small")
	if s.Path == "/big" {
		body = h.big
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "application/octet-stream"}, {"content-length", strconv.Itoa(len(body))}},
		body)
}
func (*pathHandler885) RouteAsync(_, _ string) bool { return true }
func (*pathHandler885) HasAsyncRoutes() bool        { return true }

var _ stream.AsyncRouteResolver = (*pathHandler885)(nil)

// startE2E885 starts an epoll engine on a free loopback port with cfg adjusted
// by mut, and returns it with its address.
func startE2E885(t *testing.T, async bool, h stream.Handler, mut func(*resource.Config)) (*Engine, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg := resource.Config{
		Addr:          addr,
		Protocol:      engine.HTTP1,
		AsyncHandlers: async,
		Resources:     resource.Resources{Workers: 2},
	}
	mut(&cfg)
	e, err := New(cfg, h)
	if err != nil {
		t.Fatalf("epoll engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(30 * time.Second):
		}
	})
	for dl := time.Now().Add(15 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind")
	}
	return e, e.Addr().String()
}

// dialRcvBuf885 dials addr with SO_RCVBUF set before the connect, so the
// advertised window is the small one from the first segment.
func dialRcvBuf885(t *testing.T, addr string, rcvbuf int) net.Conn {
	t.Helper()
	d := net.Dialer{Timeout: 3 * time.Second, Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, rcvbuf)
		})
	}}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func get885(t *testing.T, c net.Conn, path string) {
	t.Helper()
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", path); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestEngineKeepAliveResponseSurvivesReadTimeoutWhileTheClientReads885:
// ReadTimeout 2 s, WriteTimeout 30 s, a keep-alive response of 40 MiB that the
// client takes at about 8 MiB/s, steadily, over more than twice ReadTimeout;
// then, a third of a second after the last byte, a second request on the same
// conn, which the conn must still be there to answer (the idle clock after a
// response starts at its end, not at the request before it).
func TestEngineKeepAliveResponseSurvivesReadTimeoutWhileTheClientReads885(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "async"}[async], func(t *testing.T) {
			// The server's send buffer is bounded, so its last write runs at most
			// 64 KiB (8 ms at the client's pace) ahead of the client's last read,
			// and the second request follows 50 ms after that read: the idle
			// clock has about 1.9 s of its 2 s to spare. With a 1 s ReadTimeout,
			// the default send buffer and a 300 ms gap it had about 550 ms (see
			// the margin test below).
			runKeepAliveSurvives885(t, async, keepAliveSndBuf885, 50*time.Millisecond)
		})
	}
}

// The tests above and below: the server's SO_SNDBUF, the ReadTimeout and the
// client's pace. ReadTimeout is 2 s and the pace 8 MiB/s (40 MiB in 5 s, over
// twice the ReadTimeout as the premise requires), where they were 1 s and
// 16 MiB/s: the margin of the second request is a fraction of ReadTimeout, so a
// stall of the loop thread on a loaded runner has to be twice as long to reap the
// conn.
const (
	keepAliveSndBuf885 = 64 << 10
	keepAliveRead885   = 2 * time.Second
	keepAliveRate885   = 8 << 20 // bytes per second
)

// TestEngineKeepAliveMarginWithABoundedSendBuffer885 holds the margin the test
// above relies on. The idle clock after a response starts when the SERVER
// finished writing it, not when the client finished reading: with the kernel's
// default send buffer (autotuned up to tcp_wmem's maximum, 4 MiB) the server's
// last write runs ahead of the client's last read by the buffer's drain time at
// the client's pace, up to about 250 ms at 16 MiB/s. Measured at --cpus 2 on
// the original test (ReadTimeout 1 s, 16 MiB/s), with the default buffer the
// second request finds the conn alive at a gap of 800 ms after the client's
// last read and reaped at 900 ms (5 of 5 runs each, both arms), so its 300 ms
// gap left about 550 ms of margin, which a loaded runner with coverage and
// -race can eat (the test failed once in CI, in the async arm). With the send
// buffer bounded to 64 KiB the lag is a few milliseconds and the idle clock
// has its full ReadTimeout: a gap of 1.5 s, three quarters of the 2 s, must
// find the conn alive, i.e. the test above (it uses 50 ms) has this much room.
func TestEngineKeepAliveMarginWithABoundedSendBuffer885(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "async"}[async], func(t *testing.T) {
			runKeepAliveSurvives885(t, async, keepAliveSndBuf885, 1500*time.Millisecond)
		})
	}
}

// runKeepAliveSurvives885 is the test above with the server's send buffer
// (SocketSend, 0 = the kernel's default) and the gap before the second request
// as parameters.
func runKeepAliveSurvives885(t *testing.T, async bool, sndbuf int, gap time.Duration) {
	const body = 40 << 20
	_, addr := startE2E885(t, async, newPathHandler885(body), func(c *resource.Config) {
		c.ReadTimeout = keepAliveRead885
		c.WriteTimeout = 30 * time.Second
		c.IdleTimeout = -1
		c.Resources.SocketSend = sndbuf
	})
	c := dialRcvBuf885(t, addr, 64<<10)
	br := bufio.NewReaderSize(c, 512<<10)
	get885(t, c, "/big")
	start := time.Now()
	_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("celeris885: reading the response head %v in: %v", time.Since(start).Round(time.Millisecond), err)
	}
	got := int64(0)
	buf := make([]byte, 512<<10)
	var rerr error
	for rerr == nil {
		var n int
		n, rerr = resp.Body.Read(buf)
		got += int64(n)
		// Paced to keepAliveRate885 by the clock, not by a sleep per read.
		if d := time.Until(start.Add(time.Duration(float64(got) / keepAliveRate885 * float64(time.Second)))); d > 0 {
			time.Sleep(d)
		}
	}
	took := time.Since(start)
	t.Logf("client received %d of %d body bytes in %v with ReadTimeout %v (end: %v)", got, body, took.Round(time.Millisecond), keepAliveRead885, rerr)
	if got != body {
		t.Fatalf("the client received %d of %d body bytes %v in, reading steadily: the keep-alive response was cut by a "+
			"ReadTimeout of %v measured from the request (celeris#885 item 35)", got, body, took.Round(time.Millisecond), keepAliveRead885)
	}
	if took < 2*keepAliveRead885 {
		t.Fatalf("celeris885 PREMISE: the transfer took %v, not longer than twice ReadTimeout", took)
	}
	time.Sleep(gap)
	get885(t, c, "/small")
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp2, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("a second request %v after the response ended, on the same conn: %v (the conn was reaped on a ReadTimeout "+
			"measured from the request before the response, not from its end; celeris#885 item 35)", gap, err)
	}
	b, _ := io.ReadAll(resp2.Body)
	if string(b) != "small" {
		t.Errorf("second response body %q, want small", b)
	}
}

// TestEngineStalledKeepAliveResponseIsBoundedByWriteTimeout885: a client with
// a 4 KiB receive buffer reads nothing for 5 s, WriteTimeout 1 s (ReadTimeout
// and IdleTimeout off, so nothing but WriteTimeout can end the conn). The
// server must have closed it by then, and the client, which starts to read
// afterwards, gets what the kernel had already taken (the server's send buffer,
// 64 KiB, and the client's window) and not the 16 MiB response.
func TestEngineStalledKeepAliveResponseIsBoundedByWriteTimeout885(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "async"}[async], func(t *testing.T) {
			const body = 16 << 20
			e, addr := startE2E885(t, async, newPathHandler885(body), func(c *resource.Config) {
				c.ReadTimeout = -1
				c.IdleTimeout = -1
				c.WriteTimeout = time.Second
				c.Resources.SocketSend = 64 << 10
			})
			c := dialRcvBuf885(t, addr, 4096)
			get885(t, c, "/big")
			gone := time.Duration(0)
			t0 := time.Now()
			for time.Since(t0) < 5*time.Second {
				time.Sleep(20 * time.Millisecond)
				if gone == 0 && time.Since(t0) > 500*time.Millisecond && e.Metrics().ActiveConnections == 0 {
					gone = time.Since(t0)
				}
			}
			active := e.Metrics().ActiveConnections
			// Now read: what is left is what the kernel held at the close.
			_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
			n, _ := io.Copy(io.Discard, c)
			t.Logf("celeris885 STALL async=%v gone_ms=%d active_at_5s=%d received=%d of %d", async, gone.Milliseconds(), active, n, body)
			if active != 0 {
				t.Errorf("the conn of a client that read nothing for 5 s was still open (ActiveConnections=%d) with WriteTimeout 1s: "+
					"a keep-alive response parked on EPOLLOUT is not bounded by WriteTimeout (celeris#885 item 45)", active)
			}
			if n >= body/2 {
				t.Errorf("the client that read nothing for 5 s then received %d of %d response bytes: the server kept the response "+
					"queued past WriteTimeout 1s (celeris#885 item 45)", n, body)
			}
		})
	}
}
