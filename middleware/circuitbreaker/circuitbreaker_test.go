package circuitbreaker

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/internal/testutil"
)

func okHandler(c *celeris.Context) error {
	return c.String(200, "ok")
}

func errHandler500(c *celeris.Context) error {
	return celeris.NewHTTPError(500, "Internal Server Error")
}

// tripBreaker sends MinRequests failing requests to trip the breaker.
func tripBreaker(t *testing.T, mw celeris.HandlerFunc, minReqs int) {
	t.Helper()
	chain := []celeris.HandlerFunc{mw, errHandler500}
	for range minReqs {
		_, _ = testutil.RunChain(t, chain, "GET", "/")
	}
}

func TestClosedPassesThrough(t *testing.T) {
	mw := New(Config{
		Threshold:   0.5,
		MinRequests: 10,
		WindowSize:  100 * time.Millisecond,
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestErrorRateBelowThresholdStaysClosed(t *testing.T) {
	mw, brk := NewWithBreaker(Config{
		Threshold:   0.5,
		MinRequests: 10,
		WindowSize:  1 * time.Second,
	})

	successChain := []celeris.HandlerFunc{mw, okHandler}
	failChain := []celeris.HandlerFunc{mw, errHandler500}

	// 8 successes, 2 failures = 20% error rate (below 50%).
	for range 8 {
		_, err := testutil.RunChain(t, successChain, "GET", "/")
		testutil.AssertNoError(t, err)
	}
	for range 2 {
		_, _ = testutil.RunChain(t, failChain, "GET", "/")
	}

	if brk.State() != Closed {
		t.Fatalf("expected Closed, got %s", brk.State())
	}
}

func TestErrorRateAtThresholdTripsOpen(t *testing.T) {
	mw, brk := NewWithBreaker(Config{
		Threshold:      0.5,
		MinRequests:    2,
		WindowSize:     1 * time.Second,
		CooldownPeriod: 10 * time.Second,
	})

	failChain := []celeris.HandlerFunc{mw, errHandler500}
	for range 2 {
		_, _ = testutil.RunChain(t, failChain, "GET", "/")
	}

	if brk.State() != Open {
		t.Fatalf("expected Open, got %s", brk.State())
	}
}

func TestOpenReturns503(t *testing.T) {
	mw, _ := NewWithBreaker(Config{
		Threshold:      0.5,
		MinRequests:    2,
		WindowSize:     1 * time.Second,
		CooldownPeriod: 10 * time.Second,
	})

	tripBreaker(t, mw, 2)

	// Next request should be rejected.
	_, err := testutil.RunMiddleware(t, mw)
	testutil.AssertHTTPError(t, err, 503)
}

func TestOpenSetsRetryAfterHeader(t *testing.T) {
	mw, _ := NewWithBreaker(Config{
		Threshold:      0.5,
		MinRequests:    2,
		WindowSize:     1 * time.Second,
		CooldownPeriod: 10 * time.Second,
	})

	tripBreaker(t, mw, 2)

	chain := []celeris.HandlerFunc{mw, okHandler}
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithHandlers(chain...))
	_ = ctx.Next()

	// The retry-after header is set on the context's response headers,
	// not written to the recorder (since the error short-circuits).
	var found bool
	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "retry-after" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("retry-after header missing from response headers")
	}
}

func TestOpenToHalfOpenAfterCooldown(t *testing.T) {
	mw, brk := NewWithBreaker(Config{
		Threshold:      0.5,
		MinRequests:    2,
		WindowSize:     100 * time.Millisecond,
		CooldownPeriod: 50 * time.Millisecond,
		HalfOpenMax:    1,
	})

	tripBreaker(t, mw, 2)

	if brk.State() != Open {
		t.Fatalf("expected Open, got %s", brk.State())
	}

	time.Sleep(60 * time.Millisecond)

	// Next request should transition to half-open and pass through.
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	if brk.State() != Closed {
		t.Fatalf("expected Closed after successful probe, got %s", brk.State())
	}
}

func TestHalfOpenSuccessToClosed(t *testing.T) {
	mw, brk := NewWithBreaker(Config{
		Threshold:      0.5,
		MinRequests:    2,
		WindowSize:     100 * time.Millisecond,
		CooldownPeriod: 50 * time.Millisecond,
		HalfOpenMax:    1,
	})

	tripBreaker(t, mw, 2)
	time.Sleep(60 * time.Millisecond)

	chain := []celeris.HandlerFunc{mw, okHandler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)

	if brk.State() != Closed {
		t.Fatalf("expected Closed, got %s", brk.State())
	}
}

func TestHalfOpenFailureToOpen(t *testing.T) {
	mw, brk := NewWithBreaker(Config{
		Threshold:      0.5,
		MinRequests:    2,
		WindowSize:     100 * time.Millisecond,
		CooldownPeriod: 50 * time.Millisecond,
		HalfOpenMax:    1,
	})

	tripBreaker(t, mw, 2)
	time.Sleep(60 * time.Millisecond)

	chain := []celeris.HandlerFunc{mw, errHandler500}
	_, _ = testutil.RunChain(t, chain, "GET", "/")

	if brk.State() != Open {
		t.Fatalf("expected Open, got %s", brk.State())
	}
}

func TestHalfOpenLimitsConcurrent(t *testing.T) {
	mw, brk := NewWithBreaker(Config{
		Threshold:      0.5,
		MinRequests:    2,
		WindowSize:     100 * time.Millisecond,
		CooldownPeriod: 50 * time.Millisecond,
		HalfOpenMax:    1,
	})

	tripBreaker(t, mw, 2)
	time.Sleep(60 * time.Millisecond)

	// First request in half-open: allowed (probe).
	// We need a slow handler so we can test concurrent access.
	slowHandler := func(c *celeris.Context) error {
		time.Sleep(20 * time.Millisecond)
		return c.String(200, "ok")
	}

	var wg sync.WaitGroup
	var allowed, rejected atomic.Int32

	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			chain := []celeris.HandlerFunc{mw, slowHandler}
			ctx, _ := celeristest.NewContext("GET", "/",
				celeristest.WithHandlers(chain...))
			err := ctx.Next()
			celeristest.ReleaseContext(ctx)
			if err != nil {
				var he *celeris.HTTPError
				if errors.As(err, &he) && he.Code == 503 {
					rejected.Add(1)
					return
				}
			}
			allowed.Add(1)
		}()
	}

	wg.Wait()
	_ = brk

	if allowed.Load() < 1 {
		t.Fatal("expected at least 1 allowed request in half-open")
	}
	if rejected.Load() < 1 {
		t.Fatal("expected at least 1 rejected request in half-open")
	}
}

