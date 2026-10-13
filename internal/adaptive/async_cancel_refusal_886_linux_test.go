//go:build linux

package adaptive

import (
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/iouring"
	"github.com/goceleris/celeris/internal/probe"
)

// The two tests below make the io_uring engine refuse to be built the way a
// kernel before Linux 5.19 (or one that rejects the IORING_ASYNC_CANCEL flags)
// makes it, on the kernel that runs them (iouring.ForceAsyncCancelProbe).
// CI runs on a kernel that accepts the flags, where nothing else reaches the
// two behaviours that depend on the refusal (celeris#886 item 3, celeris#682):
// the start engine falling back to epoll, and the standby build backing off.

// refuseIOUring886 forces the refusal, or skips (fails under
// CELERIS_REQUIRE_UPSWITCH=1) where the probe is never asked: with the probed
// tier none, New refuses io_uring before it probes, and the test would pass
// for that reason instead.
func refuseIOUring886(t *testing.T) {
	t.Helper()
	if !probe.Probe().IOUringTier.Available() {
		skipOrFailUpswitch662(t, "io_uring unavailable here (tier none): the async-cancel refusal is never reached")
	}
	t.Cleanup(iouring.ForceAsyncCancelProbe(iouring.AsyncCancelRejected,
		"IORING_ASYNC_CANCEL flags rejected: cqe.res=-22 (EINVAL); forced by the test"))
}

// TestAdaptiveStartFallsBackToEpollWhenIOUringRefused886: the io_uring start
// engine is refused (CELERIS_ADAPTIVE_START=iouring, a probe that rejects the
// flags), and the adaptive engine starts on epoll instead of failing New. It
// warns with the refusal, keeps the io_uring standby to build on a later
// switch, and serves.
func TestAdaptiveStartFallsBackToEpollWhenIOUringRefused886(t *testing.T) {
	refuseIOUring886(t)
	e, addr, logs := newFloorAdaptive682(t, "iouring")
	stop := listenFloor682(t, e)
	defer stop()

	if e.startType != engine.Epoll {
		t.Errorf("startType = %v, want epoll: the io_uring start engine was refused", e.startType)
	}
	if got := e.ActiveEngine().Type(); got != engine.Epoll {
		t.Errorf("active engine = %v, want epoll", got)
	}
	e.slotsMu.Lock()
	primary, secondary := e.primary, e.secondary
	e.slotsMu.Unlock()
	if primary == nil || secondary != nil {
		t.Errorf("slots after the fallback: primary=%v secondary=%v, want epoll in primary and no io_uring engine", primary, secondary)
	}
	if e.buildStandby == nil {
		t.Error("no standby constructor after the fallback: a later switch could never build io_uring")
	}
	var warned bool
	for _, r := range logs.records() {
		if msg, _ := r["msg"].(string); strings.HasPrefix(msg, "io_uring start engine unavailable") {
			warned = r["level"] == "WARN"
			if err, _ := r["error"].(string); !strings.Contains(err, "io_uring not available on this system") || !strings.Contains(err, "5.19") {
				t.Errorf("fallback record error = %q, want the io_uring engine's refusal naming Linux 5.19", err)
			}
		}
	}
	if !warned {
		t.Error("no WARN \"io_uring start engine unavailable\" record: the fallback was silent")
	}
	if code := getOnce(t, addr); code != 200 {
		t.Errorf("GET = %d, want 200", code)
	}
}

// TestAdaptiveStandbyBuildAbortsWhenIOUringRefused886: the lazy io_uring
// standby is refused when the first switch asks for it. The switch aborts with
// a WARN naming the refusal, the epoll start engine stays active and serving,
// no io_uring engine lands in a slot, and the controller records the failure
// (its backoff, celeris#656) instead of asking for the same build on the next
// tick.
func TestAdaptiveStandbyBuildAbortsWhenIOUringRefused886(t *testing.T) {
	refuseIOUring886(t)
	e, addr, logs := newFloorAdaptive682(t, "epoll")
	stop := listenFloor682(t, e)
	defer stop()
	if e.startType != engine.Epoll {
		t.Fatalf("startType = %v, want epoll (CELERIS_ADAPTIVE_START=epoll)", e.startType)
	}

	e.ForceSwitch()

	if got := e.ActiveEngine().Type(); got != engine.Epoll {
		t.Errorf("active engine after the aborted switch = %v, want epoll", got)
	}
	e.slotsMu.Lock()
	secondary := e.secondary
	e.slotsMu.Unlock()
	if secondary != nil {
		t.Errorf("an io_uring engine %v sits in the standby slot after a refused build", secondary)
	}
	if n := e.Metrics().AdaptiveSwitches; n != 0 {
		t.Errorf("AdaptiveSwitches = %d, want 0: the switch did not happen", n)
	}
	e.switchMu.Lock()
	failures := e.ctrl.state.buildFailures
	e.switchMu.Unlock()
	if failures != 1 {
		t.Errorf("recorded standby build failures = %d, want 1", failures)
	}
	var aborted bool
	for _, r := range logs.records() {
		if msg, _ := r["msg"].(string); strings.HasPrefix(msg, "aborting switch: lazy standby build failed") {
			aborted = r["level"] == "WARN"
			if err, _ := r["error"].(string); !strings.Contains(err, "io_uring not available on this system") || !strings.Contains(err, "5.19") {
				t.Errorf("abort record error = %q, want the io_uring engine's refusal naming Linux 5.19", err)
			}
		}
	}
	if !aborted {
		t.Error("no WARN \"aborting switch: lazy standby build failed\" record")
	}
	if code := getOnce(t, addr); code != 200 {
		t.Errorf("GET after the aborted switch = %d, want 200", code)
	}
}
