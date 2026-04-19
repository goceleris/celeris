package overload

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/observe"
)

type mockCPU struct {
	val atomic.Value // float64
	err error
}

func (m *mockCPU) Sample() (float64, error) {
	if m.err != nil {
		return -1, m.err
	}
	v, _ := m.val.Load().(float64)
	return v, nil
}
func (m *mockCPU) Close() error  { return nil }
func (m *mockCPU) set(v float64) { m.val.Store(v) }

func runOnce(t *testing.T, mw celeris.HandlerFunc, handler celeris.HandlerFunc, method, path string) *celeristest.ResponseRecorder {
	t.Helper()
	ctx, rec := celeristest.NewContextT(t, method, path, celeristest.WithHandlers(mw, handler))
	_ = ctx.Next()
	return rec
}

// provider wires a fresh collector with a mockCPU. Returns a CollectorProvider closure.
func withMock(m *mockCPU) func() *observe.Collector {
	col := observe.NewCollector()
	col.SetCPUMonitor(m)
	return func() *observe.Collector { return col }
}

// waitForStage polls the controller until the expected stage appears or
// the deadline elapses.
func waitForStage(t *testing.T, ctrl *Controller, want Stage) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ctrl.Stage() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitForStage: want %s, got %s", want, ctrl.Stage())
}

func TestNormalPassThrough(t *testing.T) {
	m := &mockCPU{}
	m.set(0.10)
	mw, ctrl := NewWithController(Config{
		CollectorProvider: withMock(m),
		PollInterval:      10 * time.Millisecond,
	})
	defer ctrl.Stop()

	time.Sleep(40 * time.Millisecond)
	if ctrl.Stage() != StageNormal {
		t.Fatalf("expected Normal, got %s", ctrl.Stage())
	}
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	rec := runOnce(t, mw, handler, "GET", "/")
	if rec.StatusCode != 200 {
		t.Fatalf("status: %d", rec.StatusCode)
	}
}

func TestRejectStageReturns503(t *testing.T) {
	m := &mockCPU{}
	m.set(0.99)
	mw, ctrl := NewWithController(Config{
		CollectorProvider: withMock(m),
		PollInterval:      5 * time.Millisecond,
	})
	defer ctrl.Stop()

	waitForStage(t, ctrl, StageReject)

	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	rec := runOnce(t, mw, handler, "GET", "/anything")
	if rec.StatusCode != 503 {
		t.Fatalf("Reject: got %d want 503", rec.StatusCode)
	}
	if rec.Header("retry-after") == "" {
		t.Fatal("expected retry-after header")
	}
}

func TestExemptPathBypassesReject(t *testing.T) {
	m := &mockCPU{}
	m.set(0.99)
	mw, ctrl := NewWithController(Config{
		CollectorProvider: withMock(m),
		PollInterval:      5 * time.Millisecond,
		ExemptPaths:       []string{"/health"},
	})
	defer ctrl.Stop()

	waitForStage(t, ctrl, StageReject)
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	rec := runOnce(t, mw, handler, "GET", "/health")
	if rec.StatusCode != 200 {
		t.Fatalf("exempt: got %d want 200", rec.StatusCode)
	}
}

func TestReorderDropsLowPriority(t *testing.T) {
	m := &mockCPU{}
	m.set(0.88) // within Reorder range
	mw, ctrl := NewWithController(Config{
		CollectorProvider: withMock(m),
		PollInterval:      5 * time.Millisecond,
		PriorityFunc: func(c *celeris.Context) int {
			if c.Path() == "/low" {
				return -1
			}
			return 1
		},
		PriorityThreshold: 0,
	})
	defer ctrl.Stop()

	waitForStage(t, ctrl, StageReorder)
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }

	rec := runOnce(t, mw, handler, "GET", "/low")
	if rec.StatusCode != 503 {
		t.Fatalf("low priority: got %d", rec.StatusCode)
	}
	rec2 := runOnce(t, mw, handler, "GET", "/high")
	if rec2.StatusCode != 200 {
		t.Fatalf("high priority: got %d", rec2.StatusCode)
	}
}

