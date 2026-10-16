//go:build linux

package adaptive

import (
	"bytes"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

// celeris#886 item 9 (user decision 2026-10-13): the error rate that drives the
// io_uring safety revert is the interval's faults divided by the larger of its
// requests and its connection-seconds, computed in the sampler only. A real
// send fault on a streaming workload (SSE, a WebSocket hub: a handful of
// requests, many live connections) used to be divided by that handful.

func near886(got, want float64) bool { return math.Abs(got-want) <= 1e-9 }

// TestFaultRateIsPerUnitOfWork886 is the arithmetic.
func TestFaultRateIsPerUnitOfWork886(t *testing.T) {
	for _, tc := range []struct {
		name       string
		faults     uint64
		reqs       uint64
		connSecs   float64
		wantRate   float64
		wantDenom  float64
		wantRevert bool
	}{
		// 10 requests, 200 connections for 1 s, 7 send faults: 0.7 before.
		{"sse stream", 7, 10, 200, 0.035, 200, false},
		{"sse stream, more faults", 12, 10, 200, 0.06, 200, true},
		// A request-dominated interval is divided by its requests, as before.
		{"request dominated", 40, 1000, 80, 0.04, 1000, false},
		{"request dominated, revert", 60, 1000, 80, 0.06, 1000, true},
		// One request per connection per second is the point where the two
		// measures meet.
		{"requests equal connection-seconds", 5, 100, 100, 0.05, 100, false},
		// No request in the interval used to read 0 whatever happened.
		{"streams, no request", 10, 0, 100, 0.1, 100, true},
		// Nothing to divide by.
		{"no requests, no connections", 5, 0, 0, 0, 0, false},
		{"no faults", 0, 10, 200, 0, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rate, denom := faultRate(tc.faults, tc.reqs, tc.connSecs)
			if !near886(rate, tc.wantRate) || !near886(denom, tc.wantDenom) {
				t.Errorf("faultRate(%d, %d, %v) = %v / %v, want %v / %v",
					tc.faults, tc.reqs, tc.connSecs, rate, denom, tc.wantRate, tc.wantDenom)
			}
			if got := rate > newController(nil, nil, nil, testLogger()).errorRevertRate; got != tc.wantRevert {
				t.Errorf("rate %v: revert = %v, want %v", rate, got, tc.wantRevert)
			}
		})
	}
}

func TestConnSecondsIsTheTrapezoid886(t *testing.T) {
	for _, tc := range []struct {
		prev, cur int64
		elapsed   float64
		want      float64
	}{
		{200, 200, 1, 200},
		{1000, 0, 1, 500}, // a hub whose subscribers all left in the interval
		{0, 1000, 2, 1000},
		{0, 0, 1, 0},
		{-4, 8, 1, 4}, // a gauge below 0 counts as 0
		{100, 100, 0.25, 25},
	} {
		if got := connSeconds(tc.prev, tc.cur, tc.elapsed); !near886(got, tc.want) {
			t.Errorf("connSeconds(%d, %d, %v) = %v, want %v", tc.prev, tc.cur, tc.elapsed, got, tc.want)
		}
	}
}

// feed886 gives the active io_uring engine a baseline of base, then the
// counters base + delta one second later by an injected clock (the real clock
// of feed856 gives 5 ms), and returns whether the controller recommends a
// revert and the snapshot it decided on.
func feed886(t *testing.T, logger *slog.Logger, base engine.EngineMetrics, delta func(m *engine.EngineMetrics)) (revert bool, snap TelemetrySnapshot) {
	t.Helper()
	iou := newMockEngine(engine.IOUring)
	live := newLiveSampler(nil)
	t0 := time.Now()
	clk := t0
	live.clock = func() time.Time { return clk }
	sampler := &lastSnapSampler{TelemetrySampler: live}
	c := newController(iou, newMockEngine(engine.Epoll), sampler, logger)
	c.cooldown = 0
	c.loadDownRevert = false

	iou.SetMetrics(base)
	if c.evaluate(t0, false) {
		t.Fatal("the baseline tick recommended a switch")
	}
	if sampler.last.ErrorRate != 0 || sampler.last.RateDenominator != 0 || sampler.last.ConnSeconds != 0 {
		t.Errorf("the baseline sample has a rate %v / %v / %v, want 0 (no previous counters)",
			sampler.last.ErrorRate, sampler.last.RateDenominator, sampler.last.ConnSeconds)
	}
	clk = t0.Add(time.Second)
	m := base
	delta(&m)
	iou.SetMetrics(m)
	revert = c.evaluate(clk, false)
	return revert, sampler.last
}

// TestStreamingSampleIsPerConnectionSecond886 is the sample of the issue: 10
// requests, 200 connections, 7 send faults in a second. It read 0.7 and
// reverted; it reads 0.035 and does not. The same stream with 12 faults is
// still a revert: the engine's faults count, per connection-second.
func TestStreamingSampleIsPerConnectionSecond886(t *testing.T) {
	base := engine.EngineMetrics{RequestCount: 1000, ActiveConnections: 200, Workers: 1}
	revert, snap := feed886(t, testLogger(), base, func(m *engine.EngineMetrics) {
		m.RequestCount += 10
		m.ErrorSend = 7
		m.ErrorCount = 7
	})
	if revert {
		t.Errorf("7 send faults on 200 connections over 1 s reverted (error_rate=%v)", snap.ErrorRate)
	}
	if !near886(snap.ErrorRate, 0.035) || !near886(snap.RateDenominator, 200) || !near886(snap.ConnSeconds, 200) {
		t.Errorf("error_rate / denominator / conn_seconds = %v / %v / %v, want 0.035 / 200 / 200",
			snap.ErrorRate, snap.RateDenominator, snap.ConnSeconds)
	}

	revert, snap = feed886(t, testLogger(), base, func(m *engine.EngineMetrics) {
		m.RequestCount += 10
		m.ErrorSend = 12
		m.ErrorCount = 12
	})
	if !revert || !near886(snap.ErrorRate, 0.06) {
		t.Errorf("12 send faults on 200 connections: revert=%v error_rate=%v, want a revert at 0.06", revert, snap.ErrorRate)
	}
}

