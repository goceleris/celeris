//go:build linux

package epoll

// celeris#1381. An epoll loop with nothing to serve woke about 285 times a
// second (the 4 ms epoll_wait, and the 25 ms sweep timer through the same
// epoll set), and the process cost followed. The change makes a loop that holds
// no connection of any kind, and nothing queued, wait 100 ms and tick its sweep
// timer every 100 ms; a loop with any connection keeps the 25 ms sweep.
//
// What these tests guard is the other side of the same change: everything that
// used to be seen within a few milliseconds by a loop that is idle must still be
// seen as promptly (a context cancel, a pause, an accept, an adopt, a driver
// registration, a connection that arrives and goes), and a loop that holds a
// connection must keep its sweep cadence. They are written against the loop's
// own identifiers only (the sweep hook, consecutiveEmpty), so they compile and
// run on the code before the change; only the first is expected to fail there.
//
// Two ways a CI runner could make a bound here flaky are avoided. A bound on a
// lag is taken on the MEDIAN of several cycles and the cycle starts right after
// a sweep tick, when the loop's next wake is a full wait away: without the wake
// the lag of a cycle is then close to 100 ms and the median is not luck, with it
// the lag is a few milliseconds, and one stalled cycle does not move a median.
// And a rate is counted over a window of seconds as a count of the loop
// thread's voluntary context switches, which a loaded runner does not raise (a
// descheduled thread is an involuntary one).

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

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// idleStat1381 is what the sweep hook records about one loop, from the loop's
// own thread.
type idleStat1381 struct {
	tid        atomic.Int64 // the loop's thread, for /proc/self/task/<tid>
	ticks      atomic.Int64 // sweeps made
	empties    atomic.Int64 // consecutiveEmpty at the last sweep
	liveSweeps atomic.Int64 // sweeps made while the loop held a connection
}

type idleProbe1381 struct{ stats sync.Map } // *Loop -> *idleStat1381

func (p *idleProbe1381) of(l *Loop) *idleStat1381 {
	if s, ok := p.stats.Load(l); ok {
		return s.(*idleStat1381)
	}
	s, _ := p.stats.LoadOrStore(l, &idleStat1381{})
	return s.(*idleStat1381)
}

// probe1381 installs the sweep hook for the test. The hook is global, and the
// tests of this file do not run in parallel.
func probe1381(t *testing.T) *idleProbe1381 {
	t.Helper()
	p := &idleProbe1381{}
	h := func(l *Loop) {
		s := p.of(l)
		s.tid.Store(int64(unix.Gettid()))
		s.ticks.Add(1)
		s.empties.Store(int64(l.consecutiveEmpty))
		if len(l.liveConns) > 0 {
			s.liveSweeps.Add(1)
		}
	}
	testHookCheckTimeouts.Store(&h)
	t.Cleanup(func() { testHookCheckTimeouts.Store(nil) })
	return p
}

// idleEmpties1381 is how many empty rounds in a row a loop must have had to be
// "idle" for these tests: the longest tier of its wait starts after 100.
const idleEmpties1381 = 101

// waitIdle1381 returns once every loop has made a sweep with more than 100 empty
// rounds behind it, which is the state in which the change takes its long wait.
// The loops make that sweep themselves (the timer ticks whatever else is going
// on), so this is a wait for an event, not a sleep.
func waitIdle1381(t *testing.T, p *idleProbe1381, loops []*Loop) {
	t.Helper()
	for dl := time.Now().Add(30 * time.Second); time.Now().Before(dl); time.Sleep(2 * time.Millisecond) {
		ok := true
		for _, l := range loops {
			if p.of(l).empties.Load() < idleEmpties1381 {
				ok = false
				break
			}
		}
		if ok {
			return
		}
	}
	t.Fatalf("the loops did not reach %d empty rounds in 30 s", idleEmpties1381)
}

// afterTick1381 waits until loop l has made a sweep that left it idle, and
// returns at once after it: the loop is then at the start of a wait, with its
// next sweep and its next timeout a full period away.
func afterTick1381(t *testing.T, p *idleProbe1381, l *Loop) {
	t.Helper()
	s := p.of(l)
	seen := s.ticks.Load()
	for dl := time.Now().Add(30 * time.Second); time.Now().Before(dl); time.Sleep(200 * time.Microsecond) {
		if s.ticks.Load() != seen && s.empties.Load() >= idleEmpties1381 {
			return
		}
	}
	t.Fatalf("loop %d made no sweep in 30 s", l.id)
}

