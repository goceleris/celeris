package circuitbreaker

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// celeris#921's family: a request whose handler panicked recorded a failure
// and re-panicked, skipping the state transition that a returned failure
// makes. A half-open probe that panicked therefore never moved the breaker to
// Open nor gave its probe slot back: the breaker stayed HalfOpen with every
// slot spent, and every later request got 503, with no cooldown to end it.
// A panic in the Closed state did not trip the breaker either, however many
// panics the window held, until some request returned normally.

// panicRun runs one GET / through mw and h and returns the status the router
// would answer (a returned HTTPError's code, 500 for another error) and the
// panic, if the chain panicked.
func panicRun(mw, h celeris.HandlerFunc) (status int, panicked any) {
	c, rec := celeristest.NewContext("GET", "/", celeristest.WithHandlers(mw, h))
	defer celeristest.ReleaseContext(c)
	func() {
		defer func() { panicked = recover() }()
		if err := c.Next(); err != nil {
			var he *celeris.HTTPError
			if errors.As(err, &he) {
				rec.StatusCode = he.Code
			} else {
				rec.StatusCode = 500
			}
		}
	}()
	return rec.StatusCode, panicked
}

// switchHandler answers per mode: 0 a returned 500, 1 a panic, 2 200 "ok".
func switchHandler(mode *atomic.Int32) celeris.HandlerFunc {
	return func(c *celeris.Context) error {
		switch mode.Load() {
		case 0:
			return celeris.NewHTTPError(500, "down")
		case 1:
			panic("probe panicked")
		}
		return c.String(200, "ok")
	}
}

// TestCircuitBreakerHalfOpenProbePanic921: a half-open probe that panics is a
// failed probe: the breaker reopens (and its cooldown ends it again), exactly
// as for a probe that returns a failure. The panic still reaches the caller.
func TestCircuitBreakerHalfOpenProbePanic921(t *testing.T) {
	const cooldown = 20 * time.Millisecond
	var mode atomic.Int32
	h := switchHandler(&mode)
	mw, brk := NewWithBreaker(Config{Threshold: 0.5, MinRequests: 1, CooldownPeriod: cooldown, HalfOpenMax: 1, WindowSize: time.Second})

	if st, _ := panicRun(mw, h); st != 500 || brk.State() != Open {
		t.Fatalf("setup: a failing request answered %d with the breaker %v, want 500 and open", st, brk.State())
	}
	time.Sleep(2 * cooldown)
	mode.Store(1)
	if _, p := panicRun(mw, h); p != "probe panicked" {
		t.Fatalf("the half-open probe: panicked with %v, want its own panic", p)
	}
	if got := brk.State(); got != Open {
		t.Errorf("after a half-open probe panicked the breaker is %v, want open (a panicking probe is a failed probe)", got)
	}

	// The backend is healthy again: once the cooldown has passed, the next
	// probe closes the breaker and the requests after it are answered.
	mode.Store(2)
	time.Sleep(2 * cooldown)
	ok := 0
	for range 5 {
		if st, _ := panicRun(mw, h); st == 200 {
			ok++
		}
	}
	if ok != 5 || brk.State() != Closed {
		t.Errorf("healthy backend after the panicking probe: %d of 5 requests answered 200, breaker %v; want 5 and closed", ok, brk.State())
	}
	t.Logf("healthy requests answered 200: %d of 5; state %v", ok, brk.State())
}

// TestCircuitBreakerClosedPanicTrips921: in the Closed state a panic is a
// failure that counts toward the threshold at once, as a returned failure
// does: with MinRequests 1 and Threshold 0.5, one panic opens the breaker.
func TestCircuitBreakerClosedPanicTrips921(t *testing.T) {
	var mode atomic.Int32
	mode.Store(1)
	h := switchHandler(&mode)
	var transitions atomic.Int32
	mw, brk := NewWithBreaker(Config{
		Threshold: 0.5, MinRequests: 1, CooldownPeriod: time.Minute, WindowSize: time.Second,
		OnStateChange: func(from, to State) {
			if from == Closed && to == Open {
				transitions.Add(1)
			}
		},
	})
	if _, p := panicRun(mw, h); p != "probe panicked" {
		t.Fatalf("panicked with %v, want the handler's panic", p)
	}
	if got := brk.State(); got != Open || transitions.Load() != 1 {
		t.Errorf("after a panic with MinRequests 1 the breaker is %v (%d Closed->Open transitions), want open (1)", got, transitions.Load())
	}
	mode.Store(2)
	if st, _ := panicRun(mw, h); st != 503 {
		t.Errorf("the next request answered %d, want 503 from the open breaker", st)
	}
}
