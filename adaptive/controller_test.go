//go:build linux

package adaptive

import (
	"log/slog"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
)

// newCtrlEpollActive returns a controller with epoll active (primary) plus the
// synthetic sampler driving its telemetry, cooldown disabled for unit testing.
func newCtrlEpollActive() (*controller, *syntheticSampler) {
	sampler := newSyntheticSampler()
	c := newController(newMockEngine(engine.Epoll), newMockEngine(engine.IOUring), sampler, testLogger())
	c.cooldown = 0
	return c, sampler
}

// newCtrlIOUringActive returns a controller with io_uring active (primary).
func newCtrlIOUringActive() (*controller, *syntheticSampler) {
	sampler := newSyntheticSampler()
	c := newController(newMockEngine(engine.IOUring), newMockEngine(engine.Epoll), sampler, testLogger())
	c.cooldown = 0
	return c, sampler
}

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// TestConnSwitchDisabledGate verifies the kernel-aware gate: with conns-per-worker
// switching OFF (the production default — the feature-gated start engine is
// authoritative on io_uring-best/epoll-best kernels), neither the epoll up-switch
// nor the io_uring down-revert fires regardless of load, but the io_uring
// error-rate safety revert STILL fires. Guards the warmup-dip revert bug where an
// idle/ramp dip on a 7.0 box reverted io_uring→epoll and stranded the load.
func TestConnSwitchDisabledGate(t *testing.T) {
	now := time.Now()

	c, sampler := newCtrlEpollActive()
	c.connSwitchEnabled = false
	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 64})
	for i := 0; i < 5; i++ {
		if c.evaluate(now.Add(time.Duration(i)*time.Second), false) {
			t.Fatalf("epoll up-switch fired with switching disabled (tick %d)", i)
		}
	}

	c2, s2 := newCtrlIOUringActive()
	c2.connSwitchEnabled = false
	s2.Set(engine.IOUring, TelemetrySnapshot{ConnsPerWorker: 0})
	for i := 0; i < 5; i++ {
		if c2.evaluate(now.Add(time.Duration(i)*time.Second), false) {
			t.Fatalf("io_uring down-revert fired with switching disabled (tick %d)", i)
		}
	}

	c3, s3 := newCtrlIOUringActive()
	c3.connSwitchEnabled = false
	s3.Set(engine.IOUring, TelemetrySnapshot{ConnsPerWorker: 64, ErrorRate: 0.5})
	if !c3.evaluate(now, false) {
		t.Fatal("error-rate safety revert did NOT fire with switching disabled")
	}
}

// TestUpSwitchRequiresSustain verifies the normal epoll→io_uring switch needs
// sustainTicks (2) consecutive ticks at or above the up threshold (20).
func TestUpSwitchRequiresSustain(t *testing.T) {
	c, sampler := newCtrlEpollActive()
	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 20})

	now := time.Now()
	if c.evaluate(now, false) {
		t.Fatal("one tick at the up threshold must not switch (sustainTicks=2)")
	}
	if c.state.upTicks != 1 {
		t.Fatalf("upTicks = %d, want 1 after first qualifying tick", c.state.upTicks)
	}
	if !c.evaluate(now.Add(time.Second), false) {
		t.Fatal("second consecutive tick at the up threshold must switch")
	}
}

// TestUpSwitchSustainResetsOnDip verifies a dip below the up threshold resets
// the sustain counter, so the count must restart.
func TestUpSwitchSustainResetsOnDip(t *testing.T) {
	c, sampler := newCtrlEpollActive()
	now := time.Now()

	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 22})
	if c.evaluate(now, false) {
		t.Fatal("first tick must not switch")
	}
	// Dip into the hysteresis band — resets upTicks.
	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 15})
	if c.evaluate(now.Add(time.Second), false) {
		t.Fatal("dip must not switch")
	}
	if c.state.upTicks != 0 {
		t.Fatalf("upTicks = %d, want 0 after a dip below the up threshold", c.state.upTicks)
	}
	// Back above threshold: needs two more ticks now.
	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 22})
	if c.evaluate(now.Add(2*time.Second), false) {
		t.Fatal("first tick after reset must not switch")
	}
	if !c.evaluate(now.Add(3*time.Second), false) {
		t.Fatal("second tick after reset must switch")
	}
}

// TestFastPathSnapsImmediately verifies conns/worker at/above the high
// watermark (32) switches on a single tick.
func TestFastPathSnapsImmediately(t *testing.T) {
	c, sampler := newCtrlEpollActive()
	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 32})

	if !c.evaluate(time.Now(), false) {
		t.Fatal("conns/worker at the high watermark must snap on one tick")
	}
}

// TestHysteresisBandNoFlap verifies the 12–20 band switches neither way.
func TestHysteresisBandNoFlap(t *testing.T) {
	for _, cpw := range []float64{12, 14, 16, 18, 19.9} {
		c, sampler := newCtrlEpollActive()
		sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: cpw})
		now := time.Now()
		for i := range 4 {
			if c.evaluate(now.Add(time.Duration(i)*time.Second), false) {
				t.Fatalf("cpw=%.1f inside hysteresis band must not switch up", cpw)
			}
		}
	}
}

// TestLargePayloadSuppression verifies a large average payload suppresses the
// switch even well above the high watermark.
func TestLargePayloadSuppression(t *testing.T) {
	c, sampler := newCtrlEpollActive()
	// Exactly at the threshold counts as large (>=).
	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 64, BytesPerReq: 16384})

	now := time.Now()
	for i := range 5 {
		if c.evaluate(now.Add(time.Duration(i)*time.Second), false) {
			t.Fatal("large payload must suppress io_uring switch")
		}
	}

	// Drop below the large-payload threshold → fast path fires.
	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 64, BytesPerReq: 16383})
	if !c.evaluate(now.Add(10*time.Second), false) {
		t.Fatal("small payload above high watermark must switch")
	}
}