func TestBackpressureDelay(t *testing.T) {
	m := &mockCPU{}
	m.set(0.92) // Backpressure range
	mw, ctrl := NewWithController(Config{
		CollectorProvider: withMock(m),
		PollInterval:      5 * time.Millisecond,
		BackpressureDelay: 30 * time.Millisecond,
		// No PriorityFunc — all requests are "high" → delayed, not rejected.
	})
	defer ctrl.Stop()

	waitForStage(t, ctrl, StageBackpressure)
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	start := time.Now()
	rec := runOnce(t, mw, handler, "GET", "/delayed")
	elapsed := time.Since(start)
	if rec.StatusCode != 200 {
		t.Fatalf("status: %d", rec.StatusCode)
	}
	if elapsed < 20*time.Millisecond {
		t.Fatalf("delay: elapsed %v, expected ≥ 20ms", elapsed)
	}
}

func TestHysteresisPreventsFlap(t *testing.T) {
	m := &mockCPU{}
	m.set(0.96) // Reject
	mw, ctrl := NewWithController(Config{
		CollectorProvider: withMock(m),
		PollInterval:      5 * time.Millisecond,
		Thresholds: Thresholds{
			Reject:     0.95,
			Hysteresis: 0.05,
		},
	})
	_ = mw
	defer ctrl.Stop()

	waitForStage(t, ctrl, StageReject)
	// CPU drops just below Reject but within hysteresis band — should stay Reject.
	m.set(0.93)
	time.Sleep(40 * time.Millisecond)
	if ctrl.Stage() != StageReject {
		t.Fatalf("hysteresis: expected Reject, got %s", ctrl.Stage())
	}
	// Drop below the hysteresis band → fall out.
	m.set(0.70)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if ctrl.Stage() != StageReject {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ctrl.Stage() == StageReject {
		t.Fatalf("expected stage to drop below Reject, still at %s", ctrl.Stage())
	}
}

func TestStopContextStopsGoroutine(t *testing.T) {
	m := &mockCPU{}
	m.set(0.10)
	before := runtime.NumGoroutine()
	mw, ctrl := NewWithController(Config{
		CollectorProvider: withMock(m),
		PollInterval:      5 * time.Millisecond,
	})
	_ = mw
	time.Sleep(20 * time.Millisecond)
	ctrl.Stop()
	// Give the goroutine a moment to unwind.
	time.Sleep(20 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+1 { // tolerate small variance from test scheduler
		t.Fatalf("goroutine leak: before=%d after=%d", before, after)
	}
}

func TestConcurrentStageReadsAreRaceFree(t *testing.T) {
	m := &mockCPU{}
	m.set(0.50)
	mw, ctrl := NewWithController(Config{
		CollectorProvider: withMock(m),
		PollInterval:      2 * time.Millisecond,
	})
	defer ctrl.Stop()

	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	var wg sync.WaitGroup
	stop := make(chan struct{})
	// 16 goroutines hammering handler.
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = runOnce(t, mw, handler, "GET", "/")
				}
			}
		}()
	}
	// Third goroutine flipping CPU to trigger stage changes.
	wg.Add(1)
	go func() {
		defer wg.Done()
		vals := []float64{0.10, 0.95, 0.50, 0.88}
		for i := 0; i < 40; i++ {
			m.set(vals[i%len(vals)])
			time.Sleep(3 * time.Millisecond)
		}
	}()
	time.Sleep(120 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestPanicsWhenCollectorProviderNil(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic")
		}
	}()
	New(Config{})
}

func TestMockCPUErrorIgnored(t *testing.T) {
	m := &mockCPU{err: errors.New("sampling failed")}
	m.set(0.01)
	mw, ctrl := NewWithController(Config{
		CollectorProvider: withMock(m),
		PollInterval:      5 * time.Millisecond,
	})
	defer ctrl.Stop()
	_ = mw
	time.Sleep(30 * time.Millisecond)
	if ctrl.Stage() != StageNormal {
		t.Fatalf("on sampling error, stage should stay Normal; got %s", ctrl.Stage())
	}
}

func TestStageString(t *testing.T) {
	if StageExpand.String() != "expand" {
		t.Fatalf("Stage.String broken")
	}
}