// voluntary1381 reads the voluntary context switches of a thread of this
// process: one for every time it blocked, in epoll_wait for a loop.
func voluntary1381(t *testing.T, tid int64) int64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/task/" + strconv.FormatInt(tid, 10) + "/status")
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

// wakeups1381 counts, per loop, the voluntary context switches of the loop's
// thread over window.
func wakeups1381(t *testing.T, p *idleProbe1381, loops []*Loop, window time.Duration) []int64 {
	t.Helper()
	before := make([]int64, len(loops))
	for i, l := range loops {
		before[i] = voluntary1381(t, p.of(l).tid.Load())
	}
	time.Sleep(window) // the measurement window, not a synchronisation
	out := make([]int64, len(loops))
	for i, l := range loops {
		out[i] = voluntary1381(t, p.of(l).tid.Load()) - before[i]
	}
	return out
}

func median1381(d []time.Duration) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[len(s)/2]
}

// An idle loop blocks (wakes) at most idleWakeBound1381 times a second. Before
// the change an idle epoll loop woke about 285 times a second (measured:
// 570 in 2 s per loop); after it about 10. The bound sits between them.
const idleWakeBound1381 = 40

// TestIdleLoopWakeupsAreBounded1381 is the test that fails before the change:
// three idle loops (the case the documentation measures), counted for 2 s once
// every one of them has settled into its idlest wait.
func TestIdleLoopWakeupsAreBounded1381(t *testing.T) {
	p := probe1381(t)
	r := start886(t, 3, 0, false)
	loops := r.e.loopSnapshot()
	waitIdle1381(t, p, loops)
	const window = 2 * time.Second
	got := wakeups1381(t, p, loops, window)
	t.Logf("celeris1381 WAKEUPS %d idle loops, voluntary context switches in %v: %v (bound %d per second per loop)",
		len(loops), window, got, idleWakeBound1381)
	for i, n := range got {
		if rate := float64(n) / window.Seconds(); rate > idleWakeBound1381 {
			t.Errorf("loop %d woke %.0f times a second while idle, want at most %d: the loop does not take a long wait with nothing to serve",
				i, rate, idleWakeBound1381)
		}
	}
}

// A cancelled context ends an idle engine within idleStopBound1381 (median of
// several stops, each right after a tick). Before the change the idle shutdown
// took about 4.5 ms; a longer wait with no wake of its own takes the rest of the
// wait, about 100 ms after a tick.
const idleStopBound1381 = 30 * time.Millisecond

func TestIdleShutdownIsPrompt1381(t *testing.T) {
	const cycles = 5
	var lags []time.Duration
	for range cycles {
		p := probe1381(t)
		r := start886(t, 2, 0, false)
		loops := r.e.loopSnapshot()
		waitIdle1381(t, p, loops)
		afterTick1381(t, p, loops[0])
		t0 := time.Now()
		if err := r.stop(t); err != nil {
			t.Fatalf("Listen: %v", err)
		}
		lags = append(lags, time.Since(t0))
		testHookCheckTimeouts.Store(nil)
	}
	t.Logf("celeris1381 IDLE-SHUTDOWN (cancel to Listen returning, right after a tick): %v, median %v, bound %v", lags, median1381(lags), idleStopBound1381)
	if m := median1381(lags); m > idleStopBound1381 {
		t.Errorf("an idle engine took a median of %v to stop, want at most %v: a context cancel does not wake an idle loop", m, idleStopBound1381)
	}
}

// A pause reaches a loop in its wait: the listener of every loop is closed
// within idlePauseBound1381 (median of several cycles, each right after a tick).
// DisableDeferAccept (start886) makes the pause close at once.
const idlePauseBound1381 = 30 * time.Millisecond

func TestBeginPauseAcceptReachesALoopThatIsIdle1381(t *testing.T) {
	const cycles = 5
	p := probe1381(t)
	r := start886(t, 2, 0, false)
	loops := r.e.loopSnapshot()
	port := portOf895(t, r.e.Addr())
	var lags []time.Duration
	for range cycles {
		waitIdle1381(t, p, loops)
		afterTick1381(t, p, loops[0])
		lags = append(lags, pauseLag886(t, r.e, port, len(loops), 3*time.Second))
	}
	t.Logf("celeris1381 IDLE-PAUSE (BeginPauseAccept to no listener, right after a tick): %v, median %v, bound %v", lags, median1381(lags), idlePauseBound1381)
	if m := median1381(lags); m > idlePauseBound1381 {
		t.Errorf("a pause took a median of %v to reach an idle loop, want at most %v: BeginPauseAccept does not wake an idle loop", m, idlePauseBound1381)
	}
}

