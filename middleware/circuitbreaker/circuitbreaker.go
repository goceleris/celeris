package circuitbreaker

import (
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris"
)

// smallRetryAfter caches the ASCII form of retry-after seconds for
// all values in [0, 3600]. Covers every realistic cooldownPeriod —
// typical production values are 5–300s; 3600 comfortably covers
// "1 hour cooldown" style setups. ~60 KiB of static strings trades
// for zero strconv.FormatInt allocations on the breaker-open hot
// path.
const retryAfterCacheSize = 3601 // inclusive upper bound

var smallRetryAfter = func() [retryAfterCacheSize]string {
	var cache [retryAfterCacheSize]string
	for i := range cache {
		cache[i] = strconv.FormatInt(int64(i), 10)
	}
	return cache
}()

func retryAfterString(n int64) string {
	if n >= 0 && int(n) < retryAfterCacheSize {
		return smallRetryAfter[n]
	}
	return strconv.FormatInt(n, 10)
}

// Breaker holds the circuit breaker state. Use [NewWithBreaker] to obtain
// a reference for programmatic state inspection and reset.
type Breaker struct {
	state        atomic.Int32
	openedAt     atomic.Int64 // UnixNano when breaker opened
	halfOpenUsed atomic.Int32 // probe requests admitted in half-open
	mu           sync.Mutex   // protects state transitions
	window       *slidingWindow

	threshold      float64
	minRequests    int
	cooldownPeriod int64 // nanoseconds
	halfOpenMax    int32
	isError        func(err error, statusCode int) bool
	onStateChange  func(from, to State)
}

// State returns the current circuit breaker state.
func (b *Breaker) State() State {
	return State(b.state.Load())
}

// Counts returns the current sliding window totals.
func (b *Breaker) Counts() (total, failures int64) {
	return b.window.counts()
}

// Reset forces the breaker back to Closed and clears the observation window.
func (b *Breaker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	from := State(b.state.Load())
	b.state.Store(int32(Closed))
	b.halfOpenUsed.Store(0)
	b.window.reset()
	if from != Closed && b.onStateChange != nil {
		b.onStateChange(from, Closed)
	}
}

// New creates a circuit breaker middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	mw, _ := NewWithBreaker(config...)
	return mw
}

// NewWithBreaker creates a circuit breaker middleware and returns both
// the handler and the underlying [Breaker] for programmatic access.
func NewWithBreaker(config ...Config) (celeris.HandlerFunc, *Breaker) {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	brk := &Breaker{
		window:         newSlidingWindow(cfg.WindowSize),
		threshold:      cfg.Threshold,
		minRequests:    cfg.MinRequests,
		cooldownPeriod: int64(cfg.CooldownPeriod),
		halfOpenMax:    int32(cfg.HalfOpenMax),
		isError:        cfg.IsError,
		onStateChange:  cfg.OnStateChange,
	}

	errHandler := cfg.ErrorHandler

	handler := func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		now := time.Now().UnixNano()
		st := State(brk.state.Load())

		switch st {
		case Open:
			if now-brk.openedAt.Load() >= brk.cooldownPeriod {
				brk.mu.Lock()
				// Double-check under lock.
				if State(brk.state.Load()) == Open && now-brk.openedAt.Load() >= brk.cooldownPeriod {
					brk.state.Store(int32(HalfOpen))
					brk.halfOpenUsed.Store(0)
					if brk.onStateChange != nil {
						brk.onStateChange(Open, HalfOpen)
					}
					st = HalfOpen
				} else {
					st = State(brk.state.Load())
				}
				brk.mu.Unlock()
			}
			if st == Open {
				retryAfter := (brk.cooldownPeriod - (now - brk.openedAt.Load())) / int64(time.Second)
				if retryAfter < 1 {
					retryAfter = 1
				}
				c.SetHeader("retry-after", retryAfterString(retryAfter))
				return errHandler(c, ErrServiceUnavailable)
			}
			// Fell through to HalfOpen.
			fallthrough

		case HalfOpen:
			used := brk.halfOpenUsed.Add(1)
			if used > brk.halfOpenMax {
				// Static "1" — avoids strconv.FormatInt allocation
				// on every over-cap HalfOpen request.
				c.SetHeader("retry-after", "1")
				return errHandler(c, ErrServiceUnavailable)
			}

		case Closed:
			// Let through.
		}

		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					// A panic is a failure, with the transition a returned
					// failure makes: a half-open probe that panicked once
					// left the breaker HalfOpen with its slot spent, so
					// every later request got 503 for good (celeris#921).
					brk.settle(true)
					panic(r)
				}
			}()
			err = c.Next()
		}()

		status := responseStatus(c, err)
		brk.settle(brk.isError(err, status))
		return err
	}

	return handler, brk
}

// settle records a let-through request's outcome in the window and makes the
// transition it calls for: Closed trips to Open once the failure rate reaches
// the threshold; a HalfOpen probe moves the breaker to Open on a failure and
// to Closed otherwise.
func (b *Breaker) settle(isFailure bool) {
	if isFailure {
		b.window.recordFailure()
	} else {
		b.window.recordSuccess()
	}

	switch State(b.state.Load()) {
	case Closed:
		total, failures := b.window.counts()
		if total >= int64(b.minRequests) && float64(failures)/float64(total) >= b.threshold {
			b.mu.Lock()
			// Double-check under lock.
			if State(b.state.Load()) == Closed {
				b.state.Store(int32(Open))
				b.openedAt.Store(time.Now().UnixNano())
				b.window.reset()
				if b.onStateChange != nil {
					b.onStateChange(Closed, Open)
				}
			}
			b.mu.Unlock()
		}

	case HalfOpen:
		b.mu.Lock()
		if State(b.state.Load()) == HalfOpen {
			if isFailure {
				b.state.Store(int32(Open))
				b.openedAt.Store(time.Now().UnixNano())
				b.halfOpenUsed.Store(0)
				b.window.reset()
				if b.onStateChange != nil {
					b.onStateChange(HalfOpen, Open)
				}
			} else {
				b.state.Store(int32(Closed))
				b.halfOpenUsed.Store(0)
				b.window.reset()
				if b.onStateChange != nil {
					b.onStateChange(HalfOpen, Closed)
				}
			}
		}
		b.mu.Unlock()
	}
}

// responseStatus derives the HTTP status code from the error or context.
func responseStatus(c *celeris.Context, err error) int {
	if err != nil {
		var he *celeris.HTTPError
		if errors.As(err, &he) {
			return he.Code
		}
		return 500
	}
	if status := c.StatusCode(); status != 0 {
		return status
	}
	return 200
}
