package adaptive

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

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
// before Listen unwinds. The switch must not build a standby, and a standby
// that a switch did build must not be left unshut.
func TestSwitchAfterShutdownReturnsBuildsNothing877(t *testing.T) {
	e, _, lazy, builds := newShutdownWindowEngine(t, nil)

	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	e.ForceSwitch()

	if got := builds.Load(); got != 0 {
		t.Errorf("a switch after Shutdown returned built %d standby(s), want 0 (#877)", got)
	}
	if shut, _ := lazy.state(); builds.Load() > 0 && shut == 0 {
		t.Errorf("a standby built after Shutdown returned was never shut down (#877)")
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
