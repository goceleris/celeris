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

// TestEngineClosingResponseSurvivesReadTimeoutWhileTheClientReads is
// celeris#876 end to end on the engine: ReadTimeout 1 s, WriteTimeout 30 s, a
// Connection: close response of 40 MiB that the client takes at about
// 16 MiB/s, steadily, over more than twice ReadTimeout. The read the sweep
// measured from was the request's, a second before the client had taken a
// quarter of it.
func TestEngineClosingResponseSurvivesReadTimeoutWhileTheClientReads(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	const body = 40 << 20
	e, err := New(resource.Config{
		Addr:         addr,
		Protocol:     engine.HTTP1,
		ReadTimeout:  time.Second,
		WriteTimeout: 30 * time.Second,
		Resources:    resource.Resources{Workers: 2},
	}, &bigResponseHandler{bodySize: body})
	if err != nil {
		t.Skipf("epoll engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	defer func() {
		cancel()
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
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 64<<10)
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
	start := time.Now()
	got := int64(0)
	buf := make([]byte, 512<<10)
	_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
	for {
		n, rerr := c.Read(buf)
		got += int64(n)
		if rerr != nil {
			break
		}
		// Paced to 16 MiB/s by the clock, not by a sleep per read: a read
		// returns what the small receive buffer holds.
		if d := time.Until(start.Add(time.Duration(float64(got) / (16 << 20) * float64(time.Second)))); d > 0 {
			time.Sleep(d)
		}
	}
	took := time.Since(start)
	t.Logf("client received %d of %d body bytes in %v with ReadTimeout %v", got, body, took.Round(time.Millisecond), time.Second)
	if got < body {
		t.Errorf("the client received %d of %d body bytes (plus headers): the closing response was cut %v in, with the client "+
			"reading steadily, by a ReadTimeout of 1s measured from the request (celeris#876)", got, body, took.Round(time.Millisecond))
	}
	if got >= body && took < 2*time.Second {
		t.Fatalf("celeris876 PREMISE: the transfer took %v, not longer than twice ReadTimeout", took)
	}

}
