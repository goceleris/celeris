//go:build linux

package adaptive

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
)

// errBuckets is the celeris#645 breakdown as a map, so a test can assert on
// the whole partition instead of on the one bucket it happens to expect.
func errBuckets(m engine.EngineMetrics) map[string]uint64 {
	return map[string]uint64{
		"AcceptFDLimit":    m.ErrorAcceptFDLimit,
		"AcceptCancelled":  m.ErrorAcceptCancelled,
		"AcceptOther":      m.ErrorAcceptOther,
		"ConnTableCap":     m.ErrorConnTableCap,
		"ConnRegister":     m.ErrorConnRegister,
		"ListenerRecreate": m.ErrorListenerRecreate,
		"TransplantAdopt":  m.ErrorTransplantAdopt,
		"SendPeerGone":     m.ErrorSendPeerGone,
		"Send":             m.ErrorSend,
		"RequestBody":      m.ErrorRequestBody,
		"Handler":          m.ErrorHandler,
	}
}

func bucketDeltas(before, after engine.EngineMetrics) map[string]uint64 {
	b, a := errBuckets(before), errBuckets(after)
	d := make(map[string]uint64, len(a))
	for k := range a {
		d[k] = a[k] - b[k]
	}
	return d
}

func sumBuckets(m engine.EngineMetrics) uint64 {
	var total uint64
	for _, v := range errBuckets(m) {
		total += v
	}
	return total
}

// lossBuckets are the causes that mean a connection was DROPPED — the ones
// celeris#645 needed to rule in or out and could not, because a bare
// ErrorCount cannot tell a descriptor shortage from an accept the engine
// cancelled on purpose. Each of them closes or abandons a real connection,
// which is what the run's single ws_handshake_fail would look like from the
// inside.
var lossBuckets = []string{
	"AcceptFDLimit", "ConnTableCap", "ConnRegister", "ListenerRecreate", "TransplantAdopt",
}

// TestPromotionUnderLoadAttributesEveryError runs the shape of celeris#645's
// cell — keep-alive load plus connection churn across a real epoll→io_uring
// promotion on a real adaptive engine — and checks that whatever errors the
// switch costs, the metrics NAME them.
//
// The value here is not the number; it is that the number now has a cause and
// a side. The issue's table could only say "421, and the two sub-engines on
// their own do not account for it". A run of this shape now reports which
// bucket moved and whether it moved on the standby or the promoted engine.
//
// Two assertions, both about shape rather than magnitude, so the test is not
// a thermometer for the host it runs on:
//
//   - the published total equals the published parts, on a live engine with
//     both sub-engines running;
//   - no LOSS bucket moves across a clean promotion. A switch that starts
//     dropping connections at its conn-table cap, exhausting descriptors, or
//     refusing adoptions is a regression, and before the split it was
//     indistinguishable from the accept teardown the pause pays on purpose.
func TestPromotionUnderLoadAttributesEveryError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in -short mode")
	}
	e, addr, stop := newBoundAdaptiveH(t, respHandler{}, false)
	defer stop()
	// The controller must not switch on its own mid-measurement: this test
	// is about ONE promotion, at a moment it chooses.
	e.FreezeSwitching()

	pause, stopLoad := make(chan struct{}), make(chan struct{})
	var ok, errc atomic.Int64
	wg := driveKeepAlive(addr, 32, pause, stopLoad, &ok, &errc)
	defer func() {
		close(pause)
		close(stopLoad)
		wg.Wait()
	}()
	time.Sleep(500 * time.Millisecond)

	before := e.Metrics()
	if got := sumBuckets(before); got != before.ErrorCount {
		t.Fatalf("before the switch: buckets sum to %d but ErrorCount is %d", got, before.ErrorCount)
	}

	e.UnfreezeSwitching()
	e.ForceSwitch() // epoll -> io_uring, the promotion #645 measured
	time.Sleep(1500 * time.Millisecond)

	after := e.Metrics()
	d := bucketDeltas(before, after)
	t.Logf("promotion: ErrorCount %d→%d (+%d), standby share %d, buckets moved %v",
		before.ErrorCount, after.ErrorCount, after.ErrorCount-before.ErrorCount,
		after.StandbyErrorCount, d)
	t.Logf("requests ok=%d client-errors=%d transplanted=%d/%d",
		ok.Load(), errc.Load(), after.TransplantAdopted, after.TransplantDetached)

	if got := sumBuckets(after); got != after.ErrorCount {
		t.Errorf("after the switch: buckets sum to %d but ErrorCount is %d — on a "+
			"live adaptive engine the breakdown must still account for the whole "+
			"total", got, after.ErrorCount)
	}
	if after.StandbyErrorCount > after.ErrorCount {
		t.Errorf("StandbyErrorCount %d exceeds ErrorCount %d",
			after.StandbyErrorCount, after.ErrorCount)
	}
	for _, name := range lossBuckets {
		if d[name] != 0 {
			t.Errorf("a clean promotion moved the %s bucket by %d — that bucket "+
				"means connections were dropped, not that an accept was torn "+
				"down on purpose (celeris#645)", name, d[name])
		}
	}
}

// TestRevertChargesItsAcceptTeardownToTheStandby is the one error source a
// switch produces that neither sub-engine produces while running alone, so it
// is the first thing celeris#645's arithmetic has to account for: pausing the
// io_uring sub-engine cancels each worker's in-flight multishot accept
// (-ECANCELED), one counted failure per worker per pause. It used to be two:
// the pause also re-armed accept on the descriptor it was closing, and that
// accept failed with -EBADF after the close (celeris#662).
//
// On the epoll→io_uring direction it is zero, because epoll's pause path
// counts no accept failure: it accepts the connections still queued and
// serves them. So this is a floor under a REVERT, not under the promotion in
// the issue's table, and it is far too small to be #645's ~5/s. Pinning it is
// what lets the next nightly subtract it instead of arguing about it.
func TestRevertChargesItsAcceptTeardownToTheStandby(t *testing.T) {
	e, _, stop := newBoundAdaptiveH(t, respHandler{}, false)
	defer stop()

	e.ForceSwitch() // epoll -> io_uring: builds and starts the standby
	time.Sleep(300 * time.Millisecond)

	before := e.Metrics()
	e.ForceSwitch() // io_uring -> epoll: PAUSES io_uring, which is the cost
	time.Sleep(500 * time.Millisecond)
	after := e.Metrics()

	d := bucketDeltas(before, after)
	t.Logf("revert: ErrorCount +%d, standby share %d, buckets moved %v",
		after.ErrorCount-before.ErrorCount, after.StandbyErrorCount, d)

	for name, got := range d {
		if name == "AcceptCancelled" {
			continue
		}
		if got != 0 {
			t.Errorf("the revert moved bucket %s by %d, want 0 — a deliberate "+
				"accept teardown must not read as %s", name, got, name)
		}
	}
	if got := sumBuckets(after); got != after.ErrorCount {
		t.Errorf("buckets sum to %d but ErrorCount is %d", got, after.ErrorCount)
	}
	// The paused sub-engine is the standby by definition, so its teardown
	// cost must be reported on the standby side of the split.
	if d["AcceptCancelled"] > 0 && after.StandbyErrorCount < d["AcceptCancelled"] {
		t.Errorf("StandbyErrorCount = %d but the pause charged %d cancelled accepts "+
			"to the sub-engine that was just paused — that sub-engine IS the "+
			"standby, so the split is naming the wrong side",
			after.StandbyErrorCount, d["AcceptCancelled"])
	}
}
