//go:build linux

package epoll

import (
	"context"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// TestEngineBytesWrittenAfterShutdownEqualsWhatTheClientReceived is the
// issue's metric end to end (celeris#874): a response far larger than the
// socket buffers is mid-send when the engine is shut down; Shutdown hands the
// loops its budget, Listen's context is cancelled, the loops' shutdown drain
// sends the rest to a client that reads it only now, and once Listen has
// returned Metrics().BytesWritten must be every byte the client received.
func TestEngineBytesWrittenAfterShutdownEqualsWhatTheClientReceived(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	const body = 24 << 20
	e, err := New(resource.Config{
		Addr:      addr,
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
	}, &bigResponseHandler{bodySize: body})
	if err != nil {
		t.Skipf("epoll engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	listenDone := false
	defer func() {
		cancel()
		if listenDone {
			return
		}
		select {
		case <-errCh:
		case <-time.After(30 * time.Second):
		}
	}()
	for dl := time.Now().Add(15 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind")
	}

	d := net.Dialer{Timeout: 3 * time.Second, Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 8<<10)
		})
	}}
	c, err := d.Dial("tcp", e.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Let the server stage the response and fill the socket: BytesWritten
	// stops moving, with the client not reading.
	var last uint64
	for stable, dl := 0, time.Now().Add(10*time.Second); stable < 4 && time.Now().Before(dl); time.Sleep(100 * time.Millisecond) {
		cur := e.Metrics().BytesWritten
		if cur > 0 && cur == last {
			stable++
		} else {
			stable = 0
		}
		last = cur
	}
	before := e.Metrics().BytesWritten
	if before == 0 || before >= body {
		t.Fatalf("celeris874 PREMISE: %d bytes counted before shutdown, want some of the %d, not all", before, body)
	}

	budget, cancelBudget := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelBudget()
	_ = e.Shutdown(budget)
	cancel()

	got := int64(0)
	readStart := time.Now()
	var lastData time.Time
	buf := make([]byte, 256<<10)
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	for {
		n, rerr := c.Read(buf)
		got += int64(n)
		if n > 0 {
			lastData = time.Now()
		}
		if rerr != nil {
			t.Logf("read ended after %v (last data at %v): %v", time.Since(readStart).Round(time.Millisecond),
				lastData.Sub(readStart).Round(time.Millisecond), rerr)
			break
		}
	}
	select {
	case <-errCh:
		listenDone = true
	case <-time.After(30 * time.Second):
		t.Fatal("Listen did not return after the context was cancelled")
	}

	counted := e.Metrics().BytesWritten
	t.Logf("client received %d bytes; BytesWritten %d before shutdown, %d after", got, before, counted)
	if got <= int64(before) {
		t.Fatalf("celeris874 PREMISE: the client received %d bytes, no more than the %d counted before shutdown; "+
			"the shutdown drain sent nothing", got, before)
	}
	if int64(counted) != got {
		t.Errorf("BytesWritten = %d after Shutdown, but the client received %d bytes: the %d the shutdown drain "+
			"sent are in no counter (celeris#874)", counted, got, got-int64(counted))
	}
}
