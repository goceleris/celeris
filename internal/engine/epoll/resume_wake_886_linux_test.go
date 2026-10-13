//go:build linux

package epoll

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	celerisengine "github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#886 item 8 (the polish list for the adaptive engine). ResumeAccept
// woke only a loop parked in its DRAINING to SUSPENDED step. A loop that has
// not parked closed its listener at the pause and went back to waiting up to 500 ms in epoll_wait with no listener;
// the resume found it running, did nothing, and the listener came back only
// when that wait ended. That is a loop that holds a connection (it cannot
// park) and also an idle one in the first wait after the pause (it parks at
// the iteration after the close). The wake eventfd is armed in every mode, so
// ResumeAccept signals it.
//
// A loop whose Config has a ReadHeaderTimeout (the default, 10 s) also owns a
// 25 ms timerfd, which ends that wait on its own: the lag is at most 25 ms
// there (measured 18 to 24 ms on origin/main), and only a loop without a header
// deadline (ReadHeaderTimeout < 0) sits out the whole 500 ms.

// live886 opens n keep-alive connections to addr, each with one answered
// request, so that they are registered with a loop: a loop that holds a
// connection does not park in a pause, and the resume reaches it only through
// the wake eventfd. n = 0 opens none.
func live886(t *testing.T, addr string, n int) {
	t.Helper()
	conns := make([]net.Conn, 0, n)
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close()
		}
	})
	for range n {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial %s: %v", addr, err)
		}
		conns = append(conns, c)
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		_ = c.SetDeadline(time.Time{})
	}
}

// resumeLag886 pauses the engine, resumes it at once and returns how long it
// took until want loops listen again (the kernel's table, census895), or
// gaveup if that did not happen.
func resumeLag886(t *testing.T, e *Engine, port, want int, gaveup time.Duration) time.Duration {
	t.Helper()
	if err := e.PauseAccept(); err != nil {
		t.Fatalf("PauseAccept: %v", err)
	}
	if n := census895(t).listening[port]; n != 0 {
		t.Fatalf("celeris886 PREMISE: %d listen sockets on port %d after PauseAccept, want 0", n, port)
	}
	t0 := time.Now()
	if err := e.ResumeAccept(); err != nil {
		t.Fatalf("ResumeAccept: %v", err)
	}
	for time.Since(t0) < gaveup {
		if census895(t).listening[port] >= want {
			return time.Since(t0)
		}
		time.Sleep(200 * time.Microsecond)
	}
	return gaveup
}

// start886 is start895 with a ReadHeaderTimeout: a negative one disables the
// header deadline, and with it the loop's 25 ms timerfd.
func start886(t *testing.T, loops int, readHeaderTimeout time.Duration) *run895 {
	t.Helper()
	e, err := New(resource.Config{
		Addr:               "127.0.0.1:0",
		Engine:             celerisengine.Epoll,
		Protocol:           celerisengine.HTTP1,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		DisableDeferAccept: true,
		ReadHeaderTimeout:  readHeaderTimeout,
		Resources:          resource.Resources{Workers: loops},
	}, respondingHandler{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	r := &run895{e: e, cancel: cancel, done: done}
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); {
		if e.Addr() != nil {
			t.Cleanup(func() { cancel(); <-done })
			return r
		}
		select {
		case err := <-done:
			t.Fatalf("Listen returned before publishing an address: %v", err)
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
	t.Fatalf("engine never published an address")
	return nil
}

// TestResumeAcceptReachesALoopThatCannotPark886: a resume that comes straight
// after the pause must make every loop listen again within lagBound. The pause
// is DisableDeferAccept's (its listener closes at once), the resume follows it
// with no sleep, and the cycle runs three times, since the lag is the
// remainder of a wait. The default arm is the guard that the 25 ms timerfd
// keeps the lag short; the other arm is the one the eventfd signal is for.
func TestResumeAcceptReachesALoopThatCannotPark886(t *testing.T) {
	const (
		cycles   = 3
		lagBound = 200 * time.Millisecond
	)
	for _, tc := range []struct {
		name  string
		rht   time.Duration
		conns int
	}{
		{"default-read-header-timeout/live-connections", 0, 16},
		{"no-read-header-timeout/live-connections", -1, 16},
		{"default-read-header-timeout/no-connections", 0, 0},
		{"no-read-header-timeout/no-connections", -1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := start886(t, 2, tc.rht)
			r.e.mu.Lock()
			want := len(r.e.loops)
			r.e.mu.Unlock()
			port := portOf895(t, r.e.Addr())
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			live886(t, addr, tc.conns)

			var lags []time.Duration
			for range cycles {
				lags = append(lags, resumeLag886(t, r.e, port, want, 3*time.Second))
			}
			t.Logf("celeris886 RESUME-LAG (%d loops, %d live connections, pause then resume with no sleep): %v, bound %v", want, tc.conns, lags, lagBound)
			for i, lag := range lags {
				if lag > lagBound {
					t.Errorf("celeris886 LAG: cycle %d took %v to have %d loops listening again, want within %v: "+
						"a loop that had not parked did not see the resume until its wait ended", i, lag, want, lagBound)
				}
			}
			get895(t, addr)
		})
	}
}
