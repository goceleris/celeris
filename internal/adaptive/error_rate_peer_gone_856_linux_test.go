//go:build linux

package adaptive

import (
	"context"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#856: the io_uring error-rate safety revert fired on SSE and
// WebSocket-hub traffic with every client request answered. Its numerator
// included send_peer_gone, the io_uring sends that complete after the
// subscriber they went to has left; its denominator was the HTTP request
// count, which on a streaming workload is a handful of subscriptions. So a
// hub broadcasting to a thousand subscribers that then dropped read as an
// error rate of 5.8 (355 on v1.5.8) and reverted a serving engine.
//
// These tests drive the real liveSampler and the real controller over mock
// sub-engines whose counters move the way the bench's did.

// lastSnapSampler remembers the snapshot the controller was last given.
type lastSnapSampler struct {
	TelemetrySampler
	last TelemetrySnapshot
}

func (s *lastSnapSampler) Sample(e engine.Engine) TelemetrySnapshot {
	s.last = s.TelemetrySampler.Sample(e)
	return s.last
}

// feed856 gives the active io_uring engine one baseline sample and then one
// interval's counters, and returns whether the controller recommends a revert
// and the snapshot it decided on.
func feed856(t *testing.T, delta func(m *engine.EngineMetrics)) (revert bool, snap TelemetrySnapshot) {
	t.Helper()
	iou := newMockEngine(engine.IOUring)
	sampler := &lastSnapSampler{TelemetrySampler: newLiveSampler(nil)}
	c := newController(iou, newMockEngine(engine.Epoll), sampler, testLogger())
	c.cooldown = 0
	// The production policy: only the error-revert can move an io_uring
	// engine, so a low-load tick cannot recommend anything here.
	c.loadDownRevert = false

	base := engine.EngineMetrics{RequestCount: 1000, ActiveConnections: 80, Workers: 1}
	iou.SetMetrics(base)
	now := time.Now()
	if c.evaluate(now, false) {
		t.Fatal("the baseline tick recommended a switch")
	}
	time.Sleep(5 * time.Millisecond) // the sampler divides by the elapsed time

	m := base
	delta(&m)
	iou.SetMetrics(m)
	revert = c.evaluate(now.Add(time.Second), false)
	return revert, sampler.last
}

// TestHubBroadcastToDroppedSubscribersDoesNotRevert856: 580 sends to
// subscribers that left, against 3 HTTP requests. Before the fix this is an
// error rate of 193 and a revert.
func TestHubBroadcastToDroppedSubscribersDoesNotRevert856(t *testing.T) {
	revert, snap := feed856(t, func(m *engine.EngineMetrics) {
		m.RequestCount += 3
		m.ErrorSendPeerGone = 580
		m.ErrorCount = 580 // ErrorCount is the sum of the buckets
	})
	if revert {
		t.Errorf("the error-rate safety revert fired on client abandonment alone (error_rate=%v)", snap.ErrorRate)
	}
	if snap.ErrorRate != 0 {
		t.Errorf("ErrorRate = %v for 580 sends to departed peers and no engine fault, want 0", snap.ErrorRate)
	}
}

// TestRealSendFaultsStillRevert856 is the second control: the same burst, but
// as transmit faults rather than departed peers. The fix must leave the revert
// exactly as sensitive to a failing engine as it was.
func TestRealSendFaultsStillRevert856(t *testing.T) {
	revert, snap := feed856(t, func(m *engine.EngineMetrics) {
		m.RequestCount += 3
		m.ErrorSend = 580
		m.ErrorCount = 580
	})
	if !revert {
		t.Errorf("580 send faults against 3 requests did not revert (error_rate=%v)", snap.ErrorRate)
	}
	if snap.ErrorRate <= 0.05 {
		t.Errorf("ErrorRate = %v for 580 send faults against 3 requests, want > 0.05", snap.ErrorRate)
	}
}

// TestPeerGoneIsSubtractedNotTheWholeErrorCount856: a mixed interval. Only the
// part that is not abandonment counts, whichever of the two dominates.
func TestPeerGoneIsSubtractedNotTheWholeErrorCount856(t *testing.T) {
	for _, tc := range []struct {
		name        string
		gone, fault uint64
		requests    uint64
		wantRevert  bool
		wantRate    float64
	}{
		{"mostly abandonment", 500, 1, 100, false, 0.01},
		{"abandonment hides a real fault", 500, 10, 100, true, 0.10},
		{"faults only", 0, 4, 100, false, 0.04},
	} {
		t.Run(tc.name, func(t *testing.T) {
			revert, snap := feed856(t, func(m *engine.EngineMetrics) {
				m.RequestCount += tc.requests
				m.ErrorSendPeerGone = tc.gone
				m.ErrorSend = tc.fault
				m.ErrorCount = tc.gone + tc.fault
			})
			if revert != tc.wantRevert {
				t.Errorf("revert = %v, want %v (error_rate=%v)", revert, tc.wantRevert, snap.ErrorRate)
			}
			if got := snap.ErrorRate; got < tc.wantRate-1e-9 || got > tc.wantRate+1e-9 {
				t.Errorf("ErrorRate = %v, want %v", got, tc.wantRate)
			}
		})
	}
}

// TestPeerGoneBucketAheadOfTotalCannotUnderflow856: a bucket read ahead of the
// total it is part of (the engines fill them from one snapshot, so this should
// not happen) must read as no errors, not as 2^64 of them.
func TestPeerGoneBucketAheadOfTotalCannotUnderflow856(t *testing.T) {
	revert, snap := feed856(t, func(m *engine.EngineMetrics) {
		m.RequestCount += 100
		m.ErrorSendPeerGone = 7
		m.ErrorCount = 3
	})
	if revert || snap.ErrorRate != 0 {
		t.Errorf("revert=%v error_rate=%v for a peer-gone bucket larger than ErrorCount, want no revert and 0",
			revert, snap.ErrorRate)
	}
}

// bigRespHandler answers every request with a body far larger than a socket
// buffer, so a client that leaves after asking leaves the engine with sends in
// flight to a peer that is gone: the shape of a streaming subscriber that
// drops, with no streaming support needed in the handler.
type bigRespHandler struct{}

const bigBody856 = 4 << 20

func (bigRespHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "application/octet-stream"}, {"content-length", strconv.Itoa(bigBody856)}},
		make([]byte, bigBody856))
}

