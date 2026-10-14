//go:build linux

package celeris_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris"
)

// celeris#811 through the public API, on Adaptive started on io_uring (and on
// io_uring itself): an async conn whose ring SEND waits for a client that does
// not read must not keep the worker from parking. The engine-level test
// (internal/engine/iouring, dirty_busy_poll_811_linux_test.go) holds the
// precise measurement; this one is the same shape through celeris.Server, so
// that the engine Adaptive hands the conns to is the one measured, and a
// server that was not on io_uring fails its premise instead of passing on
// epoll.

const (
	busyConns811  = 4
	busyBody811   = 3 << 20
	busyWindow811 = 2 * time.Second
	busySettle811 = 600 * time.Millisecond
	busyBudget811 = 0.10
)

func busyByte811(i int) byte { return byte(i*31 + 7) }

func busyCPU811(t *testing.T) time.Duration {
	t.Helper()
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
		t.Fatalf("getrusage: %v", err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func TestIouringStalledAsyncSendDoesNotBusyPollThroughTheServer811(t *testing.T) {
	body := make([]byte, busyBody811)
	for i := range body {
		body[i] = busyByte811(i)
	}
	for _, e := range drainEngines806 {
		if e.name != "adaptive-iouring" && e.name != "io_uring" {
			continue
		}
		t.Run(e.name, func(t *testing.T) {
			srv := startDrainServer806(t, e, 5*time.Second, 0,
				func(c *celeris.Config) {
					c.AsyncHandlers = true
					c.SocketSendBuf = 64 << 10 // the direct write is short on any host
				},
				func(s *celeris.Server) {
					s.GET("/big", func(c *celeris.Context) error {
						return c.Blob(http.StatusOK, "application/octet-stream", body)
					}).Async()
					s.GET("/small", func(c *celeris.Context) error { return c.String(http.StatusOK, "small") }).Async()
				})

			i0, it0 := busyCPU811(t), time.Now()
			time.Sleep(time.Second)
			t.Logf("celeris811 IDLE engine=%s window_ms=%d cpu_ms=%.1f", e.name, time.Since(it0).Milliseconds(),
				float64(busyCPU811(t)-i0)/1e6)

			type client struct {
				c  net.Conn
				br *bufio.Reader
			}
			clients := make([]*client, busyConns811)
			for i := range clients {
				d := net.Dialer{
					Timeout: 3 * time.Second,
					Control: func(_, _ string, rc syscall.RawConn) error {
						var serr error
						if err := rc.Control(func(fd uintptr) {
							serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 4096)
						}); err != nil {
							return err
						}
						return serr
					},
				}
				c, err := d.Dial("tcp", srv.addr)
				if err != nil {
					t.Fatalf("dial: %v", err)
				}
				t.Cleanup(func() { _ = c.Close() })
				clients[i] = &client{c: c, br: bufio.NewReaderSize(c, 64<<10)}
				if _, err := fmt.Fprintf(c, "GET /big HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
					t.Fatalf("send /big: %v", err)
				}
			}
			time.Sleep(300 * time.Millisecond) // every first SEND is in flight
			for _, c := range clients {
				if _, err := fmt.Fprintf(c.c, "GET /small HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
					t.Fatalf("send /small: %v", err)
				}
			}
			time.Sleep(busySettle811)

			c0, t0 := busyCPU811(t), time.Now()
			time.Sleep(busyWindow811)
			spent, wall := busyCPU811(t)-c0, time.Since(t0)
			frac := float64(spent) / float64(wall)
			t.Logf("celeris811 CPU engine=%s conns=%d window_ms=%d cpu_ms=%.1f cpu_frac=%.3f", e.name, busyConns811,
				wall.Milliseconds(), float64(spent)/1e6, frac)
			// The premise again, after the window: a CPU budget met by a server
			// that Adaptive had moved to epoll in the meantime would pass on an
			// engine that is not the one under test.
			if err := e.premise(srv); err != nil {
				t.Fatalf("after the CPU window: %v", err)
			}
			if n := srv.s.EngineInfo().Metrics.AdaptiveSwitches; n != 0 {
				t.Fatalf("celeris811 PREMISE: %s switched engines %d time(s) during the CPU window: the budget was not measured on one engine", e.name, n)
			}
			if frac > busyBudget811 {
				t.Errorf("celeris#811: %d async conns whose clients do not read cost the server %.0f ms of CPU in %d ms "+
					"(%.1f%% of a core, budget %.0f%%) on %s: the worker is not parked",
					busyConns811, float64(spent)/1e6, wall.Milliseconds(), 100*frac, 100*busyBudget811, e.name)
			}

			// The wake: every client reads now, and gets everything, in order.
			t1 := time.Now()
			errs := make(chan error, len(clients))
			for _, c := range clients {
				go func() {
					_ = c.c.SetReadDeadline(time.Now().Add(40 * time.Second))
					resp, err := http.ReadResponse(c.br, nil)
					if err != nil {
						errs <- fmt.Errorf("first response head: %w", err)
						return
					}
					got, err := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if err != nil || len(got) != busyBody811 {
						errs <- fmt.Errorf("first response: %d of %d bytes, err %v", len(got), busyBody811, err)
						return
					}
					for i := 0; i < len(got); i += 4099 {
						if got[i] != busyByte811(i) {
							errs <- fmt.Errorf("first response: byte %d is %#x, want %#x", i, got[i], busyByte811(i))
							return
						}
					}
					resp, err = http.ReadResponse(c.br, nil)
					if err != nil {
						errs <- fmt.Errorf("second response head: %w", err)
						return
					}
					second, err := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if err != nil || !bytes.Equal(second, []byte("small")) {
						errs <- fmt.Errorf("second response %q, err %v; want %q", second, err, "small")
						return
					}
					errs <- nil
				}()
			}
			for range clients {
				if err := <-errs; err != nil {
					t.Errorf("celeris#811: after the clients resumed reading: %v", err)
				}
			}
			t.Logf("celeris811 RESUME engine=%s conns=%d transfer_ms=%.1f bytes=%s", e.name, busyConns811,
				float64(time.Since(t1))/1e6, strconv.Itoa(busyConns811*busyBody811))
		})
	}
}
