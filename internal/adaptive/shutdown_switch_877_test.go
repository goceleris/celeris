//go:build linux

package adaptive

import (
	"context"
	"errors"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#877: Shutdown snapshots the sub-engines and hands them its ctx (the
// drain budget) before it cancels Listen. A standby built by a switch that runs
// after that snapshot is not in the snapshot, so it first sees Shutdown after
// its Listen ctx is cancelled: its drain has already started with no budget.
// And a switch that runs after Shutdown returns builds a standby that nothing
// shuts down. performSwitch must not build once Shutdown has started.

// trackedSub is a sub-engine test double. Shutdown records whether the Listen
// ctx the Engine gave it was still live when Shutdown first reached it, which
// is the budget the real epoll and io_uring sub-engines need.
type trackedSub struct {
	*mockEngine
	listenCtx   context.Context // the Listen ctx the Engine hands this sub
	onShutdown  func()          // runs at the first Shutdown call, before mu
	hookRunOnce atomic.Bool

	mu                sync.Mutex
	shutdowns         int
	firstShutdownLive bool // ctx was live at the first Shutdown call
}

func newTrackedSub(et engine.EngineType) *trackedSub {
	return &trackedSub{mockEngine: newMockEngine(et)}
}

func (s *trackedSub) Shutdown(_ context.Context) error {
	if s.onShutdown != nil && s.hookRunOnce.CompareAndSwap(false, true) {
		s.onShutdown()
	}
	s.mu.Lock()
	if s.shutdowns == 0 {
		s.firstShutdownLive = s.listenCtx.Err() == nil
	}
	s.shutdowns++
	s.mu.Unlock()
	return nil
}

func (s *trackedSub) state() (shutdowns int, firstLive bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shutdowns, s.firstShutdownLive
}

// newShutdownWindowEngine builds the lazy shape (epoll eager and active,
// io_uring built on the first switch) with Listen's ctx, wait group, cancel and
// done wired as Listen wires them, so Shutdown cancels the Listen ctx at the
// same step as in production. builds counts buildStandby calls.
func newShutdownWindowEngine(t *testing.T, buildHook func()) (e *Engine, active, lazy *trackedSub, builds *atomic.Int32) {
	t.Helper()
	active = newTrackedSub(engine.Epoll)
	lazy = newTrackedSub(engine.IOUring)
	builds = new(atomic.Int32)
	sampler := newSyntheticSampler()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	active.listenCtx = ctx
	lazy.listenCtx = ctx

	e = &Engine{
		primary:   active,
		secondary: nil,
		cfg:       resource.Config{Protocol: engine.HTTP1},
		logger:    testLogger(),
		startType: engine.Epoll,
	}
	e.buildStandby = func() (engine.Engine, error) {
		builds.Add(1)
		if buildHook != nil {
			buildHook()
		}
		return lazy, nil
	}
	e.ctrl = newController(e.primary, e.secondary, sampler, e.logger)
	e.ctrl.state.activeIsPrimary = true
	e.ctrl.cooldown = 0
	var ap engine.Engine = active
	e.active.Store(&ap)

	var wg sync.WaitGroup
	wg.Add(1)
	t.Cleanup(func() { wg.Done() })
	e.listenCtx = ctx
	e.listenWG = &wg
	e.listenCancel = cancel
	done := make(chan struct{})
	close(done) // the eval loop is not running in these tests
	e.listenDone = done
	return e, active, lazy, builds
}

// TestSwitchInsideShutdownSnapshotWindowBuildsNothing877 (window A): a switch
// that runs after Shutdown has taken its sub-engine snapshot and before it
// cancels Listen. The switch is started from inside the active sub-engine's
// first Shutdown, which Shutdown calls outside e.mu. Without the fix the
// standby is built and first sees Shutdown after its Listen ctx is cancelled.
func TestSwitchInsideShutdownSnapshotWindowBuildsNothing877(t *testing.T) {
	e, active, lazy, builds := newShutdownWindowEngine(t, nil)
	active.onShutdown = func() { e.ForceSwitch() }

	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if got := builds.Load(); got != 0 {
		t.Errorf("a switch that ran during Shutdown built %d standby(s), want 0 (#877)", got)
	}
	if shut, live := lazy.state(); shut > 0 && !live {
		t.Errorf("the standby built during Shutdown first saw Shutdown after its Listen ctx was cancelled: its drain had no budget (#877)")
	}
}

// TestSwitchAfterShutdownReturnsBuildsNothing877 (window B): a switch that runs
// after Shutdown has returned, as the eval loop can when the caller's ctx ends
// before Listen unwinds. The switch must not build a standby, because nothing
// would shut that standby down.
func TestSwitchAfterShutdownReturnsBuildsNothing877(t *testing.T) {
	e, _, _, builds := newShutdownWindowEngine(t, nil)

	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	e.ForceSwitch()

	if got := builds.Load(); got != 0 {
		t.Errorf("a switch after Shutdown returned built %d standby(s), want 0 (#877)", got)
	}
}

// TestSwitchInProgressWhenShutdownStartsKeepsItsBudget877 (positive case): a
// switch that is already building the standby when Shutdown starts. Shutdown
// waits for e.mu, so the standby is in its snapshot and gets Shutdown while its
// Listen ctx is still live. This must pass on main and on the fix.
func TestSwitchInProgressWhenShutdownStartsKeepsItsBudget877(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	e, _, lazy, builds := newShutdownWindowEngine(t, func() {
		once.Do(func() { close(entered) })
		<-release
	})

	switchDone := make(chan struct{})
	go func() {
		defer close(switchDone)
		e.ForceSwitch()
	}()
	<-entered

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- e.Shutdown(context.Background()) }()
	close(release)

	<-switchDone
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("builds = %d, want 1", got)
	}
	if shut, live := lazy.state(); shut == 0 || !live {
		t.Errorf("standby built before Shutdown: shutdowns=%d firstLive=%v, want shut at least once with its Listen ctx live", shut, live)
	}
}

