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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pinnedThreads1381 returns the thread ids of this process that are pinned to
// one CPU: the workers' (LockOSThread and a one-CPU affinity). The other threads
// of the process may run on any CPU the process may use. A test takes the set
// before it starts its engine and the difference after: a thread pinned by an
// earlier test, if one were left, is not counted.
func pinnedThreads1381(t *testing.T) map[int]bool {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/task")
	if err != nil {
		t.Fatalf("read /proc/self/task: %v", err)
	}
	tids := map[int]bool{}
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
					tids[tid] = true
				}
			}
		}
	}
	return tids
}

// newPinned1381 is the pinned threads that are not in before.
func newPinned1381(t *testing.T, before map[int]bool) []int {
	t.Helper()
	var out []int
	for tid := range pinnedThreads1381(t) {
		if !before[tid] {
			out = append(out, tid)
		}
	}
	slices.Sort(out)
	return out
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

// An idle worker waits on its ring at most idleWakeBound1381 times a second.
// Before the change an idle io_uring worker woke 54 to 62 times a second with two
// workers and 41 with the one worker the Unit job's 8 MiB memlock funds (measured
// on CI, both architectures: the 25 ms cap is 40 a second, and what else wakes
// it), after it 10 to 11. The bound is the one the failing-first run used and saw
// main's code fail: 2.3 times over the fix's reading and 1.6 times under main's
// single-worker figure.
const idleWakeBound1381 = 25

// ringWaits1381 counts the ring waits each worker of the process makes, and keeps
// the wait tier (Worker.baseTimeout) of the last one, through ringWaitSeam: the
// seam passes each wait's own result through, and runs on the worker's thread, so
// it reads what the worker owns. A wait that ends is a wake-up of the loop, which
// is what the idle wait is about. The test used to count the voluntary context
// switches of the worker's thread instead, and those are the loop's waits plus
// what the Go runtime adds besides: on CI (both architectures, 36 windows of 2 s
// each, two workers) the loop made 20 to 23 waits in every window and the thread's switches
// were 20 to 47 in the same windows (the bound is 50); a run of the whole package
// read 53 (celeris#1484). The wait that ends after the loop's idle rounds is the
// thing the bound is about, and the thread's other switches are not.
// What the test no longer catches: wake-ups of the worker's thread that are not
// the loop's own (the Go runtime's, a timer, another goroutine on that thread).
// The idle CPU and wake-up figures of the documentation are the check for those.
type ringWaits1381 struct{ m sync.Map } // *Worker to *waitStat1381

type waitStat1381 struct {
	n    atomic.Int64 // ring waits that ended
	base atomic.Int64 // baseTimeout when the last one ended, in nanoseconds
}

func (rw *ringWaits1381) stat(w *Worker) *waitStat1381 {
	if v, ok := rw.m.Load(w); ok {
		return v.(*waitStat1381)
	}
	v, _ := rw.m.LoadOrStore(w, &waitStat1381{})
	return v.(*waitStat1381)
}

// countRingWaits1381 installs the counting seam for the test. Call it before the
// engine starts: the engine's own cleanup (registered later, run first) stops its
// workers before the seam is put back.
func countRingWaits1381(t *testing.T) *ringWaits1381 {
	t.Helper()
	rw := &ringWaits1381{}
	fn := func(w *Worker, _ ringWaitMode, err error) error {
		s := rw.stat(w)
		s.n.Add(1)
		s.base.Store(int64(w.baseTimeout()))
		return err
	}
	old := ringWaitSeam.Load()
	ringWaitSeam.Store(&fn)
	t.Cleanup(func() { ringWaitSeam.Store(old) })
	return rw
}

// TestIdleWorkerWakeupsAreBounded1381 fails before the change. A worker takes the
// long wait only after 100 empty rounds (about 0.6 s, 1 ms and 5 ms rounds first),
// so the test waits until each worker's last ring wait was the long one (100 ms)
// before it counts, and counts the loop's waits (ringWaits1381). The voluntary
// context switches of the pinned threads are logged beside them and not asserted.
func TestIdleWorkerWakeupsAreBounded1381(t *testing.T) {
	rw := countRingWaits1381(t)
	before := pinnedThreads1381(t)
	r := startPause886(t, 2, 0) // the default ReadHeaderTimeout: the 25 ms cap
	r.e.mu.Lock()
	ws := slices.Clone(r.e.workers)
	r.e.mu.Unlock()
	inLongWait := func() bool {
		for _, w := range ws {
			if rw.stat(w).base.Load() != int64(100*time.Millisecond) {
				return false
			}
		}
		return true
	}
	// The settle is a poll for that state, bounded in time, not a sleep: a
	// worker that never gets there (the 25 ms cap kept) is measured as it is.
	settleBy := time.Now().Add(10 * time.Second)
	for !inLongWait() && time.Now().Before(settleBy) {
		time.Sleep(10 * time.Millisecond) // a poll interval, not a synchronisation
	}
	settled := inLongWait()
	tids := newPinned1381(t, before)
	if len(tids) != len(ws) {
		t.Fatalf("celeris1381 PREMISE: %d threads pinned to one CPU, want one per worker (%d)", len(tids), len(ws))
	}
	const window = 2 * time.Second
	waits0 := make([]int64, len(ws))
	for i, w := range ws {
		waits0[i] = rw.stat(w).n.Load()
	}
	switches := wakeups1381(t, tids, window) // sleeps the window
	got := make([]int64, len(ws))
	for i, w := range ws {
		got[i] = rw.stat(w).n.Load() - waits0[i]
	}
	t.Logf("celeris1381 WAKEUPS %d idle workers, ring waits in %v: %v (bound %d per second per worker); voluntary context switches of their threads, not asserted: %v; in the long wait before the window: %v",
		len(ws), window, got, idleWakeBound1381, switches, settled)
	if !settled {
		t.Errorf("a worker that holds nothing was not in its long wait (100 ms) within 10 s of its start: it keeps the 25 ms cap")
	}
	for i, n := range got {
		if rate := float64(n) / window.Seconds(); rate > idleWakeBound1381 {
			t.Errorf("worker %d woke %.0f times a second while idle, want at most %d: it keeps the 25 ms cap with nothing to serve", i, rate, idleWakeBound1381)
		}
	}
}

// A worker that holds a connection keeps its 25 ms cap: the worker that holds
// the one keep-alive connection wakes at least liveWakeFloor1381 times a second.
// This passes before the change and fails if the long wait ever reaches a
// worker that holds a connection.
const liveWakeFloor1381 = 25

func TestWorkerWithAConnectionKeepsTheShortWait1381(t *testing.T) {
	before := pinnedThreads1381(t)
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
	tids := newPinned1381(t, before)
	r.e.mu.Lock()
	if len(tids) != len(r.e.workers) {
		r.e.mu.Unlock()
		t.Fatalf("celeris1381 PREMISE: %d threads pinned to one CPU, want one per worker", len(tids))
	}
	r.e.mu.Unlock()
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
