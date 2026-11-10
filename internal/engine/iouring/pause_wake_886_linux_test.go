//go:build linux

package iouring

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
	"github.com/goceleris/celeris/internal/wakefd"
)

// startPause886 is start895 on a loopback port 0 with DisableDeferAccept (a
// pause closes at once) and a ReadHeaderTimeout: a negative one disables the
// header deadline, and with it the 25 ms cap on the worker's ring wait, which
// leaves the 100 ms of Worker.baseTimeout.
func startPause886(t *testing.T, workers int, readHeaderTimeout time.Duration) *run895 {
	t.Helper()
	needWorkers895(t, 1)
	var run *run895
	_, _, err := retryRingENOMEM(func() error {
		e, err := New(resource.Config{
			Addr:               "127.0.0.1:0",
			Engine:             engine.IOUring,
			Protocol:           engine.HTTP1,
			Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
			DisableDeferAccept: true,
			ReadHeaderTimeout:  readHeaderTimeout,
			Resources:          resource.Resources{Workers: workers},
		}, respondingHandler{})
		if err != nil {
			skipOrFail656(t, "New: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- e.Listen(ctx) }()
		for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); {
			if e.Addr() != nil {
				run = &run895{e: e, cancel: cancel, done: done}
				t.Cleanup(func() { cancel(); <-done })
				return nil
			}
			select {
			case lerr := <-done:
				cancel()
				return lerr
			case <-time.After(time.Millisecond):
			}
		}
		cancel()
		<-done
		t.Fatalf("engine never published an address")
		return nil
	})
	if err != nil {
		if ioUringUnavailable639(err) {
			skipUnlessIOUringUnusable(t, err)
		}
		t.Fatalf("engine start: %v", err)
	}
	return run
}

// celeris#886 item 24 (and the flap remainder of item 11). BeginPauseAccept
// stored the flag and woke nobody: a worker that is idle and still listening
// saw the pause only when its ring wait ended (Worker.baseTimeout: 100 ms, 50 ms
// with detached conns, 25 ms with a ReadHeaderTimeout), so its listener closed
// late. The resume side got the wake eventfd in #1013; this is the pause side.
// The test is #1013's lag test run the other way: the pause is
// DisableDeferAccept's (its listener closes at once, there is no linger), and
// the lag is the time from BeginPauseAccept until the kernel's table shows no
// listener of the engine.

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
			t.Fatalf("celeris886: %d workers listening on port %d 3s after the resume, want %d", census895(t).listening[port], port, want)
		}
	}
	return lag
}

// TestBeginPauseAcceptReachesAnIdleWorker886: a pause must close every
// worker's listener within lagBound, whatever the worker is waiting for. Two
// workers are asked for: with memlock raised both start, and a fix that
// signalled only one of them fails; at the runner's 8 MiB one starts (see the
// PREMISE check).
func TestBeginPauseAcceptReachesAnIdleWorker886(t *testing.T) {
	const cycles = 8
	for _, tc := range []struct {
		name     string
		rht      time.Duration
		conns    int
		lagBound time.Duration
	}{
		// The default arms are the guard that the 25 ms cap of a ReadHeaderTimeout
		// keeps the lag short; the others have the 100 ms wait the signal is for.
		// The bound is loose on purpose (CI runs this under -race on two vCPUs):
		// the lag is a few ms with the signal, and up to 99 ms without it
		// (measured), a tail too rare to fail on reliably. The deterministic test
		// below is the one that fails before the change.
		{"default-read-header-timeout/live-connections", 0, 16, 100 * time.Millisecond},
		{"no-read-header-timeout/live-connections", -1, 16, 100 * time.Millisecond},
		{"default-read-header-timeout/no-connections", 0, 0, 100 * time.Millisecond},
		{"no-read-header-timeout/no-connections", -1, 0, 100 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lagBound := tc.lagBound
			r := startPause886(t, 2, tc.rht)
			r.e.mu.Lock()
			want := len(r.e.workers)
			r.e.mu.Unlock()
			if maxW := MaxWorkersForMemlock(); want < 2 && (maxW < 0 || maxW >= 2) {
				t.Fatalf("celeris886 PREMISE: %d io_uring worker(s) started with memlock funding %d (-1: unlimited), want 2", want, maxW)
			}
			port := portOf895(t, r.e.Addr())
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			live886(t, addr, tc.conns)

			// Settle first: the wait grows with the empty iterations the worker has
			// had in a row (about 200 ms to reach its longest on epoll, 0.5 s on
			// io_uring), and the lag is worst at the longest.
			time.Sleep(1200 * time.Millisecond)
			var lags []time.Duration
			for range cycles {
				// Idle for a while first: the worker is then deep in its ring
				// wait, not just out of one, the worst case for the lag.
				time.Sleep(300 * time.Millisecond)
				lags = append(lags, pauseLag886(t, r.e, port, want, 3*time.Second))
			}
			t.Logf("celeris886 PAUSE-LAG [%s] (%d workers, %d live connections, BeginPauseAccept to no listener): %v, bound %v", tc.name, want, tc.conns, lags, lagBound)
			for i, lag := range lags {
				if lag > lagBound {
					t.Errorf("celeris886 PAUSE-LAG: cycle %d took %v to have no worker listening, want within %v: "+
						"an idle worker did not see the pause until its wait ended", i, lag, lagBound)
				}
			}
			get895(t, addr)
		})
	}
}

// TestBeginPauseAcceptSignalsEveryWorkerWakeFD886 is the deterministic half:
// the pause must write the wake eventfd of every worker, which is what ends
// the ring wait of a worker that has not parked. The end-to-end lag above
// depends on where in its wait each worker is, so it cannot fail reliably
// before the change; the eventfd counter can.
func TestBeginPauseAcceptSignalsEveryWorkerWakeFD886(t *testing.T) {
	e := &Engine{}
	var efds []int
	for range 3 {
		efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
		if err != nil {
			t.Fatalf("eventfd: %v", err)
		}
		t.Cleanup(func() { _ = unix.Close(efd) })
		efds = append(efds, efd)
		e.workers = append(e.workers, &Worker{wakeFD: wakefd.New(efd)})
	}
	e.BeginPauseAccept()
	if !e.acceptPaused.Load() {
		t.Fatal("BeginPauseAccept did not set the pause")
	}
	for i, efd := range efds {
		var buf [8]byte
		if n, err := unix.Read(efd, buf[:]); err != nil || n != 8 || buf == [8]byte{} {
			t.Errorf("celeris886 worker %d: its wake eventfd was not signalled by BeginPauseAccept (read n=%d err=%v): "+
				"an idle worker sees the pause only when its ring wait ends", i, n, err)
		}
	}
}