// TestRevertRequiresSustain verifies the io_uring→epoll revert needs
// sustainTicks below the down threshold (12).
func TestRevertRequiresSustain(t *testing.T) {
	c, sampler := newCtrlIOUringActive()
	sampler.Set(engine.IOUring, TelemetrySnapshot{ConnsPerWorker: 8})

	now := time.Now()
	if c.evaluate(now, false) {
		t.Fatal("one low tick must not revert (sustainTicks=2)")
	}
	if !c.evaluate(now.Add(time.Second), false) {
		t.Fatal("second consecutive low tick must revert")
	}
}

// TestErrorRevertImmediate verifies an error rate above errorRevertRate (0.05)
// reverts io_uring→epoll immediately regardless of load.
func TestErrorRevertImmediate(t *testing.T) {
	c, sampler := newCtrlIOUringActive()
	sampler.Set(engine.IOUring, TelemetrySnapshot{ConnsPerWorker: 64, ErrorRate: 0.06})

	if !c.evaluate(time.Now(), false) {
		t.Fatal("error rate above errorRevertRate must revert on one tick")
	}
}

// TestNoRevertWhenBusy verifies io_uring stays put above the down threshold
// with a clean error rate.
func TestNoRevertWhenBusy(t *testing.T) {
	c, sampler := newCtrlIOUringActive()
	sampler.Set(engine.IOUring, TelemetrySnapshot{ConnsPerWorker: 16, ErrorRate: 0.0})

	now := time.Now()
	for i := range 5 {
		if c.evaluate(now.Add(time.Duration(i)*time.Second), false) {
			t.Fatal("must not revert while busy with a clean error rate")
		}
	}
}

// TestCooldownGatesRevert verifies the cooldown blocks a revert immediately
// after a switch, but the first switch (no prior switch) is not gated.
func TestCooldownGatesRevert(t *testing.T) {
	c, sampler := newCtrlEpollActive()
	c.cooldown = 1 * time.Hour

	now := time.Now()
	// First switch is not gated (lastSwitch is zero).
	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 40})
	if !c.evaluate(now, false) {
		t.Fatal("first switch must not be cooldown-gated")
	}
	c.recordSwitch(now)

	// io_uring now active and idle, but the revert is cooldown-gated.
	sampler.Set(engine.IOUring, TelemetrySnapshot{ConnsPerWorker: 0})
	if c.evaluate(now.Add(time.Minute), false) {
		t.Fatal("revert within cooldown must be blocked")
	}
}

// TestFrozenBlocksSwitch verifies the frozen flag short-circuits evaluate.
func TestFrozenBlocksSwitch(t *testing.T) {
	c, sampler := newCtrlEpollActive()
	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 100})
	if c.evaluate(time.Now(), true) {
		t.Fatal("frozen must block all switches")
	}
}

// TestOscillationLockExpiry verifies 3 switches in 5 minutes locks the
// controller, and the lock expires after 5 minutes.
func TestOscillationLockExpiry(t *testing.T) {
	sampler := newSyntheticSampler()
	c := newController(newMockEngine(engine.Epoll), newMockEngine(engine.IOUring), sampler, testLogger())
	c.cooldown = 0

	now := time.Now()
	for range 3 {
		c.recordSwitch(now)
		now = now.Add(time.Second)
	}
	if !c.state.locked {
		t.Fatal("expected lock after 3 switches within 5 minutes")
	}

	// Locked: a strong load must not switch.
	sampler.Set(c.activeEngine().Type(), TelemetrySnapshot{ConnsPerWorker: 100})
	if c.evaluate(now, false) {
		t.Fatal("locked controller must not switch")
	}

	// After lock expiry, evaluation resumes (lock cleared on the next eval).
	after := c.state.lockUntil.Add(time.Second)
	_ = c.evaluate(after, false)
	if c.state.locked {
		t.Fatal("lock should clear once lockUntil has passed")
	}
}

// TestRecordSwitchResetsTicks verifies recordSwitch zeroes both tick counters.
func TestRecordSwitchResetsTicks(t *testing.T) {
	c, _ := newCtrlEpollActive()
	c.state.upTicks = 5
	c.state.downTicks = 3
	c.recordSwitch(time.Now())
	if c.state.upTicks != 0 || c.state.downTicks != 0 {
		t.Fatalf("recordSwitch must reset ticks, got up=%d down=%d", c.state.upTicks, c.state.downTicks)
	}
	if c.state.activeIsPrimary {
		t.Fatal("recordSwitch must toggle activeIsPrimary")
	}
}

// guards against a config drift in the documented thresholds.
func TestDefaultThresholds(t *testing.T) {
	c, _ := newCtrlEpollActive()
	if c.upThreshold != 20 || c.downThreshold != 12 || c.highWatermark != 32 {
		t.Fatalf("threshold drift: up=%.0f down=%.0f hwm=%.0f", c.upThreshold, c.downThreshold, c.highWatermark)
	}
	if c.largePayloadBytes != 16384 || c.errorRevertRate != 0.05 || c.sustainTicks != 2 {
		t.Fatalf("policy drift: largePayload=%.0f errRevert=%.3f sustain=%d",
			c.largePayloadBytes, c.errorRevertRate, c.sustainTicks)
	}
	if c.evalInterval != time.Second {
		t.Fatalf("evalInterval = %v, want 1s", c.evalInterval)
	}
	if c.cooldown != 0 { // overridden to 0 by the helper
		t.Fatalf("test helper should zero cooldown, got %v", c.cooldown)
	}
}
