//go:build linux

package adaptive

import (
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
)

// celeris#656, the backoff's boundary. A failed lazy standby build holds off
// the LOAD-driven switches, and nothing else. The io_uring error-rate safety
// revert is the one path this package documents as always active, and on the
// io_uring-start production path -- New() leaves connSwitchEnabled and
// loadDownRevert false there -- it is the ONLY switch evaluate can ever
// recommend. A backoff over it would leave an erroring io_uring serving for as
// long as standbyBuildBackoffMax, and the backoff grows on the very failures
// an error storm provokes. The sampler must also keep being called on every
// tick: liveSampler is delta-based against the previous sample, so a tick that
// skips it widens the next window to the whole skipped span and smears the
// error burst the revert is looking at.

// countingSampler counts Sample calls, returning pre-set snapshots.
type countingSampler struct {
	inner *syntheticSampler
	calls int
}

func newCountingSampler() *countingSampler {
	return &countingSampler{inner: newSyntheticSampler()}
}

func (c *countingSampler) Set(et engine.EngineType, snap TelemetrySnapshot) { c.inner.Set(et, snap) }

func (c *countingSampler) Sample(e engine.Engine) TelemetrySnapshot {
	c.calls++
	return c.inner.Sample(e)
}

// newIOUringActiveController is the io_uring-start production shape: io_uring
// is the active engine, epoll is the lazy standby, and both load-driven
// directions are off, as New() configures that path.
func newIOUringActiveController(t *testing.T, sampler TelemetrySampler) *controller {
	t.Helper()
	ctrl := newController(newMockEngine(engine.Epoll), newMockEngine(engine.IOUring), sampler, testLogger())
	ctrl.state.activeIsPrimary = false
	ctrl.connSwitchEnabled = false
	ctrl.loadDownRevert = false
	return ctrl
}

// TestErrorRateSafetyRevertIsNotGatedByTheStandbyBuildBackoff: an io_uring
// error storm must reach the revert on the next tick however long the epoll
// standby's build has been backing off.
func TestErrorRateSafetyRevertIsNotGatedByTheStandbyBuildBackoff(t *testing.T) {
	sampler := newCountingSampler()
	sampler.Set(engine.IOUring, TelemetrySnapshot{ConnsPerWorker: 30, ActiveConnections: 120, ErrorRate: 0.5})
	ctrl := newIOUringActiveController(t, sampler)

	now := time.Now()
	if !ctrl.evaluate(now, false) {
		t.Fatal("precondition: the controller does not recommend the error-rate safety revert for a 0.5 error rate")
	}

	// Three consecutive failed epoll standby builds: 30s << 2.
	var backoff time.Duration
	for range 3 {
		backoff = ctrl.recordStandbyBuildFailure(now)
	}
	if want := 4 * standbyBuildBackoffBase; backoff != want {
		t.Fatalf("precondition: backoff after 3 failures is %v, want %v", backoff, want)
	}
	if !now.Before(ctrl.state.buildRetryAt) {
		t.Fatalf("precondition: buildRetryAt %v is not in the future", ctrl.state.buildRetryAt)
	}

	for _, at := range []time.Duration{time.Second, 30 * time.Second, backoff - time.Second} {
		if !ctrl.evaluate(now.Add(at), false) {
			t.Errorf("the error-rate safety revert is suppressed %v into a %v standby-build backoff; "+
				"on the io_uring-start path it is the only switch evaluate can recommend", at, backoff)
		}
	}

	// At the cap the suppression would have lasted standbyBuildBackoffMax.
	for range 5 {
		backoff = ctrl.recordStandbyBuildFailure(now)
	}
	if backoff != standbyBuildBackoffMax {
		t.Fatalf("precondition: backoff after 8 failures is %v, want the cap %v", backoff, standbyBuildBackoffMax)
	}
	if !ctrl.evaluate(now.Add(standbyBuildBackoffMax-time.Minute), false) {
		t.Errorf("the error-rate safety revert is still suppressed one minute before a %v backoff expires", standbyBuildBackoffMax)
	}
}

