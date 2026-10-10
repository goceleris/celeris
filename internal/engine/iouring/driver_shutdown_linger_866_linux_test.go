//go:build linux

package iouring

// celeris#866: shutdownDrivers closed each lingering driver conn's engine
// descriptor (opFD) one at a time on the worker goroutine, so with N
// SO_LINGER sockets holding unsent data for peers that do not read, the worker
// spent N x linger inside shutdownDrivers, no CQE processed, before the
// shutdown went on. The closes now run in parallel (bounded), and the
// worker waits for the slowest instead of the sum.
//
// A close that should linger does not always do so: the kernel leaves the wait
// early on a pending signal (see lingeringCloseAttempt), and an attempt in
// which any close has returned by the time it is sampled proves nothing, so it
// is void and tried again, as the celeris#735 tests do. The gate counts closes
// that are in their linger wait at the same moment, not wall time: a serial
// close has at most one, and a wall-time bound would pass on luck whenever the
// linger ended early.

import (
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lingerSet866 is n lingering driver conns registered by hand on a bare
// Worker (shutdownDrivers needs only the driver map, the driver-action queue
// and the closer count; no loop runs), each with its own socket: opFD is the
// descriptor shutdownDrivers closes.
type lingerSet866 struct {
	w       *Worker
	fds     []int
	ids     []socketIdentity
	drains  []func()
	mu      sync.Mutex
	fired   map[int]error // fd -> the onClose error
	closedB map[int]bool  // fd -> the engine's descriptor was already closed when onClose ran
	nFired  atomic.Int32
}

func newLingerSet866(t testing.TB, n, linger int) *lingerSet866 {
	t.Helper()
	s := &lingerSet866{
		w:       &Worker{driverConns: make(map[int]*driverConn, n)},
		fired:   make(map[int]error, n),
		closedB: make(map[int]bool, n),
	}
	for range n {
		fd, drain := deeplyLingeringTCPSocket(t, linger)
		id := identityOf(t, fd)
		s.fds = append(s.fds, fd)
		s.ids = append(s.ids, id)
		s.drains = append(s.drains, drain)
		fd0 := fd
		s.w.driverConns[fd] = &driverConn{
			fd: fd, opFD: fd, opFDOpen: true, w: s.w,
			onClose: func(err error) {
				s.mu.Lock()
				s.fired[fd0] = err
				s.closedB[fd0] = !stillNames(fd0, id)
				s.mu.Unlock()
				s.nFired.Add(1)
			},
		}
	}
	s.w.hasDriverConns.Store(true)
	return s
}

func (s *lingerSet866) drainAll() {
	for _, d := range s.drains {
		d()
	}
}

// classify reports, for each socket, whether its close is in its linger wait,
// has returned, or has not started (the number still names the socket).
func (s *lingerSet866) classify(t testing.TB) (waiting, returned, notStarted int) {
	t.Helper()
	for i, fd := range s.fds {
		switch w, _ := closeInLingerWait(t, s.ids[i]); {
		case stillNames(fd, s.ids[i]):
			notStarted++
		case w:
			waiting++
		default:
			returned++
		}
	}
	return
}

// TestShutdownDriversClosesLingeringDescriptorsInParallel866 is the gate: n
// lingering driver conns, shutdownDrivers running; half a linger in, every
// close must be in its wait at once. On the serial close one is, and the
// others have not begun.
func TestShutdownDriversClosesLingeringDescriptorsInParallel866(t *testing.T) {
	const (
		n      = 4
		linger = 2 // seconds
	)
	void := 0
	for attempt := 1; attempt <= lingerAttempts; attempt++ {
		verdict, ok := shutdownLingerAttempt866(t, n, linger)
		if !ok {
			void++
			t.Logf("celeris866 GATE attempt=%d void (a close returned early: %s)", attempt, verdict)
			continue
		}
		t.Logf("celeris866 GATE attempt=%d void=%d %s", attempt, void, verdict)
		return
	}
	t.Fatalf("apparatus: no attempt of %d had every close either lingering or not begun (%d void)", lingerAttempts, void)
}

// shutdownLingerAttempt866 is one attempt; ok is false when it is void. The
// failures it reports (t.Errorf) are the gate's.
func shutdownLingerAttempt866(t *testing.T, n, linger int) (verdict string, ok bool) {
	t.Helper()
	s := newLingerSet866(t, n, linger)
	drained := false
	defer func() {
		if !drained {
			s.drainAll()
		}
	}()
	done := make(chan struct{})
	go func() {
		s.w.shutdownDrivers()
		s.w.waitDriverCloses()
		close(done)
	}()
	time.Sleep(time.Duration(linger) * time.Second / 2)
	waiting, returned, notStarted := s.classify(t)
	verdict = "waiting=" + strconv.Itoa(waiting) + " returned=" + strconv.Itoa(returned) + " notStarted=" + strconv.Itoa(notStarted)
	if returned > 0 || waiting == 0 {
		s.drainAll()
		drained = true
		<-done
		return verdict, false
	}
	if waiting != n {
		t.Errorf("shutdownDrivers closed the %d lingering descriptors one after another: %s, want all %d in their linger wait together "+
			"(the worker waits N x linger instead of the longest)", n, verdict, n)
	}
	s.drainAll()
	drained = true
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("shutdownDrivers and waitDriverCloses did not return after the peers drained")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.fired) != n {
		t.Errorf("onClose fired for %d of %d conns", len(s.fired), n)
	}
	for fd, err := range s.fired {
		if err != errEngineShutdown {
			t.Errorf("fd %d: onClose(%v), want errEngineShutdown", fd, err)
		}
		if !s.closedB[fd] {
			t.Errorf("fd %d: onClose ran while the engine's descriptor still named the socket: a driver must see the socket closed first", fd)
		}
	}
	return verdict, true
}