// TestShutdownHonoursCtxWhileSwitchBuildsStandby886 (celeris#886 item 15):
// Shutdown's first lock of e.mu waits behind a switch that is building the lazy
// standby, and the build holds e.mu for as long as it takes (a bind wait of up
// to 5 s, then the listener wait). A Shutdown(ctx) with a short deadline must
// not outlast ctx because of it: it returns ctx's error at the deadline, and
// whatever it could not do by then is done by the goroutine that was queued
// for the lock, once it has the lock: the sub-engines are shut down and no
// later switch builds a standby.
//
// The blocking hook stands for a standby factory that is slow; it is released
// only after Shutdown has returned (or by the watchdog, so that a Shutdown that
// waits for it fails the test instead of hanging it).
func TestShutdownHonoursCtxWhileSwitchBuildsStandby886(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once, releaseOnce sync.Once
	rel := func() { releaseOnce.Do(func() { close(release) }) }
	e, active, lazy, builds := newShutdownWindowEngine(t, func() {
		once.Do(func() { close(entered) })
		<-release
	})
	t.Cleanup(rel)
	watchdog := time.AfterFunc(5*time.Second, rel)
	defer watchdog.Stop()

	switchDone := make(chan struct{})
	go func() {
		defer close(switchDone)
		e.ForceSwitch()
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the switch never reached the standby build")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := e.Shutdown(ctx)
	took := time.Since(start)
	t.Logf("celeris886 shutdown ctx: deadline=200ms took=%v err=%v", took.Round(time.Millisecond), err)
	if took >= time.Second {
		t.Errorf("Shutdown(200ms) took %v while a switch held e.mu: it waited for the build instead of honouring ctx", took)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown returned %v, want context.DeadlineExceeded", err)
	}
	if e.listenCtx.Err() == nil {
		t.Error("Shutdown gave up on its deadline without cancelling the Listen context: the standby wait would run on")
	}

	// The build finishes. The goroutine queued for the lock now finishes the
	// shutdown Shutdown could not.
	rel()
	select {
	case <-switchDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the switch did not return after the build was released")
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s did not happen within 5s of the build finishing", what)
			}
		}
	}
	waitFor("the active sub-engine's Shutdown", func() bool { n, _ := active.state(); return n > 0 })
	waitFor("the standby's Shutdown", func() bool { n, _ := lazy.state(); return n > 0 })
	waitFor("e.mu to be free", func() bool {
		if e.mu.TryLock() {
			e.mu.Unlock()
			return true
		}
		return false
	})
	e.mu.Lock()
	shutting := e.shuttingDown
	e.mu.Unlock()
	if !shutting {
		t.Error("shuttingDown is not set after the late shutdown ran: a later switch would build a standby nothing shuts down")
	}
	before := builds.Load()
	e.ForceSwitch()
	if got := builds.Load(); got != before {
		t.Errorf("a switch after the late shutdown built a standby (builds %d -> %d)", before, got)
	}
}

