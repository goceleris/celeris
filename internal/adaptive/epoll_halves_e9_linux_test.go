//go:build linux

package adaptive

import (
	"context"
	"io"
	"log/slog"
	"net"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// The adaptive engine runs epoll loops as its start engine, so celeris#874 and
// celeris#876 reach it through the same Loop code the epoll package's own tests
// pin. These two tests drive that start engine through adaptive's Listen,
// Shutdown and Metrics: the epoll sub-engine is forced
// (CELERIS_ADAPTIVE_START=epoll) and each test fails unless the engine serving
// the connection is epoll, so a run on an io_uring start engine cannot pass for
// the wrong reason. io_uring's own halves of both are not in this file.

// bigBody is the handler of both tests: one response of n bytes.
func bigBodyHandler(n int) stream.Handler {
	body := make([]byte, n)
	for i := range body {
		body[i] = 'x'
	}
	return stream.HandlerFunc(func(_ context.Context, s *stream.Stream) error {
		if s.ResponseWriter == nil {
			return nil
		}
		return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "application/octet-stream"}}, body)
	})
}

// startEpollAdaptive builds an adaptive engine that starts on epoll, runs its
// Listen, and returns it with the cancel of Listen's context and the channel
// its Listen result arrives on.
func startEpollAdaptive(t *testing.T, cfg resource.Config, h stream.Handler) (*Engine, context.CancelFunc, <-chan error) {
	t.Helper()
	t.Setenv("CELERIS_ADAPTIVE_START", "epoll")
	cfg.Addr = "127.0.0.1:0"
	cfg.Engine = engine.Adaptive
	cfg.Protocol = engine.HTTP1
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg.Resources = resource.Resources{Workers: 2}
	e, err := New(cfg, h, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second): // 5 s: a test that waited for Listen has drained errCh already
		}
		_ = e.Shutdown(context.Background())
	})
	for dl := time.Now().Add(15 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("adaptive engine did not bind")
	}
	if got := e.ActiveEngine().Type(); got != engine.Epoll {
		t.Fatalf("e9 PREMISE: the active engine is %v, want epoll: the epoll halves of #874 and #876 are not what this test would run", got)
	}
	return e, cancel, errCh
}

// slowDial dials addr with a small receive buffer, so a response of a few
// MiB overruns the socket queues of both ends.
func slowDial(t *testing.T, addr string) net.Conn {
	t.Helper()
	d := net.Dialer{Timeout: 3 * time.Second, Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 64<<10)
		})
	}}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestAdaptiveEpollBytesWrittenAfterShutdownEqualsWhatTheClientReceived is
// #874's metric through the adaptive engine: Metrics().BytesWritten after
// Shutdown has returned is every byte the client received, including those
// the epoll loops' shutdown drain sent.
func TestAdaptiveEpollBytesWrittenAfterShutdownEqualsWhatTheClientReceived(t *testing.T) {
	const body = 24 << 20
	e, _, errCh := startEpollAdaptive(t, resource.Config{}, bigBodyHandler(body))
	c := slowDial(t, e.Addr().String())
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
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
		t.Fatalf("e9 PREMISE: %d bytes counted before shutdown, want some of the %d, not all", before, body)
	}

	// Server.Shutdown's order: hand the budget over, stop the loops. The
	// client starts reading as the stop begins.
	shutErr := make(chan error, 1)
	budget, cancelBudget := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelBudget()
	go func() { shutErr <- e.Shutdown(budget) }()
	got := int64(0)
	buf := make([]byte, 256<<10)
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	for {
		n, rerr := c.Read(buf)
		got += int64(n)
		if rerr != nil {
			break
		}
	}
	if err := <-shutErr; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-errCh:
	case <-time.After(30 * time.Second):
		t.Fatal("Listen did not return")
	}
	counted := e.Metrics().BytesWritten
	t.Logf("client received %d bytes; BytesWritten %d before shutdown, %d after", got, before, counted)
	if got <= int64(before) {
		t.Fatalf("e9 PREMISE: the client received %d bytes, no more than the %d counted before shutdown", got, before)
	}
	if int64(counted) != got {
		t.Errorf("BytesWritten = %d after Shutdown, the client received %d: %d bytes sent by the shutdown drain are in no counter (celeris#874)",
			counted, got, got-int64(counted))
	}
}

// TestAdaptiveEpollSlowReaderOfAClosingResponseIsNotCut is #876 through the
// adaptive engine: ReadTimeout 1 s, a Connection: close response of 40 MiB
// that the client takes at about 16 MiB/s, i.e. over 2 s, steadily. Measured
// from the request's read, the epoll loop cut it at ReadTimeout.
func TestAdaptiveEpollSlowReaderOfAClosingResponseIsNotCut(t *testing.T) {
	const body = 40 << 20
	e, _, _ := startEpollAdaptive(t, resource.Config{
		ReadTimeout: time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 0,
	}, bigBodyHandler(body))
	c := slowDial(t, e.Addr().String())
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
		t.Fatalf("e9 PREMISE: the transfer took %v, not longer than twice ReadTimeout", took)
	}

}
