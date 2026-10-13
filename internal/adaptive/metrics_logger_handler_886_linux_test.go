//go:build linux

package adaptive

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// A Config.Logger handler may call Engine.Metrics() (what Server.EngineInfo
// does) from a record the adaptive engine writes while it holds one of its own
// locks. runEvalLoop holds switchMu across the controller's evaluation, whose
// records ("engine switch recommended", "oscillation lock expired", the debug
// tick) go through that Logger, and performSwitch holds mu while it logs
// ("engine switch completed"; the controller's "oscillation detected" is
// written under mu and switchMu); a Metrics() that takes any of those locks
// deadlocks the goroutine that is logging (celeris#886 item 12, review round 2
// of the Metrics fix).

type metricsFromHandler886 struct {
	e    atomic.Pointer[Engine]
	only string // when set, only a record with this message calls Metrics

	mu       sync.Mutex
	returned map[string]bool // record message -> a Metrics() call from its handler returned
	started  map[string]bool // ... was entered
	signal   chan struct{}
}

func newMetricsFromHandler886(only string) *metricsFromHandler886 {
	return &metricsFromHandler886{only: only, returned: map[string]bool{}, started: map[string]bool{}, signal: make(chan struct{}, 1)}
}

func (h *metricsFromHandler886) Enabled(context.Context, slog.Level) bool { return true }
func (h *metricsFromHandler886) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *metricsFromHandler886) WithGroup(string) slog.Handler            { return h }
func (h *metricsFromHandler886) Handle(_ context.Context, r slog.Record) error {
	e := h.e.Load()
	if e == nil || (h.only != "" && r.Message != h.only) {
		return nil
	}
	h.mu.Lock()
	h.started[r.Message] = true
	h.mu.Unlock()
	_ = e.Metrics()
	h.mu.Lock()
	h.returned[r.Message] = true
	h.mu.Unlock()
	select {
	case h.signal <- struct{}{}:
	default:
	}
	return nil
}

func (h *metricsFromHandler886) has(msgs ...string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range msgs {
		if !h.returned[m] {
			return false
		}
	}
	return true
}

func (h *metricsFromHandler886) stuck() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var s []string
	for m := range h.started {
		if !h.returned[m] {
			s = append(s, m)
		}
	}
	sort.Strings(s)
	return strings.Join(s, ", ")
}

func (h *metricsFromHandler886) await(t *testing.T, d time.Duration, msgs ...string) {
	t.Helper()
	deadline := time.After(d)
	for !h.has(msgs...) {
		select {
		case <-h.signal:
		case <-deadline:
			t.Fatalf("celeris886 LOGGER DEADLOCK: a log handler's Metrics() did not return in %v for %q (stuck in Metrics on: [%s])",
				d, msgs, h.stuck())
		}
	}
}

func newLoggedEngine886(t *testing.T, h *metricsFromHandler886) *Engine {
	t.Helper()
	sampler := newSyntheticSampler()
	sampler.Set(engine.Epoll, TelemetrySnapshot{ConnsPerWorker: 10000})
	e := newFromEngines(newMockEngine(engine.Epoll), newMockEngine(engine.IOUring), sampler,
		resource.Config{Protocol: engine.HTTP1, Logger: slog.New(h)})
	e.ctrl.cooldown = 0
	e.ctrl.evalInterval = 10 * time.Millisecond
	h.e.Store(e)
	return e
}

// stopEvalLoop886 stops the loop and waits for it, but only briefly: on the
// defect this file guards the loop is stuck inside a log handler for good, and
// the test must fail with that message, not hang to the package timeout.
func stopEvalLoop886(cancel context.CancelFunc, done <-chan struct{}) {
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

// The eval loop evaluates under switchMu and the controller logs "engine switch
// recommended" from inside the evaluation: a handler that calls Metrics() on
// that record must not wait for the lock the loop itself holds.
func TestMetricsFromALogHandlerOnSwitchRecommended886(t *testing.T) {
	const rec = "engine switch recommended"
	h := newMetricsFromHandler886(rec)
	e := newLoggedEngine886(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); e.runEvalLoop(ctx) }()
	defer stopEvalLoop886(cancel, done)
	h.await(t, 3*time.Second, rec)
}

// Every record the engine writes while it runs a switch: the controller's
// recommendation and the debug tick (both under switchMu), and performSwitch's
// own records such as "engine switch completed" (under mu). Metrics() takes
// none of those locks.
func TestMetricsFromALogHandlerOnEveryRecordOfASwitch886(t *testing.T) {
	old := adaptiveDebugEnabled
	adaptiveDebugEnabled = true // the per-tick record is only written in debug mode
	defer func() { adaptiveDebugEnabled = old }()

	h := newMetricsFromHandler886("")
	e := newLoggedEngine886(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); e.runEvalLoop(ctx) }()
	defer stopEvalLoop886(cancel, done)
	h.await(t, 3*time.Second, "adaptive_tick", "engine switch recommended", "engine switch completed")
}
