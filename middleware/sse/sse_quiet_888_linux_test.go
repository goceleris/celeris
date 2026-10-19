//go:build linux

package sse_test

import (
	"net"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/sse"
)

// A producer that goes quiet while its client reads nothing (celeris#888 items
// 38 and 39, celeris#884 item 69; review of #1077). One event larger than the
// socket takes leaves a remainder queued, and then nothing is written: no
// heartbeat, no further event. On io_uring the stream's inline write leaves a
// SEND of under 64 KiB for the worker, a size the worker did not expect to wait
// on its peer, so it waited for the completion with no timeout, no sweep ran
// and the client was held for ever. The test of the other producers never saw
// it, because each of them kept writing, and every write wakes the worker.

const quietSndBuf888 = 64 << 10

// quietCapacity888 measures how many bytes one nonblocking write takes on a
// loopback pair shaped as the server's (send buffer quietSndBuf888, client
// receive buffer 4 KiB, client reads nothing).
func quietCapacity888(tb testing.TB) int {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Skipf("loopback unavailable: %v", err)
	}
	defer func() { _ = ln.Close() }()
	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) { _ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 4096) })
	}}
	cl, err := d.Dial("tcp", ln.Addr().String())
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = cl.Close() }()
	srv, err := ln.Accept()
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	rc, err := srv.(*net.TCPConn).SyscallConn()
	if err != nil {
		tb.Fatal(err)
	}
	n := 0
	_ = rc.Control(func(fd uintptr) {
		_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF, quietSndBuf888)
		_ = unix.SetNonblock(int(fd), true)
		n, _ = unix.Write(int(fd), make([]byte, 4<<20))
	})
	if n <= 0 {
		tb.Skip("could not measure the socket's capacity")
	}
	return n
}

// TestQuietNonReadingClientEndsStream888: one event of the socket's capacity
// plus delta, then silence, to a client that reads nothing. A remainder (delta
// above 0) is queued for a peer that takes none of it: the stream ends about
// WriteTimeout after connect, with the header timer or without it, and
// whatever the size of the remainder (4 KiB is under io_uring's long-SEND mark,
// 256 KiB over it). Delta -2048 is the control: the event fits, nothing is
// queued, and a client that took everything written is not closed.
func TestQuietNonReadingClientEndsStream888(t *testing.T) {
	capN := quietCapacity888(t)
	t.Logf("capacity of one write: %d bytes", capN)
	const wt = nonReadWriteTimeout888
	for _, cell := range nonReadCells888(t) {
		if cell.engine == celeris.Std {
			continue // std: a write blocked on the peer is ended by its own deadline
		}
		for _, delta := range []int{-2048, 4 << 10, 40 << 10, 256 << 10} {
			for _, noHdr := range []bool{false, true} {
				if noHdr && delta != 4<<10 {
					continue // the header timer matters for the smallest remainder only
				}
				name := cell.name + "/delta=" + strconv.Itoa(delta)
				if noHdr {
					name += "/no-header-timer"
				}
				t.Run(name, func(t *testing.T) {
					done := make(chan time.Time, 1)
					data := strings.Repeat("x", capN+delta)
					h := sse.New(sse.Config{
						HeartbeatInterval: -1,
						OnDisconnect: func(*celeris.Context, *sse.Client) {
							select {
							case done <- time.Now():
							default:
							}
						},
						Handler: func(client *sse.Client) {
							if err := client.Send(sse.Event{Data: data}); err != nil {
								return
							}
							<-client.Context().Done()
						},
					})
					cfg := celeris.Config{Engine: cell.engine, AsyncHandlers: cell.async, WriteTimeout: wt,
						ShutdownTimeout: 2 * time.Second, SocketSendBuf: quietSndBuf888}
					if noHdr {
						cfg.ReadHeaderTimeout = -1
					}
					addr, shutdown := startSSEServerCfg(t, cfg, false, h)
					defer shutdown()
					began := time.Now()
					_ = dialSmallRcvBuf888(t, addr)
					if delta < 0 {
						select {
						case at := <-done:
							t.Fatalf("a stream whose event fit the socket was ended %v after connect: nothing was queued", at.Sub(began))
						case <-time.After(3 * wt):
						}
						return
					}
					select {
					case at := <-done:
						el := at.Sub(began)
						t.Logf("OnDisconnect %v after connect (WriteTimeout %v)", el.Round(time.Millisecond), wt)
						if el < wt {
							t.Errorf("OnDisconnect ran %v after connect, before WriteTimeout %v", el, wt)
						}
					case <-time.After(wt + 6*time.Second):
						t.Fatalf("OnDisconnect never ran in %v (WriteTimeout %v): a quiet stream's client that reads nothing holds it for ever",
							wt+6*time.Second, wt)
					}
				})
			}
		}
	}
}