// TestLiveAbandonedDownloadsDoNotRevertIOUring856 is the mechanism, not the
// arithmetic: a REAL io_uring engine, REAL clients that ask for a large body
// and reset the connection before reading it, and the real controller reading
// the engine's own counters. io_uring counts each such send as send_peer_gone
// (celeris#645); epoll counts nothing, so the engines disagree on a fault
// that is not one.
//
// The premise is measured, not assumed: the engine must report a
// send_peer_gone delta, and the pre-fix ratio (ErrorCount over requests) must
// exceed the revert threshold on this very interval, else the test would pass
// on a tree that still counts abandonment.
func TestLiveAbandonedDownloadsDoNotRevertIOUring856(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	e, addr, stop := s0Bind(t, resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1}, bigRespHandler{})
	defer stop()
	forceSwitchTo(t, e, engine.IOUring)
	time.Sleep(300 * time.Millisecond)

	// The eval loop evaluates under switchMu (the engine is frozen, so it
	// returns at once); take it too for every touch of the controller.
	eval := func(at time.Time) bool {
		e.switchMu.Lock()
		defer e.switchMu.Unlock()
		return e.ctrl.evaluate(at, false)
	}
	e.switchMu.Lock()
	rec := &lastSnapSampler{TelemetrySampler: e.ctrl.sampler}
	e.ctrl.sampler = rec
	e.ctrl.loadDownRevert = false // production policy: only the error-revert can move io_uring
	e.switchMu.Unlock()
	now := time.Now()
	if eval(now) {
		t.Fatal("the baseline tick recommended a switch")
	}
	iou := func() engine.EngineMetrics {
		e.mu.Lock()
		s := e.secondary
		e.mu.Unlock()
		return s.Metrics()
	}
	base := iou()

	const clients = 48
	var wg sync.WaitGroup
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			var one [1]byte
			_, _ = c.Read(one[:]) // the response has started: sends are in flight
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetLinger(0) // close sends RST, not FIN
			}
			_ = c.Close()
		}()
	}
	wg.Wait()

	var after engine.EngineMetrics
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); time.Sleep(20 * time.Millisecond) {
		after = iou()
		if after.ErrorSendPeerGone-base.ErrorSendPeerGone >= clients/2 {
			break
		}
	}
	gone := after.ErrorSendPeerGone - base.ErrorSendPeerGone
	reqs := after.RequestCount - base.RequestCount
	errs := after.ErrorCount - base.ErrorCount
	t.Logf("celeris856 LIVE clients=%d requests=%d error_count=+%d send_peer_gone=+%d send=+%d",
		clients, reqs, errs, gone, after.ErrorSend-base.ErrorSend)

	// PREMISE (RULE 27): the interval must hold abandonment, and the pre-fix
	// ratio of it must be over the threshold.
	if gone == 0 || reqs == 0 {
		t.Fatalf("celeris856 PREMISE: no send_peer_gone was counted (gone=%d requests=%d): the cell does not exercise the signal", gone, reqs)
	}
	if old := float64(errs) / float64(reqs); old <= e.ctrl.errorRevertRate {
		t.Fatalf("celeris856 PREMISE: the pre-fix ratio %.3f (%d errors over %d requests) is not above the revert threshold %.2f, so this cell cannot tell a fixed tree from a broken one",
			old, errs, reqs, e.ctrl.errorRevertRate)
	}

	revert := eval(now.Add(time.Second))
	t.Logf("celeris856 LIVE error_rate=%.4f revert=%v", rec.last.ErrorRate, revert)
	if revert {
		t.Errorf("celeris856: the error-rate safety revert fired on %d abandoned downloads and no engine fault (error_rate=%.3f)",
			gone, rec.last.ErrorRate)
	}
	if rec.last.ErrorRate > e.ctrl.errorRevertRate {
		t.Errorf("celeris856: ErrorRate = %.3f over abandoned downloads, want <= %.2f", rec.last.ErrorRate, e.ctrl.errorRevertRate)
	}
}
