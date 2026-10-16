//go:build linux

package adaptive

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"reflect"
	"strconv"
	"strings"
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

// Rebase forwards to the wrapped sampler, so the controller still finds the
// optional interface through the wrapper.
func (s *lastSnapSampler) Rebase(e engine.Engine) {
	if r, ok := s.TelemetrySampler.(interface{ Rebase(engine.Engine) }); ok {
		r.Rebase(e)
	}
}

// stepClock is a manual clock for a liveSampler.
type stepClock struct{ t time.Time }

func (c *stepClock) now() time.Time          { return c.t }
func (c *stepClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// feed856 gives the active io_uring engine one baseline sample and then one
// interval's counters, and returns whether the controller recommends a revert
// and the snapshot it decided on.
func feed856(t *testing.T, delta func(m *engine.EngineMetrics)) (revert bool, snap TelemetrySnapshot) {
	t.Helper()
	return feed856Log(t, testLogger(), delta)
}

// feed856Log is feed856 with the controller's logger supplied, for a test that
// reads what the controller logged.
func feed856Log(t *testing.T, logger *slog.Logger, delta func(m *engine.EngineMetrics)) (revert bool, snap TelemetrySnapshot) {
	t.Helper()
	iou := newMockEngine(engine.IOUring)
	// The sampler's clock is a manual one: the rate is divided by the larger of
	// the requests and the connection-seconds (celeris#886 item 9), and a real
	// clock that stretched the 5 ms step would change which one it is.
	clk := &stepClock{t: time.Now()}
	live := newLiveSampler(nil)
	live.clock = clk.now
	sampler := &lastSnapSampler{TelemetrySampler: live}
	c := newController(iou, newMockEngine(engine.Epoll), sampler, logger)
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
	clk.advance(5 * time.Millisecond) // the sampler divides by the elapsed time

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

// The bench has no per-bucket error series, so a revert could not be
// attributed to a bucket after the fact: the "engine switch recommended" line
// carried only error_rate (celeris#886 item 10). The line now carries the
// interval's delta of every non-zero Error* bucket, send_peer_gone included
// although the rate leaves it out.

// switchLine856 returns the "engine switch recommended" record out of a JSON
// slog stream, as a map of its attributes.
func switchLine856(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var found map[string]any
	dec := json.NewDecoder(buf)
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			t.Fatalf("log output is not JSON: %v", err)
		}
		if rec["msg"] == "engine switch recommended" {
			if found != nil {
				t.Fatalf("two switch lines: %v and %v", found, rec)
			}
			found = rec
		}
	}
	if found == nil {
		t.Fatalf("no \"engine switch recommended\" line was logged: %s", buf.String())
	}
	return found
}

// snake856 turns a field name into its attribute name: ErrorSendPeerGone to
// error_send_peer_gone, ErrorAcceptFDLimit to error_accept_fd_limit.
func snake856(s string) string {
	isUpper := func(b byte) bool { return b >= 'A' && b <= 'Z' }
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		// A new word starts at an upper-case letter that follows a lower-case
		// one, or that ends an acronym (the L of FDLimit).
		if i > 0 && isUpper(s[i]) && (!isUpper(s[i-1]) || (i+1 < len(s) && !isUpper(s[i+1]))) {
			b.WriteByte('_')
		}
		b.WriteByte(s[i])
	}
	return strings.ToLower(b.String())
}

