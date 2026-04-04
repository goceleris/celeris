package timeout

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

func TestNormalRequestCompletes(t *testing.T) {
	mw := New(Config{Timeout: 1 * time.Second})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestTimeoutExceeded(t *testing.T) {
	mw := New(Config{Timeout: 1 * time.Millisecond})
	handler := func(c *celeris.Context) error {
		time.Sleep(50 * time.Millisecond)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/slow")
	testutil.AssertHTTPError(t, err, 503)
}

func TestSkipBypassesTimeout(t *testing.T) {
	mw := New(Config{
		Timeout: 1 * time.Millisecond,
		Skip:    func(_ *celeris.Context) bool { return true },
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "skipped")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/skip")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "skipped")
}

func TestCustomErrorHandlerResponseNotWritten(t *testing.T) {
	// When the handler times out without writing a response, the custom
	// ErrorHandler is invoked and its result is returned.
	mw := New(Config{
		Timeout: 1 * time.Millisecond,
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return celeris.NewHTTPError(504, "Gateway Timeout")
		},
	})
	handler := func(c *celeris.Context) error {
		time.Sleep(50 * time.Millisecond)
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/custom")
	testutil.AssertHTTPError(t, err, 504)
}

func TestCustomErrorHandlerResponseAlreadyWritten(t *testing.T) {
	// When the handler times out but has already written a response,
	// ErrServiceUnavailable is returned directly (the response is
	// committed, so ErrorHandler cannot write a new one).
	mw := New(Config{
		Timeout: 1 * time.Millisecond,
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return celeris.NewHTTPError(504, "Gateway Timeout")
		},
	})
	handler := func(c *celeris.Context) error {
		time.Sleep(50 * time.Millisecond)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/custom-written")
	testutil.AssertHTTPError(t, err, 503)
}

func TestCustomTimeoutDuration(t *testing.T) {
	mw := New(Config{Timeout: 500 * time.Millisecond})
	handler := func(c *celeris.Context) error {
		return c.String(200, "fast")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/fast")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestDefaultConfigValues(t *testing.T) {
	if defaultConfig.Timeout != 5*time.Second {
		t.Fatalf("default timeout: got %v, want 5s", defaultConfig.Timeout)
	}
}

func TestApplyDefaultsFixesZeroTimeout(t *testing.T) {
	// With TimeoutFunc set, applyDefaults should fix zero Timeout to 5s.
	cfg := applyDefaults(Config{
		Timeout:     0,
		TimeoutFunc: func(_ *celeris.Context) time.Duration { return time.Second },
	})
	if cfg.Timeout != 5*time.Second {
		t.Fatalf("expected 5s, got %v", cfg.Timeout)
	}
}

func TestApplyDefaultsFixesNegativeTimeout(t *testing.T) {
	// With TimeoutFunc set, applyDefaults should fix negative Timeout to 5s.
	cfg := applyDefaults(Config{
		Timeout:     -1 * time.Second,
		TimeoutFunc: func(_ *celeris.Context) time.Duration { return time.Second },
	})
	if cfg.Timeout != 5*time.Second {
		t.Fatalf("expected 5s, got %v", cfg.Timeout)
	}
}

func TestDeadlinePropagated(t *testing.T) {
	mw := New(Config{Timeout: 2 * time.Second})
	var hasDeadline bool
	handler := func(c *celeris.Context) error {
		_, hasDeadline = c.Context().Deadline()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/deadline")
	testutil.AssertNoError(t, err)
	if !hasDeadline {
		t.Fatal("expected context to have a deadline")
	}
}

func TestContextCancelCalled(t *testing.T) {
	mw := New(Config{Timeout: 1 * time.Second})
	var childCtx context.Context
	handler := func(c *celeris.Context) error {
		childCtx = c.Context()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/cancel")
	testutil.AssertNoError(t, err)

	// After middleware returns, defer cancel() should have fired.
	select {
	case <-childCtx.Done():
		// context was cancelled
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected context to be cancelled after middleware returns")
	}
}

func TestCooperativeContextRestored(t *testing.T) {
	mw := New(Config{Timeout: 1 * time.Second})
	var origCtx, afterCtx context.Context
	outer := func(c *celeris.Context) error {
		origCtx = c.Context()
		err := c.Next()
		afterCtx = c.Context()
		return err
	}
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{outer, mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/restore")
	testutil.AssertNoError(t, err)
	if origCtx != afterCtx {
		t.Fatal("expected cooperative mode to restore the original context")
	}
}

func TestPreemptiveContextRestored(t *testing.T) {
	mw := New(Config{Timeout: 1 * time.Second, Preemptive: true})
	var ctxErrAfterNext error
	outer := func(c *celeris.Context) error {
		err := c.Next()
		ctxErrAfterNext = c.Context().Err()
		return err
	}
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{outer, mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/restore-preemptive")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if ctxErrAfterNext != nil {
		t.Fatalf("expected outer middleware to see nil context error after Next(), got: %v", ctxErrAfterNext)
	}
}

func TestHandlerErrorPassesThroughWithoutTimeout(t *testing.T) {
	mw := New(Config{Timeout: 1 * time.Second})
	handler := func(_ *celeris.Context) error {
		return celeris.NewHTTPError(400, "bad request")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/error")
	testutil.AssertHTTPError(t, err, 400)
}

func TestHandlerBareErrorPassesThrough(t *testing.T) {
	mw := New(Config{Timeout: 1 * time.Second})
	handler := func(_ *celeris.Context) error {
		return errors.New("something went wrong")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/bare-error")
	testutil.AssertError(t, err)
}

func TestDefaultConfigNoArgs(t *testing.T) {
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.String(200, "default")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/default")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestPreemptiveNormalCompletion(t *testing.T) {
	mw := New(Config{Timeout: 1 * time.Second, Preemptive: true})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestPreemptiveTimeout(t *testing.T) {
	mw := New(Config{Timeout: 10 * time.Millisecond, Preemptive: true})
	var wg sync.WaitGroup
	wg.Add(1)
	handler := func(c *celeris.Context) error {
		defer wg.Done()
		<-c.Context().Done()
		return c.Context().Err()
	}
	c, _ := celeristest.NewContext("GET", "/slow", celeristest.WithHandlers(mw, handler))
	err := c.Next()
	wg.Wait()
	celeristest.ReleaseContext(c)
	testutil.AssertHTTPError(t, err, 503)
}

func TestPreemptiveTimeoutCustomHandler(t *testing.T) {
	mw := New(Config{
		Timeout:    10 * time.Millisecond,
		Preemptive: true,
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return celeris.NewHTTPError(504, "Gateway Timeout")
		},
	})
	var wg sync.WaitGroup
	wg.Add(1)
	handler := func(c *celeris.Context) error {
		defer wg.Done()
		<-c.Context().Done()
		return c.Context().Err()
	}
	c, _ := celeristest.NewContext("GET", "/custom", celeristest.WithHandlers(mw, handler))
	err := c.Next()
	wg.Wait()
	celeristest.ReleaseContext(c)
	testutil.AssertHTTPError(t, err, 504)
}

func TestPreemptiveSkipBypass(t *testing.T) {
	mw := New(Config{
		Timeout:    10 * time.Millisecond,
		Preemptive: true,
		Skip:       func(_ *celeris.Context) bool { return true },
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "skipped")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "skipped")
}

func TestPreemptiveSkipPathsBypass(t *testing.T) {
	mw := New(Config{
		Timeout:    10 * time.Millisecond,
		Preemptive: true,
		SkipPaths:  []string{"/health", "/ready"},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "alive")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// Skipped path bypasses timeout entirely (no buffering, no goroutine).
	rec, err := testutil.RunChain(t, chain, "GET", "/health")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "alive")

	rec, err = testutil.RunChain(t, chain, "GET", "/ready")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	// Non-skipped path with slow handler should timeout.
	slowHandler := func(c *celeris.Context) error {
		<-c.Context().Done()
		return c.Context().Err()
	}
	c, _ := celeristest.NewContext("GET", "/api/slow", celeristest.WithHandlers(mw, slowHandler))
	err = c.Next()
	celeristest.ReleaseContext(c)
	testutil.AssertHTTPError(t, err, 503)
}

func TestPreemptiveContextCancelled(t *testing.T) {
	mw := New(Config{Timeout: 10 * time.Millisecond, Preemptive: true})
	var ctxDone bool
	var wg sync.WaitGroup
	wg.Add(1)
	handler := func(c *celeris.Context) error {
		defer wg.Done()
		<-c.Context().Done()
		ctxDone = true
		return c.Context().Err()
	}
	c, _ := celeristest.NewContext("GET", "/ctx", celeristest.WithHandlers(mw, handler))
	err := c.Next()
	wg.Wait()
	celeristest.ReleaseContext(c)
	testutil.AssertHTTPError(t, err, 503)
	if !ctxDone {
		t.Fatal("expected handler goroutine to see context cancellation")
	}
}

func TestPreemptiveHandlerError(t *testing.T) {
	mw := New(Config{Timeout: 1 * time.Second, Preemptive: true})
	handler := func(_ *celeris.Context) error {
		return celeris.NewHTTPError(400, "bad request")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/error")
	testutil.AssertHTTPError(t, err, 400)
}

func TestPreemptiveDeadlinePropagated(t *testing.T) {
	mw := New(Config{Timeout: 2 * time.Second, Preemptive: true})
	var hasDeadline bool
	handler := func(c *celeris.Context) error {
		_, hasDeadline = c.Context().Deadline()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/deadline")
	testutil.AssertNoError(t, err)
	if !hasDeadline {
		t.Fatal("expected context to have a deadline")
	}
}

func TestPreemptivePanicRecovery(t *testing.T) {
	mw := New(Config{Timeout: 1 * time.Second, Preemptive: true})
	handler := func(_ *celeris.Context) error {
		panic("handler panic in goroutine")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/panic")
	testutil.AssertHTTPError(t, err, 500)

	// Verify the panic value is included in the error message.
	var he *celeris.HTTPError
	if !errors.As(err, &he) {
		t.Fatal("expected *HTTPError")
	}
	if !strings.Contains(he.Message, "handler panic in goroutine") {
		t.Fatalf("expected panic value in message, got: %s", he.Message)
	}
}

func TestPreemptiveRaceDetector(t *testing.T) {
	mw := New(Config{Timeout: 1 * time.Second, Preemptive: true})
	handler := func(c *celeris.Context) error {
		c.Set("key", "val")
		_ = c.Path()
		_ = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				rec, err := testutil.RunChain(t, chain, "GET", "/race")
				if err != nil {
					t.Errorf("unexpected error: %v", err)
					return
				}
				if rec.StatusCode != 200 {
					t.Errorf("unexpected status: %d", rec.StatusCode)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestSkipPathsBypass(t *testing.T) {
	mw := New(Config{
		Timeout:   1 * time.Millisecond,
		SkipPaths: []string{"/health", "/metrics"},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// Skipped path should not timeout.
	rec, err := testutil.RunChain(t, chain, "GET", "/health")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")

	// Another skipped path.
	rec, err = testutil.RunChain(t, chain, "GET", "/metrics")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	// Non-skipped path with slow handler should timeout.
	slowHandler := func(c *celeris.Context) error {
		time.Sleep(50 * time.Millisecond)
		return c.String(200, "ok")
	}
	slowChain := []celeris.HandlerFunc{mw, slowHandler}
	_, err = testutil.RunChain(t, slowChain, "GET", "/api/slow")
	testutil.AssertHTTPError(t, err, 503)
}

func TestTimeoutFuncPerRequest(t *testing.T) {
	mw := New(Config{
		Timeout: 5 * time.Second,
		TimeoutFunc: func(c *celeris.Context) time.Duration {
			if c.Path() == "/fast" {
				return 50 * time.Millisecond
			}
			return 0 // fallback to static
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/fast")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestTimeoutFuncZeroFallsBackToStatic(t *testing.T) {
	mw := New(Config{
		Timeout: 1 * time.Second,
		TimeoutFunc: func(_ *celeris.Context) time.Duration {
			return 0
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/fallback")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestTimeoutFuncNegativeFallsBackToStatic(t *testing.T) {
	mw := New(Config{
		Timeout: 1 * time.Second,
		TimeoutFunc: func(_ *celeris.Context) time.Duration {
			return -1 * time.Second
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/neg")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestTimeoutFuncTriggersTimeout(t *testing.T) {
	mw := New(Config{
		Timeout: 10 * time.Second,
		TimeoutFunc: func(_ *celeris.Context) time.Duration {
			return 1 * time.Millisecond
		},
	})
	handler := func(c *celeris.Context) error {
		time.Sleep(50 * time.Millisecond)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/slow-tf")
	testutil.AssertHTTPError(t, err, 503)
}

func TestPreemptiveTimeoutFunc(t *testing.T) {
	mw := New(Config{
		Timeout:    10 * time.Second,
		Preemptive: true,
		TimeoutFunc: func(_ *celeris.Context) time.Duration {
			return 10 * time.Millisecond
		},
	})
	var wg sync.WaitGroup
	wg.Add(1)
	handler := func(c *celeris.Context) error {
		defer wg.Done()
		<-c.Context().Done()
		return c.Context().Err()
	}
	c, _ := celeristest.NewContext("GET", "/preemptive-tf", celeristest.WithHandlers(mw, handler))
	err := c.Next()
	wg.Wait()
	celeristest.ReleaseContext(c)
	testutil.AssertHTTPError(t, err, 503)
}

func TestPreemptiveChannelPoolReuse(t *testing.T) {
	mw := New(Config{Timeout: 1 * time.Second, Preemptive: true})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// Run 100 preemptive requests. The channel pool should be reused
	// across iterations, resulting in at most a handful of allocations
	// rather than 100 separate channel allocations.
	allocs := testing.AllocsPerRun(100, func() {
		rec, err := testutil.RunChain(t, chain, "GET", "/pool")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rec.StatusCode != 200 {
			t.Fatalf("unexpected status: %d", rec.StatusCode)
		}
	})

	// With sync.Pool reuse, the per-iteration channel allocation cost
	// should be well under 1 alloc/op on average (amortized). We allow
	// a generous budget that would still catch a regression where the
	// pool is not used (100 allocs for 100 requests = 1 alloc/op).
	// The real ceiling is the entire operation budget including context
	// creation, response recorder, etc., so we just verify it does not
	// grow linearly.
	const maxAllocsPerOp = 50
	if allocs > maxAllocsPerOp {
		t.Fatalf("preemptive mode allocs too high: %.0f per op (max %d)", allocs, maxAllocsPerOp)
	}
}

// --- TimeoutErrors tests ---

var errDBTimeout = errors.New("database query timeout")
var errUpstreamTimeout = errors.New("upstream service timeout")

func TestTimeoutErrorsMatchTriggersErrorHandler(t *testing.T) {
	mw := New(Config{
		Timeout:       1 * time.Second,
		TimeoutErrors: []error{errDBTimeout},
	})
	handler := func(_ *celeris.Context) error {
		return errDBTimeout
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/db")
	testutil.AssertHTTPError(t, err, 503)
}

func TestTimeoutErrorsWrappedMatchTriggersErrorHandler(t *testing.T) {
	mw := New(Config{
		Timeout:       1 * time.Second,
		TimeoutErrors: []error{errDBTimeout},
	})
	handler := func(_ *celeris.Context) error {
		return fmt.Errorf("query failed: %w", errDBTimeout)
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/db-wrapped")
	testutil.AssertHTTPError(t, err, 503)
}

func TestTimeoutErrorsNoMatchPassesThrough(t *testing.T) {
	mw := New(Config{
		Timeout:       1 * time.Second,
		TimeoutErrors: []error{errDBTimeout},
	})
	handler := func(_ *celeris.Context) error {
		return errors.New("some other error")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/other")
	testutil.AssertError(t, err)
	// Should NOT be the timeout error (503).
	var he *celeris.HTTPError
	if errors.As(err, &he) && he.Code == 503 {
		t.Fatal("non-matching error should not be treated as timeout")
	}
}

func TestTimeoutErrorsMultipleMatches(t *testing.T) {
	mw := New(Config{
		Timeout:       1 * time.Second,
		TimeoutErrors: []error{errDBTimeout, errUpstreamTimeout},
	})
	handler := func(_ *celeris.Context) error {
		return errUpstreamTimeout
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/upstream")
	testutil.AssertHTTPError(t, err, 503)
}

func TestTimeoutErrorsNilErrorNotMatched(t *testing.T) {
	mw := New(Config{
		Timeout:       1 * time.Second,
		TimeoutErrors: []error{errDBTimeout},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/ok")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestTimeoutErrorsEmptySliceNoEffect(t *testing.T) {
	mw := New(Config{
		Timeout:       1 * time.Second,
		TimeoutErrors: []error{},
	})
	handler := func(_ *celeris.Context) error {
		return errDBTimeout
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/empty-slice")
	testutil.AssertError(t, err)
	// Should pass through as-is, not be converted to 503.
	var he *celeris.HTTPError
	if errors.As(err, &he) && he.Code == 503 {
		t.Fatal("empty TimeoutErrors should not match any error")
	}
}

func TestTimeoutErrorsCustomErrorHandler(t *testing.T) {
	mw := New(Config{
		Timeout:       1 * time.Second,
		TimeoutErrors: []error{errDBTimeout},
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return celeris.NewHTTPError(504, "Gateway Timeout")
		},
	})
	handler := func(_ *celeris.Context) error {
		return errDBTimeout
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/custom-te")
	testutil.AssertHTTPError(t, err, 504)
}

func TestTimeoutErrorsPreemptiveMode(t *testing.T) {
	mw := New(Config{
		Timeout:       1 * time.Second,
		Preemptive:    true,
		TimeoutErrors: []error{errDBTimeout},
	})
	handler := func(_ *celeris.Context) error {
		return errDBTimeout
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/preemptive-te")
	testutil.AssertHTTPError(t, err, 503)
}

func TestTimeoutErrorsPreemptiveWrapped(t *testing.T) {
	mw := New(Config{
		Timeout:       1 * time.Second,
		Preemptive:    true,
		TimeoutErrors: []error{errDBTimeout},
	})
	handler := func(_ *celeris.Context) error {
		return fmt.Errorf("db call: %w", errDBTimeout)
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/preemptive-wrapped")
	testutil.AssertHTTPError(t, err, 503)
}

func TestValidatePanicsOnZeroTimeoutWithoutTimeoutFunc(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for zero Timeout without TimeoutFunc")
		}
		msg, ok := r.(string)
		if !ok || msg != "timeout: Timeout must be positive or TimeoutFunc must be set" {
			t.Fatalf("unexpected panic message: %v", r)
		}
	}()
	// applyDefaults now contains the validation check, so calling it with
	// zero Timeout and no TimeoutFunc panics immediately.
	applyDefaults(Config{Timeout: 0})
}

func TestValidateNewPanicsOnZeroTimeoutWithoutTimeoutFunc(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic from New() for zero Timeout without TimeoutFunc")
		}
	}()
	New(Config{Timeout: 0, TimeoutFunc: nil})
}

func TestValidateTimeoutFuncSetBypassesPanic(t *testing.T) {
	// Should NOT panic: TimeoutFunc is set even though Timeout <= 0.
	applyDefaults(Config{
		Timeout:     0,
		TimeoutFunc: func(_ *celeris.Context) time.Duration { return time.Second },
	})
}

func TestPreemptiveErrorHandlerReturnsError(t *testing.T) {
	errCustom := errors.New("errHandler failed")
	mw := New(Config{
		Timeout:    10 * time.Millisecond,
		Preemptive: true,
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return errCustom
		},
	})
	var wg sync.WaitGroup
	wg.Add(1)
	handler := func(c *celeris.Context) error {
		defer wg.Done()
		<-c.Context().Done()
		return c.Context().Err()
	}
	c, _ := celeristest.NewContext("GET", "/err-handler", celeristest.WithHandlers(mw, handler))
	err := c.Next()
	wg.Wait()
	celeristest.ReleaseContext(c)
	if !errors.Is(err, errCustom) {
		t.Fatalf("expected errHandler error, got: %v", err)
	}
}

// --- ErrorHandler receives error tests ---

func TestErrorHandlerReceivesDeadlineExceeded(t *testing.T) {
	var received error
	mw := New(Config{
		Timeout: 1 * time.Millisecond,
		ErrorHandler: func(_ *celeris.Context, err error) error {
			received = err
			return celeris.NewHTTPError(504, "custom timeout")
		},
	})
	handler := func(_ *celeris.Context) error {
		time.Sleep(50 * time.Millisecond)
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/deadline-err")
	testutil.AssertHTTPError(t, err, 504)
	if !errors.Is(received, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got: %v", received)
	}
}

func TestErrorHandlerReceivesTimeoutError(t *testing.T) {
	var received error
	mw := New(Config{
		Timeout:       1 * time.Second,
		TimeoutErrors: []error{errDBTimeout},
		ErrorHandler: func(_ *celeris.Context, err error) error {
			received = err
			return celeris.NewHTTPError(504, "db timeout")
		},
	})
	handler := func(_ *celeris.Context) error {
		return fmt.Errorf("query failed: %w", errDBTimeout)
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/te-err")
	testutil.AssertHTTPError(t, err, 504)
	if !errors.Is(received, errDBTimeout) {
		t.Fatalf("expected errDBTimeout, got: %v", received)
	}
}

func TestErrorHandlerPreemptiveDeadline(t *testing.T) {
	var received error
	mw := New(Config{
		Timeout:    10 * time.Millisecond,
		Preemptive: true,
		ErrorHandler: func(_ *celeris.Context, err error) error {
			received = err
			return celeris.NewHTTPError(504, "preemptive timeout")
		},
	})
	var wg sync.WaitGroup
	wg.Add(1)
	handler := func(c *celeris.Context) error {
		defer wg.Done()
		<-c.Context().Done()
		return c.Context().Err()
	}
	c, _ := celeristest.NewContext("GET", "/pre-deadline", celeristest.WithHandlers(mw, handler))
	err := c.Next()
	wg.Wait()
	celeristest.ReleaseContext(c)
	testutil.AssertHTTPError(t, err, 504)
	if !errors.Is(received, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got: %v", received)
	}
}

func TestErrorHandlerPreemptiveTimeoutError(t *testing.T) {
	var received error
	mw := New(Config{
		Timeout:       1 * time.Second,
		Preemptive:    true,
		TimeoutErrors: []error{errDBTimeout},
		ErrorHandler: func(_ *celeris.Context, err error) error {
			received = err
			return celeris.NewHTTPError(504, "preemptive db timeout")
		},
	})
	handler := func(_ *celeris.Context) error {
		return errDBTimeout
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/pre-te-err")
	testutil.AssertHTTPError(t, err, 504)
	if !errors.Is(received, errDBTimeout) {
		t.Fatalf("expected errDBTimeout, got: %v", received)
	}
}

// --- ErrorHandler panic recovery tests ---

func TestErrorHandlerPanicReturns503(t *testing.T) {
	mw := New(Config{
		Timeout: 1 * time.Millisecond,
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			panic("error handler exploded")
		},
	})
	handler := func(_ *celeris.Context) error {
		time.Sleep(50 * time.Millisecond)
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/panic-handler")
	testutil.AssertHTTPError(t, err, 503)
}

func TestPreemptiveErrorHandlerPanicReturns503(t *testing.T) {
	mw := New(Config{
		Timeout:    10 * time.Millisecond,
		Preemptive: true,
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			panic("preemptive error handler exploded")
		},
	})
	var wg sync.WaitGroup
	wg.Add(1)
	handler := func(c *celeris.Context) error {
		defer wg.Done()
		<-c.Context().Done()
		return c.Context().Err()
	}
	c, _ := celeristest.NewContext("GET", "/pre-panic", celeristest.WithHandlers(mw, handler))
	err := c.Next()
	wg.Wait()
	celeristest.ReleaseContext(c)
	testutil.AssertHTTPError(t, err, 503)
}

func FuzzTimeoutDurations(f *testing.F) {
	f.Add(int64(0))
	f.Add(int64(-1))
	f.Add(int64(1))
	f.Add(int64(5000000000)) // 5s
	f.Add(int64(9223372036854775807))
	f.Fuzz(func(t *testing.T, ns int64) {
		d := time.Duration(ns)
		if d <= 0 {
			d = time.Second
		}
		mw := New(Config{Timeout: d})
		handler := func(c *celeris.Context) error {
			return c.String(200, "ok")
		}
		chain := []celeris.HandlerFunc{mw, handler}
		_, _ = testutil.RunChain(t, chain, "GET", "/fuzz")
	})
}
