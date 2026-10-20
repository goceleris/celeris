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
// ReadTimeout 1 s, WriteTimeout 30 s, a keep-alive response of 40 MiB that the
// client takes at about 16 MiB/s, steadily, over more than twice ReadTimeout;
// then, a third of a second after the last byte, a second request on the same
// conn, which the conn must still be there to answer (the idle clock after a
// response starts at its end, not at the request before it).
func TestEngineKeepAliveResponseSurvivesReadTimeoutWhileTheClientReads885(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "async"}[async], func(t *testing.T) {
			const body = 40 << 20
			_, addr := startE2E885(t, async, newPathHandler885(body), func(c *resource.Config) {
				c.ReadTimeout = time.Second
				c.WriteTimeout = 30 * time.Second
				c.IdleTimeout = -1
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
				// Paced to 16 MiB/s by the clock, not by a sleep per read.
				if d := time.Until(start.Add(time.Duration(float64(got) / (16 << 20) * float64(time.Second)))); d > 0 {
					time.Sleep(d)
				}
			}
			took := time.Since(start)
			t.Logf("client received %d of %d body bytes in %v with ReadTimeout 1s (end: %v)", got, body, took.Round(time.Millisecond), rerr)
			if got != body {
				t.Fatalf("the client received %d of %d body bytes %v in, reading steadily: the keep-alive response was cut by a "+
					"ReadTimeout of 1s measured from the request (celeris#885 item 35)", got, body, took.Round(time.Millisecond))
			}
			if took < 2*time.Second {
				t.Fatalf("celeris885 PREMISE: the transfer took %v, not longer than twice ReadTimeout", took)
			}
			time.Sleep(300 * time.Millisecond)
			get885(t, c, "/small")
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			resp2, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("a second request 300 ms after the response ended, on the same conn: %v (the conn was reaped on a ReadTimeout "+
					"measured from the request before the response, not from its end; celeris#885 item 35)", err)
			}
			b, _ := io.ReadAll(resp2.Body)
			if string(b) != "small" {
				t.Errorf("second response body %q, want small", b)
			}
		})
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
