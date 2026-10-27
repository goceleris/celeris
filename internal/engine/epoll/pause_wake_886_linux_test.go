//go:build linux

package epoll

import (
	"net"
	"strconv"
	"testing"
	"time"
)

// celeris#886 item 24 (and the flap remainder of item 11). BeginPauseAccept
// stored the flag and woke nobody: a loop that is idle and still listening saw
// the pause only when its current wait ended, so its listener closed late. The
// resume side got the wake eventfd in #1013; this is the pause side. The test
// is #1013's lag test run the other way: the pause is DisableDeferAccept's (its
// listener closes at once, there is no linger), and the lag is the time from
// BeginPauseAccept until the kernel's table shows no listener of the engine.

// pauseLag886 begins a pause and returns how long it took until no listen
// socket is left on port (the kernel's table, census895), or gaveup if that
// did not happen. It then resumes and waits for want listeners, so that the
// next cycle starts from a listening engine.
func pauseLag886(t *testing.T, e *Engine, port, want int, gaveup time.Duration) time.Duration {
	t.Helper()
	if n := census895(t).listening[port]; n < want {
		t.Fatalf("celeris886 PREMISE: %d listen sockets on port %d before the pause, want %d", n, port, want)
	}
	t0 := time.Now()
	e.BeginPauseAccept()
	lag := gaveup
	for time.Since(t0) < gaveup {
		if census895(t).listening[port] == 0 {
			lag = time.Since(t0)
			break
		}
		time.Sleep(200 * time.Microsecond)
	}
	if err := e.ResumeAccept(); err != nil {
		t.Fatalf("ResumeAccept: %v", err)
	}
	for dl := time.Now().Add(3 * time.Second); census895(t).listening[port] < want; time.Sleep(time.Millisecond) {
		if time.Now().After(dl) {
			t.Fatalf("celeris886: %d loops listening on port %d 3s after the resume, want %d", census895(t).listening[port], port, want)
		}
	}
	return lag
}

// TestBeginPauseAcceptReachesAnIdleLoop886: a pause must close every loop's
// listener within lagBound, whatever the loop is waiting for. The default arm
// is the guard that the 25 ms timerfd keeps the lag short; the arm without a
// header deadline has no timerfd and sits in epoll_wait for up to 500 ms, which
// is the one the eventfd signal is for.
func TestBeginPauseAcceptReachesAnIdleLoop886(t *testing.T) {
	const (
		cycles   = 8
		lagBound = 100 * time.Millisecond
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

			// Settle first: the wait grows with the empty iterations the loop has
			// had in a row (about 200 ms to reach its longest on epoll, 0.5 s on
			// io_uring), and the lag is worst at the longest.
			time.Sleep(1200 * time.Millisecond)
			var lags []time.Duration
			for range cycles {
				// Idle for a while first: the loop is then deep in its wait, not
				// just out of one, which is the worst case for the lag.
				time.Sleep(300 * time.Millisecond)
				lags = append(lags, pauseLag886(t, r.e, port, want, 3*time.Second))
			}
			t.Logf("celeris886 PAUSE-LAG [%s] (%d loops, %d live connections, BeginPauseAccept to no listener): %v, bound %v", tc.name, want, tc.conns, lags, lagBound)
			for i, lag := range lags {
				if lag > lagBound {
					t.Errorf("celeris886 PAUSE-LAG: cycle %d took %v to have no loop listening, want within %v: "+
						"an idle loop did not see the pause until its wait ended", i, lag, lagBound)
				}
			}
			get895(t, addr)
		})
	}
}