// TestRequestDominatedSampleKeepsItsRate886: a request/response workload (80
// keep-alive connections, 1000 requests a second) reads as before, the exact
// rates of the 856 tests, and the threshold sits where it did.
func TestRequestDominatedSampleKeepsItsRate886(t *testing.T) {
	base := engine.EngineMetrics{RequestCount: 1000, ActiveConnections: 80, Workers: 1}
	for _, tc := range []struct {
		faults     uint64
		wantRevert bool
	}{{0, false}, {40, false}, {50, false}, {51, true}, {580, true}} {
		revert, snap := feed886(t, testLogger(), base, func(m *engine.EngineMetrics) {
			m.RequestCount += 1000
			m.ErrorSend = tc.faults
			m.ErrorCount = tc.faults
		})
		if revert != tc.wantRevert {
			t.Errorf("%d faults in 1000 requests: revert = %v, want %v (error_rate=%v)", tc.faults, revert, tc.wantRevert, snap.ErrorRate)
		}
		if want := float64(tc.faults) / 1000; !near886(snap.ErrorRate, want) || !near886(snap.RateDenominator, 1000) {
			t.Errorf("%d faults in 1000 requests: error_rate %v / denominator %v, want %v / 1000", tc.faults, snap.ErrorRate, snap.RateDenominator, want)
		}
	}
}

// TestHubThatEmptiesInTheIntervalIsCountedByItsMeanConnections886: 1000
// subscribers at the start, none at the end, 3 requests. The end gauge alone
// would be 0 connection-seconds and the rate the old 7 (20 faults over 3
// requests); the mean of the two gauges is 500.
func TestHubThatEmptiesInTheIntervalIsCountedByItsMeanConnections886(t *testing.T) {
	base := engine.EngineMetrics{RequestCount: 1000, ActiveConnections: 1000, Workers: 1}
	revert, snap := feed886(t, testLogger(), base, func(m *engine.EngineMetrics) {
		m.RequestCount += 3
		m.ActiveConnections = 0
		m.ErrorSend = 20
		m.ErrorCount = 20
	})
	if revert || !near886(snap.ErrorRate, 0.04) || !near886(snap.ConnSeconds, 500) {
		t.Errorf("revert=%v error_rate=%v conn_seconds=%v, want no revert, 0.04, 500", revert, snap.ErrorRate, snap.ConnSeconds)
	}
}

// TestRateSaturatesWhenCountersFall886: counters that read lower than at the
// last sample (a rebase around a switch, a bucket wired ahead of its total)
// give 0, never a huge rate, whatever the connections.
func TestRateSaturatesWhenCountersFall886(t *testing.T) {
	base := engine.EngineMetrics{
		RequestCount: 5000, ErrorCount: 900, ErrorSend: 900, ActiveConnections: 200, Workers: 1,
	}
	for name, delta := range map[string]func(m *engine.EngineMetrics){
		"errors and requests fall": func(m *engine.EngineMetrics) {
			m.RequestCount = 10
			m.ErrorCount, m.ErrorSend = 3, 3
		},
		"errors fall": func(m *engine.EngineMetrics) { m.RequestCount += 10; m.ErrorCount, m.ErrorSend = 0, 0 },
		"bucket ahead of total": func(m *engine.EngineMetrics) {
			m.ErrorSendPeerGone = 700
			m.ErrorCount = 800
		},
		"gauge goes negative": func(m *engine.EngineMetrics) { m.ActiveConnections = -300; m.ErrorCount, m.ErrorSend = 0, 0 },
	} {
		t.Run(name, func(t *testing.T) {
			revert, snap := feed886(t, testLogger(), base, delta)
			if revert || snap.ErrorRate != 0 {
				t.Errorf("revert=%v error_rate=%v, want no revert and 0", revert, snap.ErrorRate)
			}
		})
	}
}

// TestSwitchLineCarriesTheDenominator886: the per-bucket line of celeris#886
// item 10 keeps its attrs and gains the two that make error_rate recomputable
// from it, and no error_* attr that did not move.
func TestSwitchLineCarriesTheDenominator886(t *testing.T) {
	var buf bytes.Buffer
	base := engine.EngineMetrics{RequestCount: 1000, ActiveConnections: 200, Workers: 1}
	revert, snap := feed886(t, slog.New(slog.NewJSONHandler(&buf, nil)), base, func(m *engine.EngineMetrics) {
		m.RequestCount += 10
		m.ErrorSend = 12
		m.ErrorSendPeerGone = 30
		m.ErrorCount = 42
	})
	if !revert {
		t.Fatalf("12 send faults on 200 connections did not revert (error_rate=%v)", snap.ErrorRate)
	}
	line := switchLine856(t, &buf)
	for k, want := range map[string]float64{
		"error_send": 12, "error_send_peer_gone": 30, "error_rate": 0.06,
		"rate_denominator": 200, "conn_seconds": 200, "active_connections": 200,
	} {
		if got, ok := line[k].(float64); !ok || !near886(got, want) {
			t.Errorf("%s = %v, want %v (line %v)", k, line[k], want, line)
		}
	}
	for k := range line {
		if strings.HasPrefix(k, "error_") && k != "error_rate" && k != "error_send" && k != "error_send_peer_gone" {
			t.Errorf("the line names %s although that bucket did not move (line %v)", k, line)
		}
	}
}