// TestSwitchAfterListenCtxCancelledBuildsNothing886: a Shutdown that gave up on
// its ctx has cancelled Listen's context but not set shuttingDown (it never had
// mu). A switch that runs in that gap, as one queued behind the build Shutdown
// gave up on can, must not build a standby: it would be born cancelled and sit
// in a slot until the late shutdown found it.
func TestSwitchAfterListenCtxCancelledBuildsNothing886(t *testing.T) {
	e, _, _, builds := newShutdownWindowEngine(t, nil)
	e.listenCancel()
	e.ForceSwitch()
	if got := builds.Load(); got != 0 {
		t.Errorf("a switch after Listen's context was cancelled built %d standby(s), want 0", got)
	}
}

// TestShutdownCtxVersusSwitchNeverWedges886 is the deadlock check for the ctx-aware
// first lock in Shutdown (RULE 10): Shutdown calls with deadlines either side of
// the time a concurrent switch holds e.mu, repeated, must all return, leave e.mu
// free once the switches are done, and leave the sub-engines shut down by one of
// the two paths (Shutdown itself or the goroutine it left queued for the lock).
func TestShutdownCtxVersusSwitchNeverWedges886(t *testing.T) {
	const rounds = 150
	for i := range rounds {
		hold := time.Duration(i%5) * time.Millisecond
		e, active, lazy, _ := newShutdownWindowEngine(t, func() { time.Sleep(hold) })
		var wg sync.WaitGroup
		for range 2 {
			wg.Go(e.ForceSwitch)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%4)*time.Millisecond)
		errc := make(chan error, 1)
		go func() { errc <- e.Shutdown(ctx) }()
		select {
		case err := <-errc:
			if err != nil && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("round %d: Shutdown returned %v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: Shutdown did not return", i)
		}
		cancel()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: a switch did not return", i)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			a, _ := active.state()
			free := e.mu.TryLock()
			if free {
				e.mu.Unlock()
			}
			if a > 0 && free {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("round %d: active shutdowns=%d mu free=%v after the switches returned", i, a, free)
			}
			time.Sleep(time.Millisecond)
		}
		e.mu.Lock()
		built := e.secondary == lazy
		e.mu.Unlock()
		if n, _ := lazy.state(); n > 0 && !built {
			t.Fatalf("round %d: the standby was shut down but is not in a slot", i)
		}
		if built {
			// The other way round: a standby that was built and stored must be
			// shut down by one of the two paths, the late one possibly a moment
			// after mu is free.
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
				if n, _ := lazy.state(); n > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("round %d: the standby was built and stored but never shut down", i)
				}
			}
		}
	}
}

// goroutinesIn counts the goroutines whose stack mentions sub.
func goroutinesIn(sub string) int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, sub) {
			n++
		}
	}
	return n
}

