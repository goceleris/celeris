//go:build linux

package iouring

// celeris#1381. An io_uring worker with nothing to serve woke about 40 times a
// second: its idle ring wait is 100 ms, but the default ReadHeaderTimeout caps
// it at 25 ms, with no connection for a header deadline to watch. The change
// lets a worker that holds no connection of any kind, and nothing queued, keep
// the 100 ms wait; a worker with any connection keeps the 25 ms cap. These tests
// are written against identifiers that exist before the change, so they compile
// and run on the code before it; only the first is expected to fail there. The
// bounds are counts of voluntary context switches over seconds (a descheduled
// thread makes involuntary ones) and medians over several cycles.

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// workerThreads1381 returns the thread ids of this process that are pinned to
// one CPU: the workers' (LockOSThread and a one-CPU affinity). The other
// threads of the process may run on any CPU the process may use.
func workerThreads1381(t *testing.T) []int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/task")
	if err != nil {
		t.Fatalf("read /proc/self/task: %v", err)
	}
	var tids []int
	for _, ent := range ents {
		tid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/self/task/" + ent.Name() + "/status")
		if err != nil {
			continue // the thread has exited
		}
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
				if v = strings.TrimSpace(v); !strings.ContainsAny(v, "-,") {
					tids = append(tids, tid)
				}
			}
		}
	}
	return tids
}

func voluntary1381(t *testing.T, tid int) int64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/task/" + strconv.Itoa(tid) + "/status")
	if err != nil {
		t.Fatalf("read thread status: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "voluntary_ctxt_switches:"); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return n
		}
	}
	t.Fatalf("no voluntary_ctxt_switches in the status of thread %d", tid)
	return 0
}

// wakeups1381 counts the voluntary context switches of each thread over window.
func wakeups1381(t *testing.T, tids []int, window time.Duration) []int64 {
	t.Helper()
	before := make([]int64, len(tids))
	for i, tid := range tids {
		before[i] = voluntary1381(t, tid)
	}
	time.Sleep(window) // the measurement window, not a synchronisation
	out := make([]int64, len(tids))
	for i, tid := range tids {
		out[i] = voluntary1381(t, tid) - before[i]
	}
	return out
}

// settle1381 is the time an idle worker needs to reach its longest wait: 10
// empty rounds of 1 ms, then 90 of 5 ms, then the longest (about 0.5 s). It is a
// warm-up before a measurement, not a synchronisation.
const settle1381 = 1500 * time.Millisecond

// settleShort1381 is enough for a worker to have left its 1 ms and 5 ms rounds for a test that
// does not measure the wait itself.
const settleShort1381 = 700 * time.Millisecond

func ok1381(t *testing.T, addr string) time.Duration {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	t0 := time.Now()
	if _, err := fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	return time.Since(t0)
}

// An idle worker blocks at most idleWakeBound1381 times a second. Before the
// change an idle io_uring worker woke about 40 times a second (measured), after
// it about 10. The bound sits between them, nearer the 40.
const idleWakeBound1381 = 25

// TestIdleWorkerWakeupsAreBounded1381 fails before the change.
func TestIdleWorkerWakeupsAreBounded1381(t *testing.T) {
	r := startPause886(t, 2, 0) // the default ReadHeaderTimeout: the 25 ms cap
	r.e.mu.Lock()
	want := len(r.e.workers)
	r.e.mu.Unlock()
	time.Sleep(settle1381)
	tids := workerThreads1381(t)
	if len(tids) != want {
		t.Fatalf("celeris1381 PREMISE: %d threads pinned to one CPU, want one per worker (%d)", len(tids), want)
	}
	const window = 2 * time.Second
	got := wakeups1381(t, tids, window)
	t.Logf("celeris1381 WAKEUPS %d idle workers, voluntary context switches in %v: %v (bound %d per second per worker)", want, window, got, idleWakeBound1381)
	for i, n := range got {
		if rate := float64(n) / window.Seconds(); rate > idleWakeBound1381 {
			t.Errorf("worker thread %d woke %.0f times a second while idle, want at most %d: it keeps the 25 ms cap with nothing to serve", tids[i], rate, idleWakeBound1381)
		}
	}
}

// A worker that holds a connection keeps its 25 ms cap: the worker that holds
// the one keep-alive connection wakes at least liveWakeFloor1381 times a second.
// This passes before the change and fails if the long wait ever reaches a
// worker that holds a connection.
const liveWakeFloor1381 = 25

func TestWorkerWithAConnectionKeepsTheShortWait1381(t *testing.T) {
	r := startPause886(t, 2, 0)
	addr := r.e.Addr().String()
	time.Sleep(settle1381)
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("response: %v", err)
	}
	_ = resp.Body.Close()
	time.Sleep(settle1381) // the worker that holds the connection settles into its waits again
	tids := workerThreads1381(t)
	const window = 2 * time.Second
	got := wakeups1381(t, tids, window)
	top := slices.Max(got)
	t.Logf("celeris1381 LIVE-WAKEUPS voluntary context switches in %v, one connection held: %v (the busiest must be at least %d per second)", window, got, liveWakeFloor1381)
	if rate := float64(top) / window.Seconds(); rate < liveWakeFloor1381 {
		t.Errorf("the worker that holds a connection woke %.0f times a second, want at least %d: it took the long wait with a connection", rate, liveWakeFloor1381)
	}
}

// The first connection after a long idle is accepted and answered at once: the
// accept is a completion of the ring.
func TestFirstRequestAfterIdleIsNotStalled1381(t *testing.T) {
	r := startPause886(t, 2, 0)
	addr := r.e.Addr().String()
	var lat []time.Duration
	for range 3 {
		time.Sleep(settleShort1381)
		lat = append(lat, ok1381(t, addr))
	}
	s := slices.Clone(lat)
	slices.Sort(s)
	t.Logf("celeris1381 FIRST-REQUEST after idle: %v, median %v", lat, s[len(s)/2])
	if m := s[len(s)/2]; m > 40*time.Millisecond {
		t.Errorf("the first request after an idle took a median of %v, want at most 40ms", m)
	}
}

// A cancelled context ends an idle engine at once: the engine signals each
// worker's wake eventfd, whose poll the ring keeps armed.
func TestIdleShutdownIsPrompt1381(t *testing.T) {
	var lags []time.Duration
	for range 3 {
		r := startPause886(t, 2, 0)
		time.Sleep(settleShort1381)
		t0 := time.Now()
		if err := r.stop(t); err != nil {
			t.Fatalf("Listen: %v", err)
		}
		lags = append(lags, time.Since(t0))
	}
	s := slices.Clone(lags)
	slices.Sort(s)
	t.Logf("celeris1381 IDLE-SHUTDOWN (cancel to Listen returning): %v, median %v", lags, s[len(s)/2])
	if m := s[len(s)/2]; m > 30*time.Millisecond {
		t.Errorf("an idle engine took a median of %v to stop, want at most 30ms", m)
	}
}
