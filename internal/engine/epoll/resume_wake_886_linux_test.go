//go:build linux

package epoll

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
// woke only a loop parked in its DRAINING to SUSPENDED step. A loop that
// cannot park (it holds a connection) closed its listener at the pause and
// went back to waiting up to 500 ms in epoll_wait with no listener; the resume found it
// running, did nothing, and the listener came back only when that wait ended.
// The wake eventfd is armed in every mode, so ResumeAccept signals it.

// live886 opens n keep-alive connections to addr, each with one answered
// request, so that they are registered with a loop: a loop that holds a
// connection does not park in a pause, and the resume reaches it only through
// the wake eventfd.
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

// TestResumeAcceptReachesALoopThatCannotPark886: a resume that comes
// straight after the pause must make every loop listen again within lagBound.
// The pause is DisableDeferAccept's (its listener closes at once), the resume
// follows it with no sleep, and the cycle runs three times, since the lag is
// the remainder of a wait.
func TestResumeAcceptReachesALoopThatCannotPark886(t *testing.T) {
	const (
		cycles   = 3
		lagBound = 200 * time.Millisecond
	)
	r := start895(t, "127.0.0.1:0", 2, true)
	r.e.mu.Lock()
	want := len(r.e.loops)
	r.e.mu.Unlock()
	port := portOf895(t, r.e.Addr())
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	live886(t, addr, 16)

	var lags []time.Duration
	for range cycles {
		lags = append(lags, resumeLag886(t, r.e, port, want, 3*time.Second))
	}
	t.Logf("celeris886 RESUME-LAG (%d loops, 16 live connections, pause then resume with no sleep): %v, bound %v", want, lags, lagBound)
	for i, lag := range lags {
		if lag > lagBound {
			t.Errorf("celeris886 LAG: cycle %d took %v to have %d loops listening again, want within %v: "+
				"a loop with a live connection did not see the resume until its wait ended", i, lag, want, lagBound)
		}
	}
	get895(t, addr)
}
