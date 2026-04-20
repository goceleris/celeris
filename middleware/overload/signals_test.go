package overload

import (
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func TestInFlightIsTrackedThroughNext(t *testing.T) {
	m := &mockCPU{}
	m.set(0.0)
	_, ctrl := NewWithController(Config{
		CollectorProvider: withMock(m),
		PollInterval:      50 * time.Millisecond,
	})
	t.Cleanup(ctrl.Stop)

	if got := ctrl.InFlight(); got != 0 {
		t.Fatalf("pre: InFlight=%d, want 0", got)
	}

	mw, _ := NewWithController(Config{
		CollectorProvider: withMock(m),
		PollInterval:      50 * time.Millisecond,
	})

	var peak int32
	var peakMu sync.Mutex
	slow := func(c *celeris.Context) error {
		time.Sleep(20 * time.Millisecond)
		return c.String(200, "ok")
	}
	// The slow handler runs N times in parallel; mid-flight the observed
	// count should be close to concurrency. We reuse the Controller from
	// `mw` — but NewWithController above returned a fresh controller.
	// Use a single NewWithController call to track both.
	mw2, ctrl2 := NewWithController(Config{
		CollectorProvider: withMock(m),
		PollInterval:      50 * time.Millisecond,
	})
	t.Cleanup(ctrl2.Stop)

	const N = 16
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ctx, _ := celeristest.NewContextT(t, "GET", "/ok", celeristest.WithHandlers(mw2, slow))
			_ = ctx.Next()
		}()
	}

	// Sample until handler bodies are in flight.
	dl := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(dl) {
		if n := ctrl2.InFlight(); n > 0 {
			peakMu.Lock()
			if n > peak {
				peak = n
			}
			peakMu.Unlock()
		}
		if peak >= N/2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	wg.Wait()

	// Post-flight: counter should return to zero.
	if got := ctrl2.InFlight(); got != 0 {
		t.Fatalf("post: InFlight=%d, want 0", got)
	}
	if peak < 2 {
		t.Fatalf("peak InFlight = %d; expected at least 2 during %d-way parallelism", peak, N)
	}
	_ = mw // keep unused-variable warning quiet
}

func TestDepthThresholdsEscalateStage(t *testing.T) {
	m := &mockCPU{}
	m.set(0.0) // CPU says normal; depth alone must drive reject.
	mw, ctrl := NewWithController(Config{
		CollectorProvider: withMock(m),
		PollInterval:      50 * time.Millisecond,
		DepthThresholds:   DepthThresholds{Reject: 3},
	})
	t.Cleanup(ctrl.Stop)

	block := make(chan struct{})
	blocking := func(c *celeris.Context) error {
		<-block
		return c.String(200, "ok")
	}

	// Kick off 3 parallel long-running requests so InFlight reaches Reject.
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, _ := celeristest.NewContextT(t, "GET", "/slow", celeristest.WithHandlers(mw, blocking))
			_ = ctx.Next()
		}()
	}

	// Wait for the queue to fill up.
	dl := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(dl) && ctrl.InFlight() < 3 {
		time.Sleep(time.Millisecond)
	}
	if n := ctrl.InFlight(); n < 3 {
		t.Fatalf("failed to saturate depth: InFlight=%d", n)
	}

	// A 4th request should be rejected by the depth signal.
	rejected := func(c *celeris.Context) error {
		t.Fatal("handler reached despite depth Reject")
		return nil
	}
	ctx, rec := celeristest.NewContextT(t, "GET", "/v", celeristest.WithHandlers(mw, rejected))
	_ = ctx.Next()
	if rec.StatusCode != 503 {
		t.Fatalf("expected 503 reject; got %d", rec.StatusCode)
	}

	close(block)
	wg.Wait()
}

func TestLatencyEMAAndLatencyThresholdEscalate(t *testing.T) {
	m := &mockCPU{}
	m.set(0.0)

	mw, ctrl := NewWithController(Config{
		CollectorProvider: withMock(m),
		PollInterval:      10 * time.Millisecond,
		LatencyEMAAlpha:   0.5, // make EMA responsive in tests
		LatencyThresholds: LatencyThresholds{
			Reject: 5 * time.Millisecond,
		},
	})
	t.Cleanup(ctrl.Stop)

	slow := func(c *celeris.Context) error {
		time.Sleep(20 * time.Millisecond)
		return c.String(200, "ok")
	}
	// Feed a handful of slow requests so the EMA climbs past Reject.
	for i := 0; i < 5; i++ {
		ctx, _ := celeristest.NewContextT(t, "GET", "/slow", celeristest.WithHandlers(mw, slow))
		_ = ctx.Next()
	}

	ema := ctrl.LatencyEMA()
	if ema < 5*time.Millisecond {
		t.Fatalf("expected EMA > 5ms after 5 × 20ms samples; got %v", ema)
	}

	// Give the poller one tick to fold latency into the stage.
	dl := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(dl) && ctrl.Stage() < StageReject {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ctrl.Stage(); got != StageReject {
		t.Fatalf("latency should have driven stage to Reject; got %v (ema=%v)", got, ema)
	}
}
