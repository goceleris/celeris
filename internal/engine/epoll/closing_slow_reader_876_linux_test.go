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

// celeris#876, review round 3, through the epoll engine: a client that takes a
// Connection: close response steadily but slowly. A 40 MiB response overruns
// the loopback send queue of a few MiB, so the engine parks the rest in
// userspace and waits for EPOLLOUT, which the socket raises only when about
// half of that queue has drained: at 64 KiB/s that is half a minute away, far
// beyond a 1 s (WriteTimeout) or 5 s (WriteTimeout disabled) bound. A drain
// clock that counts only the userspace queue shrinking reaped such a client at
// the bound with the response cut, where main kept the conn until ReadTimeout
// (here an hour).

// startClosingEngine876 starts an epoll engine serving one response of body
// bytes and returns it with a client that has connected with a 16 KiB receive
// buffer and asked for the response with Connection: close.
func startClosingEngine876(t *testing.T, cfg resource.Config, body int) (*Engine, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg.Addr = addr
	cfg.Protocol = engine.HTTP1
	cfg.Resources = resource.Resources{Workers: 2}
	e, err := New(cfg, &bigResponseHandler{bodySize: body})
	if err != nil {
		t.Skipf("epoll engine unavailable: %v", err)
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
	d := net.Dialer{Timeout: 3 * time.Second, Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 16<<10)
		})
	}}
	c, err := d.Dial("tcp", e.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	for dl := time.Now().Add(5 * time.Second); e.Metrics().ActiveConnections == 0 && time.Now().Before(dl); {
		time.Sleep(time.Millisecond)
	}
	if e.Metrics().ActiveConnections != 1 {
		t.Fatalf("celeris876 PREMISE: %d conns active after the request was sent", e.Metrics().ActiveConnections)
	}
	return e, c
}

// readSlowly takes 16 KiB from c every 250 ms (64 KiB/s) for d. It reports how
// much it got, the error that ended it early if one did, and how long into d
// the SERVER closed the conn (active() == 0) if it did, 0 if it did not: with
// a send queue of MiB behind the cut the client keeps reading what the kernel
// holds, so a server-side close is seen in the engine's own count, not in an
// error.
func readSlowly(c net.Conn, d time.Duration, active func() uint64) (got int64, closedAt time.Duration, err error) {
	buf := make([]byte, 16<<10)
	start := time.Now()
	for end := start.Add(d); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
		if closedAt == 0 && active() == 0 {
			closedAt = time.Since(start)
		}
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, rerr := c.Read(buf)
		got += int64(n)
		if rerr != nil {
			return got, closedAt, rerr
		}
	}
	if closedAt == 0 && active() == 0 {
		closedAt = time.Since(start)
	}
	return got, closedAt, nil
}

// TestEngineClosingResponseSurvivesASlowSteadyReader: 64 KiB/s for far longer
// than the bound, then the rest at full speed: the client must receive every
// byte. Both WriteTimeout 1 s (below ReadTimeout) and WriteTimeout disabled
// (the 5 s floor) are the configurations the review reproduced.
func TestEngineClosingResponseSurvivesASlowSteadyReader(t *testing.T) {
	for _, tc := range []struct {
		name string
		wt   time.Duration
		slow time.Duration
	}{
		{"WriteTimeout1s", time.Second, 4 * time.Second},
		{"WriteTimeoutDisabled", -1, 7 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const body = 40 << 20
			e, c := startClosingEngine876(t, resource.Config{ReadTimeout: time.Hour, WriteTimeout: tc.wt}, body)
			start := time.Now()
			active := func() uint64 { return uint64(e.Metrics().ActiveConnections) }
			got, closedAt, err := readSlowly(c, tc.slow, active)
			if err != nil || closedAt != 0 {
				t.Fatalf("the closing response was cut %v in (the engine closed the conn at %v, read error %v), with the "+
					"client taking 64 KiB/s (%d bytes so far) and a bound of WriteTimeout %v (celeris#876)",
					time.Since(start).Round(time.Millisecond), closedAt.Round(time.Millisecond), err, got, tc.wt)
			}
			_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
			rest, err := io.Copy(io.Discard, c)
			got += rest
			if err != nil || got < body {
				t.Fatalf("the client received %d of %d body bytes (err %v): the response was cut (celeris#876)", got, body, err)
			}
			t.Logf("client took %d bytes slowly over %v, all %d in %v, WriteTimeout %v", got-rest, tc.slow, got,
				time.Since(start).Round(time.Millisecond), tc.wt)
		})
	}
}

// TestEngineClosingConnIsReapedWhenASlowReaderStops keeps the fix from making
// a stalled peer immortal: the same client reads for 2 s and then takes
// nothing; the engine must close the conn about WriteTimeout (1 s) later, not
// when the (hour) ReadTimeout says.
func TestEngineClosingConnIsReapedWhenASlowReaderStops(t *testing.T) {
	e, c := startClosingEngine876(t, resource.Config{ReadTimeout: time.Hour, WriteTimeout: time.Second}, 40<<20)
	active := func() uint64 { return uint64(e.Metrics().ActiveConnections) }
	if _, closedAt, err := readSlowly(c, 2*time.Second, active); err != nil || closedAt != 0 {
		t.Fatalf("the conn was closed by the engine %v into a steady read (read error %v), before the client stopped "+
			"reading (celeris#876)", closedAt.Round(time.Millisecond), err)
	}
	stalled := time.Now()
	for e.Metrics().ActiveConnections != 0 && time.Since(stalled) < 15*time.Second {
		time.Sleep(10 * time.Millisecond)
	}
	took := time.Since(stalled)
	if e.Metrics().ActiveConnections != 0 {
		t.Fatalf("a closing conn whose client stopped reading was still open %v later (WriteTimeout 1s)", took.Round(time.Millisecond))
	}
	if took > 4*time.Second {
		t.Fatalf("the conn lived %v after the client stopped reading; want about WriteTimeout (1s)", took.Round(time.Millisecond))
	}
	t.Logf("conn closed %v after the client stopped reading", took.Round(time.Millisecond))
}