// ok1381 sends one request on a new connection and returns the time to the
// complete response.
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
	d := time.Since(t0)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	return d
}

// The first connection after a long idle is accepted and answered without
// waiting for the end of a wait: it is an epoll event of the listener.
const idleFirstBound1381 = 40 * time.Millisecond

func TestFirstRequestAfterIdleIsNotStalled1381(t *testing.T) {
	const requests = 5
	p := probe1381(t)
	r := start886(t, 2, 0, false)
	loops := r.e.loopSnapshot()
	addr := r.e.Addr().String()
	var lat []time.Duration
	for range requests {
		waitIdle1381(t, p, loops)
		afterTick1381(t, p, loops[0])
		lat = append(lat, ok1381(t, addr))
	}
	t.Logf("celeris1381 FIRST-REQUEST after idle: %v, median %v, bound %v", lat, median1381(lat), idleFirstBound1381)
	if m := median1381(lat); m > idleFirstBound1381 {
		t.Errorf("the first request after an idle took a median of %v, want at most %v", m, idleFirstBound1381)
	}
}

// liveSweepsPerSecond1381 is the least number of sweeps a second that a loop
// holding a connection must make: its timer is 25 ms, 40 a second, and the
// long wait of an idle loop would give 10.
const liveSweepsPerSecond1381 = 25

// liveSweeps1381 holds one keep-alive connection open on the engine for window
// and returns the sweeps made, by the loop that holds it, in that time.
func liveSweeps1381(t *testing.T, p *idleProbe1381, addr string, window time.Duration) int64 {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("response: %v", err)
	}
	_ = resp.Body.Close()
	var before int64
	p.stats.Range(func(_, v any) bool { before += v.(*idleStat1381).liveSweeps.Load(); return true })
	time.Sleep(window) // the measurement window, not a synchronisation
	var after int64
	p.stats.Range(func(_, v any) bool { after += v.(*idleStat1381).liveSweeps.Load(); return true })
	return after - before
}

// A loop that holds a connection sweeps every 25 ms, after a long idle as
// before it: the change must give the loop its short cadence back at the
// first connection.
func TestLoopSweepsEvery25msWhileItHoldsAConnectionAfterAnIdle1381(t *testing.T) {
	p := probe1381(t)
	r := start886(t, 2, 0, false)
	loops := r.e.loopSnapshot()
	waitIdle1381(t, p, loops)
	const window = time.Second
	n := liveSweeps1381(t, p, r.e.Addr().String(), window)
	t.Logf("celeris1381 LIVE-SWEEPS: %d sweeps in %v by the loop that holds one keep-alive connection (idle for >100 rounds before it), want at least %d",
		n, window, liveSweepsPerSecond1381)
	if n < liveSweepsPerSecond1381 {
		t.Errorf("the loop that held a connection made %d sweeps in %v, want at least %d: it kept the long wait", n, window, liveSweepsPerSecond1381)
	}
}

// A deadline set on a connection that arrives after a long idle is kept: an
// IdleTimeout of 1 s closes it at 1 s plus one sweep period (the bounds of
// celeris#1194).
func TestIdleTimeoutOfAConnectionAfterAnIdleIsOnTime1381(t *testing.T) {
	p := probe1381(t)
	eng, addr, closed := start1194(t, func(c *resource.Config) { c.IdleTimeout = time.Second })
	waitIdle1381(t, p, eng.loopSnapshot())
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("first response: %v", err)
	}
	_ = resp.Body.Close()
	closeWithin1194(t, "idle keep-alive conn after an idle loop (IdleTimeout 1 s)", closed, time.Now(), time.Second, time.Second)
}

// A connection that is reset before the loop has looked at it, while the loop
// is in its wait, leaves nothing behind: the engine counts no connection
// afterwards, and the next request is answered at once.
func TestConnectionResetWhileTheLoopIsIdle1381(t *testing.T) {
	p := probe1381(t)
	r := start886(t, 2, 0, false)
	loops := r.e.loopSnapshot()
	addr := r.e.Addr().String()
	waitIdle1381(t, p, loops)
	afterTick1381(t, p, loops[0])
	for range 8 {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		_ = c.(*net.TCPConn).SetLinger(0) // close sends an RST
		_ = c.Close()
	}
	for dl := time.Now().Add(5 * time.Second); r.e.Metrics().ActiveConnections != 0; time.Sleep(time.Millisecond) {
		if time.Now().After(dl) {
			t.Fatalf("the engine still counts %d connections 5 s after every one was reset", r.e.Metrics().ActiveConnections)
		}
	}
	if d := ok1381(t, addr); d > 10*idleFirstBound1381 {
		t.Errorf("the request after the resets took %v", d)
	}
}