func TestMinRequestsPreventsPrematureTripping(t *testing.T) {
	mw, brk := NewWithBreaker(Config{
		Threshold:      0.5,
		MinRequests:    10,
		WindowSize:     1 * time.Second,
		CooldownPeriod: 10 * time.Second,
	})

	// Send 5 failures (below MinRequests of 10).
	failChain := []celeris.HandlerFunc{mw, errHandler500}
	for range 5 {
		_, _ = testutil.RunChain(t, failChain, "GET", "/")
	}

	if brk.State() != Closed {
		t.Fatalf("expected Closed (below MinRequests), got %s", brk.State())
	}
}

func TestCustomIsError(t *testing.T) {
	mw, brk := NewWithBreaker(Config{
		Threshold:      0.5,
		MinRequests:    2,
		WindowSize:     1 * time.Second,
		CooldownPeriod: 10 * time.Second,
		IsError: func(_ error, statusCode int) bool {
			return statusCode == 429
		},
	})

	// 500s should NOT count as errors with custom IsError.
	failChain := []celeris.HandlerFunc{mw, errHandler500}
	for range 5 {
		_, _ = testutil.RunChain(t, failChain, "GET", "/")
	}

	if brk.State() != Closed {
		t.Fatalf("expected Closed (500 not counted as error), got %s", brk.State())
	}

	// 429s should count. Need enough 429s to exceed threshold.
	// After 5 successes (500 not counted as error) and 10 failures (429),
	// ratio = 10/15 = 0.667 > 0.5.
	handler429 := func(c *celeris.Context) error {
		return celeris.NewHTTPError(429, "Too Many Requests")
	}
	chain429 := []celeris.HandlerFunc{mw, handler429}
	for range 10 {
		_, _ = testutil.RunChain(t, chain429, "GET", "/")
	}

	if brk.State() != Open {
		t.Fatalf("expected Open (429 counted as error), got %s", brk.State())
	}
}