// TestShutdownRetriesLeaveOneQueuedGoroutine886: a supervisor that retries
// Shutdown(short) against a standby factory that does not return must not add a
// goroutine, parked for e.mu, with every retry. The first Shutdown that gives up
// leaves one, the later ones wait for it within their own ctx and leave none.
// And a retry with a ctx that outlasts the build gets a clean result once the
// build returns, after the late shutdown of the first.
func TestShutdownRetriesLeaveOneQueuedGoroutine886(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once, releaseOnce sync.Once
	rel := func() { releaseOnce.Do(func() { close(release) }) }
	e, active, lazy, _ := newShutdownWindowEngine(t, func() {
		once.Do(func() { close(entered) })
		<-release
	})
	t.Cleanup(rel)
	watchdog := time.AfterFunc(10*time.Second, rel)
	defer watchdog.Stop()

	switchDone := make(chan struct{})
	go func() {
		defer close(switchDone)
		e.ForceSwitch()
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the switch never reached the standby build")
	}

	const retries = 6
	for i := range retries {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		err := e.Shutdown(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("retry %d: Shutdown returned %v, want context.DeadlineExceeded", i, err)
		}
	}
	queued := goroutinesIn("(*Engine).lockForShutdown.func")
	t.Logf("celeris886 retries: shutdowns_that_gave_up=%d goroutines_queued_for_mu=%d", retries, queued)
	if queued != 1 {
		t.Errorf("%d goroutines are queued for e.mu after %d Shutdown(30ms) retries behind one build, want 1", queued, retries)
	}

	// A retry with room: it waits for the build, which is released while it waits.
	retryErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		retryErr <- e.Shutdown(ctx)
	}()
	time.Sleep(50 * time.Millisecond)
	rel()
	select {
	case err := <-retryErr:
		if err != nil {
			t.Errorf("the retry with a 5s ctx returned %v, want nil once the build was released", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("the retry with a 5s ctx did not return after the build was released")
	}
	select {
	case <-switchDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the switch did not return after the build was released")
	}
	// The late shutdown ran before the retry's own (it waited for it), so both
	// reached the active sub-engine, and nothing is left queued.
	if n, _ := active.state(); n < 2 {
		t.Errorf("the active sub-engine saw %d Shutdown calls, want the late one and the retry's", n)
	}
	if n, _ := lazy.state(); n < 1 {
		t.Errorf("the standby saw %d Shutdown calls, want at least 1", n)
	}
	for deadline := time.Now().Add(5 * time.Second); goroutinesIn("(*Engine).lockForShutdown.func") != 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a goroutine is still queued for e.mu after the retry returned")
		}
	}
}

// unboundSub is a standby whose Listen runs until its ctx ends and never
// publishes an address, so the bind wait in buildAndStartStandby (up to 5 s)
// runs to its end unless something cuts it short.
type unboundSub struct {
	*mockEngine
	listened chan struct{}
	returned chan struct{}
}

func (u *unboundSub) Listen(ctx context.Context) error {
	close(u.listened)
	<-ctx.Done()
	close(u.returned)
	return ctx.Err()
}

func (u *unboundSub) Addr() net.Addr { return nil }

// TestShutdownGiveUpCutsStandbyBindWait886 is item 15 in the shape the issue
// names: a standby parked in the 5 s bind wait while it holds e.mu. Shutdown(ctx)
// gives up at ctx and cancels Listen's ctx, and that has to end the bind wait:
// the switch returns, the standby's Listen unwinds and no standby is left in a
// slot, all well before the 5 s the wait would have run.
func TestShutdownGiveUpCutsStandbyBindWait886(t *testing.T) {
	e, _, _, builds := newShutdownWindowEngine(t, nil)
	ub := &unboundSub{mockEngine: newMockEngine(engine.IOUring), listened: make(chan struct{}), returned: make(chan struct{})}
	e.buildStandby = func() (engine.Engine, error) {
		builds.Add(1)
		return ub, nil
	}

	switchDone := make(chan struct{})
	go func() {
		defer close(switchDone)
		e.ForceSwitch()
	}()
	select {
	case <-ub.listened:
	case <-time.After(5 * time.Second):
		t.Fatal("the standby's Listen never started")
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := e.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown returned %v, want context.DeadlineExceeded", err)
	}
	select {
	case <-switchDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the switch is still in the standby's bind wait 3s after Shutdown gave up")
	}
	select {
	case <-ub.returned:
	case <-time.After(3 * time.Second):
		t.Fatal("the standby's Listen did not unwind after Shutdown gave up")
	}
	took := time.Since(start)
	t.Logf("celeris886 bind wait: deadline=200ms shutdown_to_switch_and_listen_unwound=%v (the wait runs 5s)", took.Round(time.Millisecond))
	if took >= 2*time.Second {
		t.Errorf("the bind wait took %v to end after Shutdown gave up", took)
	}
	e.mu.Lock()
	inSlot := e.secondary == engine.Engine(ub)
	e.mu.Unlock()
	if inSlot {
		t.Error("the standby that never bound sits in a slot")
	}
}