// After its last connection has closed, a loop goes back to the long wait: the
// wake-up rate falls again, so that nothing a connection set (a count, a
// pending sweep) keeps the loop from being idle. Fails before the change, like
// the rate test.
func TestLoopIsIdleAgainAfterTheLastConnectionCloses1381(t *testing.T) {
	p := probe1381(t)
	r := start886(t, 2, 0, false)
	loops := r.e.loopSnapshot()
	addr := r.e.Addr().String()
	waitIdle1381(t, p, loops)
	for range 4 {
		ok1381(t, addr)
	}
	for dl := time.Now().Add(5 * time.Second); r.e.Metrics().ActiveConnections != 0; time.Sleep(time.Millisecond) {
		if time.Now().After(dl) {
			t.Fatalf("the engine still counts %d connections", r.e.Metrics().ActiveConnections)
		}
	}
	// Each loop settles again (more than 100 empty rounds) before the count.
	for _, l := range loops {
		p.of(l).empties.Store(0)
	}
	waitIdle1381(t, p, loops)
	const window = 2 * time.Second
	got := wakeups1381(t, p, loops, window)
	t.Logf("celeris1381 WAKEUPS after the last connection closed, %v: %v (bound %d per second per loop)", window, got, idleWakeBound1381)
	for i, n := range got {
		if rate := float64(n) / window.Seconds(); rate > idleWakeBound1381 {
			t.Errorf("loop %d woke %.0f times a second after its last connection closed, want at most %d", i, rate, idleWakeBound1381)
		}
	}
}

// A connection handed over by the other engine (an adopt) after a long idle is
// served at once, and its deadlines run at the short cadence.
func TestAdoptedConnectionAfterAnIdleIsServed1381(t *testing.T) {
	p := probe1381(t)
	eng, _, _ := start1194(t, func(c *resource.Config) { c.IdleTimeout = time.Second })
	loops := eng.loopSnapshot()
	waitIdle1381(t, p, loops)
	afterTick1381(t, p, loops[0])
	client, fd := adoptPair658(t)
	if err := eng.AdoptConn(fd, engine.Carryover{}); err != nil {
		_ = unix.Close(fd)
		t.Fatalf("AdoptConn: %v", err)
	}
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	t0 := time.Now()
	if _, err := fmt.Fprint(client, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("response on the adopted connection: %v", err)
	}
	_ = resp.Body.Close()
	if d := time.Since(t0); d > 10*idleFirstBound1381 {
		t.Errorf("the adopted connection's first response took %v", d)
	}
	var before int64
	p.stats.Range(func(_, v any) bool { before += v.(*idleStat1381).liveSweeps.Load(); return true })
	time.Sleep(time.Second) // the measurement window, not a synchronisation
	var after int64
	p.stats.Range(func(_, v any) bool { after += v.(*idleStat1381).liveSweeps.Load(); return true })
	t.Logf("celeris1381 ADOPT-SWEEPS: %d sweeps in 1 s by the loop that holds the adopted connection, want at least %d", after-before, liveSweepsPerSecond1381)
	if after-before < liveSweepsPerSecond1381 {
		t.Errorf("the loop that holds the adopted connection made %d sweeps in 1 s, want at least %d", after-before, liveSweepsPerSecond1381)
	}
}

// A driver connection registered on an idle loop receives its bytes at once.
func TestDriverConnectionOnAnIdleLoopReceives1381(t *testing.T) {
	p := probe1381(t)
	eng, stop := newTestEngine(t)
	t.Cleanup(stop)
	loops := eng.loopSnapshot()
	waitIdle1381(t, p, loops)
	afterTick1381(t, p, loops[0])
	wl := eng.WorkerLoop(0)
	local, peer := socketpairNonblocking(t)
	t.Cleanup(func() { _ = unix.Close(local); _ = unix.Close(peer) })
	got := make(chan struct{}, 1)
	if err := wl.RegisterConn(local, func([]byte) {
		select {
		case got <- struct{}{}:
		default:
		}
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	t0 := time.Now()
	if _, err := unix.Write(peer, []byte{'x'}); err != nil {
		t.Fatalf("write peer: %v", err)
	}
	select {
	case <-got:
		d := time.Since(t0)
		t.Logf("celeris1381 DRIVER-RECV on an idle loop: %v", d)
		if d > 10*idleFirstBound1381 {
			t.Errorf("a driver connection registered on an idle loop took %v to receive its first byte", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the registered driver connection received nothing in 5 s")
	}
	if err := wl.UnregisterConn(local); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
}