func TestOnStateChangeCallback(t *testing.T) {
	var mu sync.Mutex
	var transitions [][2]State

	mw, _ := NewWithBreaker(Config{
		Threshold:      0.5,
		MinRequests:    2,
		WindowSize:     100 * time.Millisecond,
		CooldownPeriod: 50 * time.Millisecond,
		HalfOpenMax:    1,
		OnStateChange: func(from, to State) {
			mu.Lock()
			transitions = append(transitions, [2]State{from, to})
			mu.Unlock()
		},
	})

	// Trip the breaker: Closed -> Open.
	tripBreaker(t, mw, 2)

	// Wait for cooldown, then send a successful request: Open -> HalfOpen -> Closed.
	time.Sleep(60 * time.Millisecond)
	chain := []celeris.HandlerFunc{mw, okHandler}
	_, _ = testutil.RunChain(t, chain, "GET", "/")

	mu.Lock()
	defer mu.Unlock()

	if len(transitions) < 3 {
		t.Fatalf("expected at least 3 transitions, got %d: %v", len(transitions), transitions)
	}

	// Closed -> Open
	if transitions[0] != [2]State{Closed, Open} {
		t.Fatalf("transition[0]: got %v, want [Closed, Open]", transitions[0])
	}
	// Open -> HalfOpen
	if transitions[1] != [2]State{Open, HalfOpen} {
		t.Fatalf("transition[1]: got %v, want [Open, HalfOpen]", transitions[1])
	}
	// HalfOpen -> Closed
	if transitions[2] != [2]State{HalfOpen, Closed} {
		t.Fatalf("transition[2]: got %v, want [HalfOpen, Closed]", transitions[2])
	}
}

func TestCustomErrorHandler(t *testing.T) {
	customErr := celeris.NewHTTPError(503, "custom circuit open")
	mw := New(Config{
		Threshold:      0.5,
		MinRequests:    2,
		WindowSize:     1 * time.Second,
		CooldownPeriod: 10 * time.Second,
		ErrorHandler: func(c *celeris.Context, _ error) error {
			return customErr
		},
	})

	tripBreaker(t, mw, 2)

	_, err := testutil.RunMiddleware(t, mw)
	if err != customErr {
		t.Fatalf("expected custom error, got %v", err)
	}
}

func TestSkipPath(t *testing.T) {
	mw, brk := NewWithBreaker(Config{
		Threshold:      0.5,
		MinRequests:    2,
		WindowSize:     1 * time.Second,
		CooldownPeriod: 10 * time.Second,
		SkipPaths:      []string{"/health"},
	})

	// Trip the breaker.
	tripBreaker(t, mw, 2)

	if brk.State() != Open {
		t.Fatalf("expected Open, got %s", brk.State())
	}

	// Skipped path should pass through even when open.
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/health")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestWindowExpiry(t *testing.T) {
	mw, brk := NewWithBreaker(Config{
		Threshold:      0.5,
		MinRequests:    2,
		WindowSize:     50 * time.Millisecond,
		CooldownPeriod: 10 * time.Second,
	})

	// Send failures but within the window.
	failChain := []celeris.HandlerFunc{mw, errHandler500}
	_, _ = testutil.RunChain(t, failChain, "GET", "/")

	// Wait for window to expire.
	time.Sleep(70 * time.Millisecond)

	// Send one more failure. Total in window is now 1 (below MinRequests=2).
	_, _ = testutil.RunChain(t, failChain, "GET", "/")

	if brk.State() != Closed {
		t.Fatalf("expected Closed (old failures expired), got %s", brk.State())
	}
}