// TestSwitchLineCarriesTheErrorBuckets886: 7 transmit faults and 3 sends to
// departed peers in one interval, against 10 requests. The revert fires on the
// 7 (error_rate 0.7); the line names both buckets, the peer-gone one although
// the rate leaves it out, and names no bucket that did not move.
func TestSwitchLineCarriesTheErrorBuckets886(t *testing.T) {
	var buf bytes.Buffer
	revert, snap := feed856Log(t, slog.New(slog.NewJSONHandler(&buf, nil)), func(m *engine.EngineMetrics) {
		m.RequestCount += 10
		m.ErrorSend = 7
		m.ErrorSendPeerGone = 3
		m.ErrorCount = 10
	})
	if !revert {
		t.Fatalf("7 send faults against 10 requests did not revert (error_rate=%v)", snap.ErrorRate)
	}
	line := switchLine856(t, &buf)
	if got, ok := line["error_send"].(float64); !ok || got != 7 {
		t.Errorf("error_send = %v, want 7 (line %v)", line["error_send"], line)
	}
	if got, ok := line["error_send_peer_gone"].(float64); !ok || got != 3 {
		t.Errorf("error_send_peer_gone = %v, want 3 (line %v)", line["error_send_peer_gone"], line)
	}
	if got, ok := line["error_rate"].(float64); !ok || got < 0.7-1e-9 || got > 0.7+1e-9 {
		t.Errorf("error_rate = %v, want 0.7: the existing attrs must stay (line %v)", line["error_rate"], line)
	}
	for k := range line {
		if strings.HasPrefix(k, "error_") && k != "error_rate" && k != "error_send" && k != "error_send_peer_gone" {
			t.Errorf("the line names %s although that bucket did not move (line %v)", k, line)
		}
	}
}

// TestSwitchLineNamesEveryErrorField886 is the guard against a bucket added to
// EngineMetrics and left out of the log: every Error* field but the ErrorCount
// sum is given its own value in the interval, and each must come back under
// its snake-case name with that value. A field the controller does not
// carry fails here, whatever the table inside it looks like.
func TestSwitchLineNamesEveryErrorField886(t *testing.T) {
	typ := reflect.TypeOf(engine.EngineMetrics{})
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		if n := typ.Field(i).Name; strings.HasPrefix(n, "Error") && n != "ErrorCount" {
			names = append(names, n)
		}
	}
	if len(names) != 11 {
		t.Fatalf("EngineMetrics has %d Error* buckets besides ErrorCount (%v), this test and the log were written for 11: extend both", len(names), names)
	}
	var buf bytes.Buffer
	revert, snap := feed856Log(t, slog.New(slog.NewJSONHandler(&buf, nil)), func(m *engine.EngineMetrics) {
		m.RequestCount += 10
		v := reflect.ValueOf(m).Elem()
		var sum uint64
		for i, n := range names {
			v.FieldByName(n).SetUint(uint64(i + 1))
			sum += uint64(i + 1)
		}
		m.ErrorCount = sum
	})
	if !revert {
		t.Fatalf("no revert (error_rate=%v)", snap.ErrorRate)
	}
	line := switchLine856(t, &buf)
	for i, n := range names {
		key := snake856(n)
		if got, ok := line[key].(float64); !ok || got != float64(i+1) {
			t.Errorf("%s = %v, want %d (line %v)", key, line[key], i+1, line)
		}
	}
}