// BenchmarkShutdownDriversLinger866 measures how the time shutdownDrivers and
// waitDriverCloses take grows with the number of lingering driver conns: n
// sockets, each with SO_LINGER {1, CELERIS_866_LINGER seconds (default 1)} and
// megabytes unsent to a peer that never reads. Run with -test.benchtime 1x
// (scripts/scale-866.sh): one shutdown per sub-benchmark, its wall time in
// ms/total and the part spent inside shutdownDrivers itself in ms/shutdownDrivers.
// A benchmark and not a skipped test, so the package's -v run, which fails on
// any skip, is not affected.
func BenchmarkShutdownDriversLinger866(b *testing.B) {
	linger := 1
	if v := os.Getenv("CELERIS_866_LINGER"); v != "" {
		linger, _ = strconv.Atoi(v)
	}
	for _, n := range []int{1, 2, 4, 8, 16} {
		b.Run("n="+strconv.Itoa(n), func(b *testing.B) {
			for range b.N {
				b.StopTimer()
				s := newLingerSet866(b, n, linger)
				b.StartTimer()
				start := time.Now()
				s.w.shutdownDrivers()
				inShutdown := time.Since(start)
				s.w.waitDriverCloses()
				total := time.Since(start)
				b.StopTimer()
				s.drainAll()
				if got := s.nFired.Load(); int(got) != n {
					b.Fatalf("onClose fired %d times, want %d", got, n)
				}
				b.ReportMetric(float64(inShutdown.Milliseconds()), "ms/shutdownDrivers")
				b.ReportMetric(float64(total.Milliseconds()), "ms/total")
				b.StartTimer()
			}
		})
	}
}

// TestCloseFDsInParallelBound866: closeFDsInParallel runs every close exactly
// once, on no more than the bound at a time, and does run that many at a time
// (a serial or a smaller pool would pass a test that checked only the maximum).
func TestCloseFDsInParallelBound866(t *testing.T) {
	defer watchdog959(t, time.Minute)()
	const workers, conns = 8, 100
	dcs := make([]*driverConn, conns)
	for i := range dcs {
		dcs[i] = &driverConn{opFD: i}
	}
	var (
		mu       sync.Mutex
		seen     = make(map[int]int, conns)
		cur, max int
		reached  = make(chan struct{})
		once     sync.Once
	)
	closeFn := func(fd int) {
		mu.Lock()
		seen[fd]++
		cur++
		if cur > max {
			max = cur
		}
		if cur == workers {
			once.Do(func() { close(reached) })
		}
		mu.Unlock()
		// Hold every close until `workers` of them are in flight, then a
		// little longer, as lingering closes would be: a pool larger than the
		// bound has its extra goroutines in here by then.
		select {
		case <-reached:
		case <-time.After(5 * time.Second):
		}
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		cur--
		mu.Unlock()
	}
	closeFDsInParallel(dcs, workers, closeFn)
	if max != workers {
		t.Errorf("at most %d closes were in flight at once, want exactly the bound %d", max, workers)
	}
	for fd := 0; fd < conns; fd++ {
		if seen[fd] != 1 {
			t.Errorf("fd %d closed %d times, want once", fd, seen[fd])
		}
	}
	if cur != 0 {
		t.Errorf("%d closes still in flight after closeFDsInParallel returned", cur)
	}
	closeFDsInParallel(nil, workers, func(int) { t.Error("closeFn called for no conns") })
	if shutdownDriverCloseWorkers < 2 || shutdownDriverCloseWorkers > 1024 {
		t.Errorf("shutdownDriverCloseWorkers = %d: outside the range that is parallel yet far from the thread limit", shutdownDriverCloseWorkers)
	}
}