// TestStandbyBuildBackoffStillGatesTheLoadDrivenSwitch: the backoff keeps
// doing its own job — the load that asked for the failed build must not get
// the same build again on the next tick.
func TestStandbyBuildBackoffStillGatesTheLoadDrivenSwitch(t *testing.T) {
	sampler := newSyntheticSampler()
	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 64, ActiveConnections: 256})
	ctrl := newController(newMockEngine(engine.Epoll), newMockEngine(engine.IOUring), sampler, testLogger())

	now := time.Now()
	if !ctrl.evaluate(now, false) {
		t.Fatal("precondition: the controller does not recommend the up-switch for this load")
	}
	if got := ctrl.recordStandbyBuildFailure(now); got != standbyBuildBackoffBase {
		t.Fatalf("first backoff %v, want %v", got, standbyBuildBackoffBase)
	}
	if ctrl.evaluate(now.Add(standbyBuildBackoffBase-time.Second), false) {
		t.Error("the controller recommends the same build again inside the backoff")
	}
	if !ctrl.evaluate(now.Add(standbyBuildBackoffBase+time.Second), false) {
		t.Error("the controller never recommends the switch again after the backoff expires")
	}
}

// TestStandbyBuildBackoffKeepsSamplingEveryTick: a backed-off tick still takes
// a sample, so the delta window stays one tick wide.
func TestStandbyBuildBackoffKeepsSamplingEveryTick(t *testing.T) {
	sampler := newCountingSampler()
	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 64, ActiveConnections: 256})
	ctrl := newController(newMockEngine(engine.Epoll), newMockEngine(engine.IOUring), sampler, testLogger())

	now := time.Now()
	ctrl.recordStandbyBuildFailure(now)
	before := sampler.calls
	const ticks = 5
	for i := 1; i <= ticks; i++ {
		if ctrl.evaluate(now.Add(time.Duration(i)*time.Second), false) {
			t.Fatalf("tick %d: the load-driven switch is not held off by the backoff", i)
		}
	}
	if got := sampler.calls - before; got != ticks {
		t.Errorf("the sampler was called %d times over %d backed-off ticks, want %d: "+
			"a delta-based sampler that skips ticks measures the first tick after the backoff "+
			"over the whole backoff window", got, ticks, ticks)
	}
}

// TestStandbyBuildBackoffDoublesAndCapsAtTheMaximum exercises the doubling and
// the cap branch itself, which no test reached: two failures is 60s, so an
// "11 minutes" probe passed for any backoff under 11 minutes, capped or not.
func TestStandbyBuildBackoffDoublesAndCapsAtTheMaximum(t *testing.T) {
	ctrl := newController(newMockEngine(engine.Epoll), newMockEngine(engine.IOUring), newSyntheticSampler(), testLogger())
	now := time.Now()

	want := []time.Duration{
		30 * time.Second,
		60 * time.Second,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		10 * time.Minute,
		10 * time.Minute,
		10 * time.Minute,
	}
	if want[0] != standbyBuildBackoffBase {
		t.Fatalf("table assumes standbyBuildBackoffBase == %v, it is %v", want[0], standbyBuildBackoffBase)
	}
	if want[len(want)-1] != standbyBuildBackoffMax {
		t.Fatalf("table assumes standbyBuildBackoffMax == %v, it is %v", want[len(want)-1], standbyBuildBackoffMax)
	}
	for i, w := range want {
		got := ctrl.recordStandbyBuildFailure(now)
		if got != w {
			t.Errorf("failure %d: backoff %v, want %v", i+1, got, w)
		}
		if !ctrl.state.buildRetryAt.Equal(now.Add(w)) {
			t.Errorf("failure %d: buildRetryAt %v, want %v", i+1, ctrl.state.buildRetryAt, now.Add(w))
		}
	}

	// A standby that does build clears the whole thing.
	ctrl.recordStandbyBuilt()
	if ctrl.state.buildFailures != 0 || !ctrl.state.buildRetryAt.IsZero() {
		t.Errorf("a successful build left failures=%d retryAt=%v, want 0 and zero",
			ctrl.state.buildFailures, ctrl.state.buildRetryAt)
	}
	if got := ctrl.recordStandbyBuildFailure(now); got != standbyBuildBackoffBase {
		t.Errorf("the failure after a successful build backs off %v, want the base %v", got, standbyBuildBackoffBase)
	}
}