// TestBucketDeltaIsPerIntervalAndSaturates886: the logged number is the delta
// of the interval, not the cumulative counter, and a bucket that reads lower
// than at the last sample reads 0, not 2^64.
func TestBucketDeltaIsPerIntervalAndSaturates886(t *testing.T) {
	iou := newMockEngine(engine.IOUring)
	s := newLiveSampler(nil)
	iou.SetMetrics(engine.EngineMetrics{RequestCount: 100, ErrorSend: 40, ErrorHandler: 9, ErrorCount: 49})
	s.Sample(iou)
	time.Sleep(2 * time.Millisecond)
	iou.SetMetrics(engine.EngineMetrics{RequestCount: 200, ErrorSend: 45, ErrorHandler: 2, ErrorCount: 47})
	snap := s.Sample(iou)
	var buf bytes.Buffer
	c := newController(iou, newMockEngine(engine.Epoll), newSyntheticSampler(), slog.New(slog.NewJSONHandler(&buf, nil)))
	c.logSwitch("io_uring", "epoll", "error-rate safety revert", 0, snap)
	line := switchLine856(t, &buf)
	if got := line["error_send"]; got != float64(5) {
		t.Errorf("error_send = %v, want 5 (45 now, 40 at the last sample)", got)
	}
	if _, ok := line["error_handler"]; ok {
		t.Errorf("error_handler = %v: a bucket that fell must read 0 and be left out", line["error_handler"])
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

// The second route to the same spurious revert (celeris#856). The controller
// samples only the active engine, so the sampler's baseline for io_uring is
// the last sample taken BEFORE the previous revert. The first sample after a
// re-promotion therefore reaches back over the whole time io_uring was the
// standby: its own teardown at the pause (a cancelled accept per worker,
// celeris#645) and the few requests it served while draining, a window in
// which "errors over requests" says nothing about the engine now. The bench's
// reverts come in pairs 62 s apart on every adaptive column that has them: a
// revert, the 30 s cooldown, the re-promotion, the 30 s cooldown, and the
// first post-cooldown sample of the stale window.

// restage856 walks a controller with io_uring active through a revert and a
// re-promotion. teardown runs while io_uring is the standby and gives its
// counters whatever a pause leaves behind; after runs once io_uring is active
// again. It returns whether the first evaluation after the re-promotion
// recommends a revert, and the snapshot it decided on.
func restage856(t *testing.T, teardown, after func(m *engine.EngineMetrics)) (bool, TelemetrySnapshot) {
	t.Helper()
	iou := newMockEngine(engine.IOUring)
	clk := &stepClock{t: time.Now()} // manual, see feed856Log
	live := newLiveSampler(nil)
	live.clock = clk.now
	sampler := &lastSnapSampler{TelemetrySampler: live}
	c := newController(iou, newMockEngine(engine.Epoll), sampler, testLogger())
	c.cooldown = 0
	c.loadDownRevert = false

	m := engine.EngineMetrics{RequestCount: 5000, ActiveConnections: 100, Workers: 4}
	iou.SetMetrics(m)
	now := time.Now()
	if c.evaluate(now, false) { // the baseline sample, taken while io_uring is active
		t.Fatal("the baseline tick recommended a switch")
	}
	c.recordSwitch(now) // revert: epoll is active, io_uring the standby
	if c.activeEngine().Type() != engine.Epoll {
		t.Fatalf("the revert left %v active", c.activeEngine().Type())
	}
	teardown(&m) // the pause, and the stragglers the standby serves
	iou.SetMetrics(m)
	clk.advance(5 * time.Millisecond)
	c.recordSwitch(now.Add(61 * time.Second)) // re-promotion
	if c.activeEngine().Type() != engine.IOUring {
		t.Fatalf("the re-promotion left %v active", c.activeEngine().Type())
	}
	after(&m)
	iou.SetMetrics(m)
	clk.advance(5 * time.Millisecond)
	revert := c.evaluate(now.Add(92*time.Second), false) // the first post-cooldown tick
	return revert, sampler.last
}

// TestReactivatedEngineIsNotJudgedOnItsStandbyWindow856: four workers' accept
// teardown and six stragglers while io_uring was the standby, nothing wrong
// since it became active again. Before the fix that is 4 errors over 16
// requests and a revert.
func TestReactivatedEngineIsNotJudgedOnItsStandbyWindow856(t *testing.T) {
	revert, snap := restage856(t,
		func(m *engine.EngineMetrics) {
			m.RequestCount += 6
			m.ErrorAcceptCancelled += 4
			m.ErrorCount += 4
		},
		func(m *engine.EngineMetrics) { m.RequestCount += 10 })
	if revert || snap.ErrorRate != 0 {
		t.Errorf("revert=%v error_rate=%v for an engine with no fault since it became active again, want no revert and 0",
			revert, snap.ErrorRate)
	}
}

// TestFaultsAfterReactivationStillRevert856 is the second control: the same
// walk, with the faults happening after the re-promotion.
func TestFaultsAfterReactivationStillRevert856(t *testing.T) {
	revert, snap := restage856(t,
		func(m *engine.EngineMetrics) {
			m.RequestCount += 6
			m.ErrorAcceptCancelled += 4
			m.ErrorCount += 4
		},
		func(m *engine.EngineMetrics) {
			m.RequestCount += 100
			m.ErrorSend += 30
			m.ErrorCount += 30
		})
	if !revert || snap.ErrorRate < 0.29 || snap.ErrorRate > 0.31 {
		t.Errorf("revert=%v error_rate=%v for 30 faults in 100 requests since the re-promotion, want a revert at 0.30",
			revert, snap.ErrorRate)
	}
}

// TestLiveRepromotionIsNotJudgedOnTheStandbyWindow856 is the mechanism on a
// real engine: promote, serve a few requests, revert (the io_uring pause
// cancels a multishot accept per worker, which the engine counts), promote
// again, and let the real controller take its first sample of io_uring. The
// premise is measured: the standby window must hold errors and the pre-fix
// ratio of them must be over the threshold.
func TestLiveRepromotionIsNotJudgedOnTheStandbyWindow856(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	e, addr, stop := s0Bind(t, resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1}, respHandler{})
	defer stop()
	forceSwitchTo(t, e, engine.IOUring)
	time.Sleep(300 * time.Millisecond)

	e.switchMu.Lock()
	rec := &lastSnapSampler{TelemetrySampler: e.ctrl.sampler}
	e.ctrl.sampler = rec
	e.ctrl.loadDownRevert = false
	e.switchMu.Unlock()
	eval := func(at time.Time) bool {
		e.switchMu.Lock()
		defer e.switchMu.Unlock()
		return e.ctrl.evaluate(at, false)
	}
	iou := func() engine.EngineMetrics {
		e.mu.Lock()
		s := e.secondary
		e.mu.Unlock()
		return s.Metrics()
	}
	now := time.Now()
	if eval(now) { // the baseline sample of io_uring, while it is active
		t.Fatal("the baseline tick recommended a switch")
	}
	base := iou()

	get := func() {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		_, _ = c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"))
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = io.Copy(io.Discard, c)
	}
	for range 4 {
		get() // a handful of requests: what a streaming workload has
	}
	forceSwitchTo(t, e, engine.Epoll)
	time.Sleep(2 * time.Second) // past io_uring's linger: its pause has torn its accepts down
	forceSwitchTo(t, e, engine.IOUring)

	after := iou()
	errs := after.ErrorCount - base.ErrorCount
	reqs := after.RequestCount - base.RequestCount
	t.Logf("celeris856 REPROMOTE standby window: requests=+%d error_count=+%d accept_cancelled=+%d peer_gone=+%d send=+%d",
		reqs, errs, after.ErrorAcceptCancelled-base.ErrorAcceptCancelled,
		after.ErrorSendPeerGone-base.ErrorSendPeerGone, after.ErrorSend-base.ErrorSend)
	if reqs == 0 || errs == 0 {
		t.Fatalf("celeris856 PREMISE: the standby window holds %d requests and %d errors", reqs, errs)
	}
	if old := float64(errs) / float64(reqs); old <= e.ctrl.errorRevertRate {
		t.Fatalf("celeris856 PREMISE: the pre-fix ratio %.3f is not above the revert threshold %.2f", old, e.ctrl.errorRevertRate)
	}
	for range 4 {
		get() // served by io_uring again, no fault
	}
	// Past the oscillation lock three switches in a row set (5 min), or the
	// controller would return before it samples.
	revert := eval(now.Add(6 * time.Minute))
	t.Logf("celeris856 REPROMOTE error_rate=%.4f revert=%v", rec.last.ErrorRate, revert)
	if revert || rec.last.ErrorRate > e.ctrl.errorRevertRate {
		t.Errorf("celeris856: the first sample after the re-promotion reads %.3f and revert=%v: it was judged on "+
			"the time io_uring spent as the standby", rec.last.ErrorRate, revert)
	}
}
