//go:build linux

package websocket

import (
	"bufio"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris"
)

// A WebSocket peer that reads nothing of what it is sent gets the same bound as
// an SSE client (celeris#888 items 38 and 39): the native engines apply
// WriteTimeout to a detached connection as "no progress for WriteTimeout",
// whatever the middleware's idle deadline says. A peer that sends pings keeps
// pushing that deadline out, which is the peer this test is.

const wsStall888Timeout = time.Second

// dialSmallRcvBuf888 dials addr with a 4 KiB receive buffer set before the
// connect, and upgrades.
func dialSmallRcvBuf888(tb testing.TB, addr string) *testWSClient {
	tb.Helper()
	d := net.Dialer{Timeout: 2 * time.Second, Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 4096)
		})
	}}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = conn.Close() })
	c := &testWSClient{conn: conn, br: bufio.NewReaderSize(conn, 16), bw: bufio.NewWriter(conn)}
	req := "GET /ws HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	_, _ = c.bw.WriteString(req)
	_ = c.bw.Flush()
	line, err := c.br.ReadString('\n')
	if err != nil || !strings.Contains(line, "101") {
		tb.Fatalf("upgrade: %q, %v", line, err)
	}
	for {
		if line, err = c.br.ReadString('\n'); err != nil {
			tb.Fatal(err)
		} else if line == "\r\n" {
			return c
		}
	}
}

func TestNonReadingPeerEndsConn888(t *testing.T) {
	for _, kind := range engineKinds(t) {
		t.Run(kind.String(), func(t *testing.T) {
			wrote := make(chan error, 1)
			burst := make([]byte, 12<<20)
			addr, shutdown, _ := startNativeServerCfg(t, celeris.Config{Engine: kind, WriteTimeout: wsStall888Timeout},
				Config{
					// The idle deadline is in the future and pushed out by each
					// ping: nothing but the stall bound can end this conn.
					IdleTimeout: time.Hour,
					Handler: func(c *Conn) {
						go func() { // the read side, so the pings are seen
							for {
								if _, _, err := c.ReadMessage(); err != nil {
									return
								}
							}
						}()
						if err := c.WriteMessage(BinaryMessage, burst); err != nil {
							wrote <- err
							return
						}
						for {
							time.Sleep(20 * time.Millisecond)
							if err := c.WriteMessage(BinaryMessage, burst[:1000]); err != nil {
								wrote <- err
								return
							}
						}
					},
				})
			defer shutdown()

			began := time.Now()
			client := dialSmallRcvBuf888(t, addr)
			// Pings, and no reads.
			stop := make(chan struct{})
			defer close(stop)
			go func() {
				for {
					select {
					case <-stop:
						return
					case <-time.After(100 * time.Millisecond):
						_ = client.writeClientFrame(true, OpPing, []byte("p"))
					}
				}
			}()
			select {
			case err := <-wrote:
				el := time.Since(began)
				t.Logf("the handler's write failed %v after connect (WriteTimeout %v): %v", el.Round(time.Millisecond), wsStall888Timeout, err)
				if el < wsStall888Timeout {
					t.Errorf("ended %v after connect, before WriteTimeout %v", el, wsStall888Timeout)
				}
			case <-time.After(wsStall888Timeout + 6*time.Second):
				t.Fatalf("a WebSocket peer that sends pings and reads nothing was not closed within %v (WriteTimeout %v): celeris#888 item 38",
					wsStall888Timeout+6*time.Second, wsStall888Timeout)
			}
		})
	}
}
