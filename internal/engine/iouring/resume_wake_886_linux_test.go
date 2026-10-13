//go:build linux

package iouring

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// celeris#886 item 8 (the polish list for the adaptive engine). ResumeAccept
// woke only a worker parked in its DRAINING to SUSPENDED step. A worker that has
// not parked closed its listener at the pause and went back to waiting up to a second in the ring (Worker.baseTimeout) with no listener;
// the resume found it running, did nothing, and the listener came back only
// when that wait ended. That is a worker that holds a connection (it cannot
// park) and also an idle one in the first wait after the pause (it parks at
// the iteration after the close). The wake eventfd is armed in every mode, so
// ResumeAccept signals it.

// live886 opens n keep-alive connections to addr, each with one answered
// request, so that they are registered with a worker: a worker that holds a
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
// took until want workers listen again (the kernel's table, census895), or
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

// TestResumeAcceptReachesAWorkerThatCannotPark886: a resume that comes
// straight after the pause must make every worker listen again within
// lagBound. The pause is DisableDeferAccept's (its listener closes at once),
// the resume follows it with no sleep, and the cycle runs three times, since
// the lag is the remainder of a wait. Two workers are asked for: with memlock
// raised both start, and a fix that signalled only one of them fails; at the
// runner's 8 MiB one starts (see the PREMISE check). The first arm holds live
// connections (the worker can never park); the second holds none, and it lags
// just the same, because the worker parks only at the iteration after the
// pause's close, which is after its first wait with no listener.
func TestResumeAcceptReachesAWorkerThatCannotPark886(t *testing.T) {
	const (
		cycles   = 3
		lagBound = 200 * time.Millisecond
	)
	for _, tc := range []struct {
		name  string
		conns int
	}{
		{"live-connections", 16},
		{"no-connections", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := start895(t, "127.0.0.1:0", 2, 1, true)
			r.e.mu.Lock()
			want := len(r.e.workers)
			r.e.mu.Unlock()
			// Where memlock funds two workers the test must run with two: a
			// fix that signalled only the first worker passes with one. The
			// runner's 8 MiB funds one, so the unit job runs one worker here;
			// the job `iouring` step for the multi-worker tests runs this test
			// by name with memlock raised and skipping forbidden.
			if maxW := MaxWorkersForMemlock(); want < 2 && (maxW < 0 || maxW >= 2) {
				t.Fatalf("celeris886 PREMISE: %d io_uring worker(s) started with memlock funding %d (-1: unlimited), want 2", want, maxW)
			}
			port := portOf895(t, r.e.Addr())
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			live886(t, addr, tc.conns)

			var lags []time.Duration
			for range cycles {
				lags = append(lags, resumeLag886(t, r.e, port, want, 3*time.Second))
			}
			t.Logf("celeris886 RESUME-LAG (%d workers, %d live connections, pause then resume with no sleep): %v, bound %v", want, tc.conns, lags, lagBound)
			for i, lag := range lags {
				if lag > lagBound {
					t.Errorf("celeris886 LAG: cycle %d took %v to have %d workers listening again, want within %v: "+
						"a worker that had not parked did not see the resume until its wait ended", i, lag, want, lagBound)
				}
			}
			get895(t, addr)
		})
	}
}