func TestConcurrentSafety(t *testing.T) {
	mw := New(Config{
		Threshold:      0.5,
		MinRequests:    2,
		WindowSize:     100 * time.Millisecond,
		CooldownPeriod: 50 * time.Millisecond,
		HalfOpenMax:    1,
	})

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			chain := []celeris.HandlerFunc{mw, func(c *celeris.Context) error {
				if time.Now().UnixNano()%2 == 0 {
					return celeris.NewHTTPError(500, "fail")
				}
				return c.String(200, "ok")
			}}
			ctx, _ := celeristest.NewContext("GET", "/",
				celeristest.WithHandlers(chain...))
			_ = ctx.Next()
			celeristest.ReleaseContext(ctx)
		}()
	}
	wg.Wait()
}

func TestResetMethod(t *testing.T) {
	mw, brk := NewWithBreaker(Config{
		Threshold:      0.5,
		MinRequests:    2,
		WindowSize:     1 * time.Second,
		CooldownPeriod: 10 * time.Second,
	})

	tripBreaker(t, mw, 2)

	if brk.State() != Open {
		t.Fatalf("expected Open, got %s", brk.State())
	}

	brk.Reset()

	if brk.State() != Closed {
		t.Fatalf("expected Closed after Reset, got %s", brk.State())
	}

	// Should be able to pass requests again.
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestDefaultConfigWorks(t *testing.T) {
	mw := New()
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestStateString(t *testing.T) {
	tests := []struct {
		state State
		want  string
	}{
		{Closed, "closed"},
		{Open, "open"},
		{HalfOpen, "half-open"},
		{State(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("State(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

func TestValidatePanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for invalid Threshold")
		}
	}()
	New(Config{Threshold: 1.5})
}

func TestValidateNegativeCooldown(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for negative CooldownPeriod")
		}
	}()
	validate(Config{
		Threshold:      0.5,
		MinRequests:    10,
		WindowSize:     10 * time.Second,
		CooldownPeriod: -1 * time.Second,
		HalfOpenMax:    1,
	})
}

func TestValidateNegativeHalfOpenMax(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for negative HalfOpenMax")
		}
	}()
	validate(Config{
		Threshold:      0.5,
		MinRequests:    10,
		WindowSize:     10 * time.Second,
		CooldownPeriod: 30 * time.Second,
		HalfOpenMax:    -1,
	})
}

func TestValidateTinyWindowSize(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for tiny WindowSize")
		}
	}()
	validate(Config{
		Threshold:      0.5,
		MinRequests:    10,
		WindowSize:     1 * time.Nanosecond,
		CooldownPeriod: 30 * time.Second,
		HalfOpenMax:    1,
	})
}

func TestValidateNegativeMinRequests(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for negative MinRequests")
		}
	}()
	validate(Config{
		Threshold:      0.5,
		MinRequests:    -5,
		WindowSize:     10 * time.Second,
		CooldownPeriod: 30 * time.Second,
		HalfOpenMax:    1,
	})
}

func TestCounts(t *testing.T) {
	mw, brk := NewWithBreaker(Config{
		Threshold:      0.9,
		MinRequests:    100,
		WindowSize:     1 * time.Second,
		CooldownPeriod: 10 * time.Second,
	})

	successChain := []celeris.HandlerFunc{mw, okHandler}
	failChain := []celeris.HandlerFunc{mw, errHandler500}

	for range 5 {
		_, _ = testutil.RunChain(t, successChain, "GET", "/")
	}
	for range 3 {
		_, _ = testutil.RunChain(t, failChain, "GET", "/")
	}

	total, failures := brk.Counts()
	if total != 8 {
		t.Fatalf("expected total=8, got %d", total)
	}
	if failures != 3 {
		t.Fatalf("expected failures=3, got %d", failures)
	}
}
