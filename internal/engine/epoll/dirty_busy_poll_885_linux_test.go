//go:build linux

package epoll_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/sse"
)

// celeris#885 item 42 (the epoll twin of celeris#811, which PR #994 fixed for
// io_uring): a truly-detached conn (Server-Sent Events here, a WebSocket the
// same) whose peer does not read left its remainder on the loop's dirty list
// for as long as the peer held out, and adaptiveTimeoutMs returns 0 while the
// list is not empty, so every pass of the loop was an epoll_wait(0) and a
// write(2) that returned EAGAIN: a core burnt for nothing. A non-detached
// conn already armed EPOLLOUT and left the list; the detached one is now
// handed to EPOLLOUT as well, and handleWritable (the loop) and the guarded
// writeFn (the middleware's goroutine) flush it.
//
// The test is the issue's measurement: N SSE streams queue a few MiB each
// toward clients with a 4 KiB receive buffer that do not read (the server's
// send buffer is 64 KiB, so what the socket takes is a fraction of it), the
// process's CPU over a 2 s window is the budget, then every client reads and
// must get every event, in order. Process CPU (getrusage), as the io_uring
// test: the loop is the only thread working in the window.

const (
	busy885Conns   = 4
	busy885Events  = 96
	busy885Payload = 32 << 10
	busy885SndBuf  = 64 << 10
	busy885Window  = 2 * time.Second
	busy885Settle  = 500 * time.Millisecond
	// busy885Budget is the fraction of one core the process may use in the
	// window while nothing can progress. A parked loop pair measures 3% to 10%
	// here (two loops with a 25 ms timer each, the sweep's detached-conn gate,
	// and the process around them; the IDLE line logs the same engine with no
	// conn), a spinning loop 100% (celeris#811 measured 37% for one conn on
	// io_uring), and four conns on two loops 200% or more. 30% is far from both.
	busy885Budget = 0.30
)

func cpu885(t *testing.T) time.Duration {
	t.Helper()
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
		t.Fatalf("getrusage: %v", err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func startBusy885Server(t *testing.T, handler func(*sse.Client)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	srv := celeris.New(celeris.Config{Engine: celeris.Epoll, Workers: 2, SocketSendBuf: busy885SndBuf})
	srv.GET("/events", sse.New(sse.Config{HeartbeatInterval: -1, Handler: handler}))
	var startErr atomic.Pointer[error]
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if e := srv.StartWithListenerAndContext(ctx, ln); e != nil {
			startErr.Store(&e)
		}
	}()
	time.Sleep(500 * time.Millisecond)
	waitUpOrStopped(t, srv, done, 30*time.Second)
	if p := startErr.Load(); p != nil {
		t.Fatalf("server start: %v", *p)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Log("server goroutine did not exit within 10s")
		}
	})
	return addr
}

// busyEvent885 is event i's payload: its number, then filler that depends on it.
func busyEvent885(i int) string {
	return fmt.Sprintf("%06d:%s", i, strings.Repeat(string(rune('a'+i%26)), busy885Payload))
}

func TestEpollDetachedBackpressureDoesNotBusyPoll885(t *testing.T) {
	sent := make(chan struct{}, busy885Conns)
	addr := startBusy885Server(t, func(c *sse.Client) {
		for i := 0; i < busy885Events; i++ {
			if err := c.SendData(busyEvent885(i)); err != nil {
				return
			}
		}
		sent <- struct{}{}
		<-c.Context().Done()
	})

	// The idle baseline: the same process, the engine up, nothing connected.
	// A red run is read against it.
	i0, it0 := cpu885(t), time.Now()
	time.Sleep(500 * time.Millisecond)
	t.Logf("celeris885 IDLE window_ms=%d cpu_ms=%.1f", time.Since(it0).Milliseconds(), float64(cpu885(t)-i0)/1e6)

	clients := make([]net.Conn, busy885Conns)
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
		c, err := d.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		clients[i] = c
		if _, err := fmt.Fprintf(c, "GET /events HTTP/1.1\r\nHost: x\r\nAccept: text/event-stream\r\n\r\n"); err != nil {
			t.Fatalf("send request: %v", err)
		}
	}
	for range clients {
		select {
		case <-sent:
		case <-time.After(10 * time.Second):
			t.Fatal("a handler never queued its events")
		}
	}
	time.Sleep(busy885Settle) // every socket is full, nothing can progress

	c0, t0 := cpu885(t), time.Now()
	time.Sleep(busy885Window)
	spent, wall := cpu885(t)-c0, time.Since(t0)
	frac := float64(spent) / float64(wall)
	t.Logf("celeris885 CPU conns=%d window_ms=%d cpu_ms=%.1f cpu_frac=%.3f", busy885Conns, wall.Milliseconds(),
		float64(spent)/1e6, frac)
	if frac > busy885Budget {
		t.Errorf("celeris#885 item 42: %d detached SSE conns whose clients do not read cost the process %.0f ms of CPU in %d ms "+
			"(%.1f%% of a core, budget %.0f%%): the loop is not parked", busy885Conns, float64(spent)/1e6, wall.Milliseconds(),
			100*frac, 100*busy885Budget)
	}

	// The wake: every client reads now, and gets every event, in order.
	errs := make(chan error, len(clients))
	t1 := time.Now()
	for _, c := range clients {
		go func() { errs <- readEvents885(c) }()
	}
	for range clients {
		err := <-errs
		t.Logf("celeris885 client done after %.1f ms: %v", float64(time.Since(t1))/1e6, err)
		if err != nil {
			t.Errorf("celeris#885 item 42: after the clients resumed reading: %v", err)
		}
	}
	t.Logf("celeris885 RESUME conns=%d transfer_ms=%.1f bytes=%d", busy885Conns, float64(time.Since(t1))/1e6,
		busy885Conns*busy885Events*(busy885Payload+16))
}

// readEvents885 reads one SSE response and checks that its events are
// 0..busy885Events-1, whole and in order.
func readEvents885(c net.Conn) error {
	_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
	br := bufio.NewReaderSize(c, 64<<10)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return fmt.Errorf("response head: %w", err)
	}
	// No Body.Close: it drains a body that never ends (the stream stays open);
	// the cleanup closes the conn.
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	body := bufio.NewReaderSize(resp.Body, 64<<10)
	for next := 0; next < busy885Events; {
		line, err := body.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("after %d of %d events: %w", next, busy885Events, err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			return fmt.Errorf("event %d: unexpected line %.40q", next, line)
		}
		if want := busyEvent885(next); data != want {
			n, _ := strconv.Atoi(strings.SplitN(data, ":", 2)[0])
			return fmt.Errorf("event %d: got event %d / %d bytes, want it whole and in order (%d bytes)", next, n, len(data), len(want))
		}
		next++
	}
	return nil
}
