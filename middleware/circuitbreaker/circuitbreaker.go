package circuitbreaker

import (
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris"
)

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
	validate(cfg)

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
				c.SetHeader("retry-after", strconv.FormatInt(retryAfter, 10))
				return errHandler(c, ErrServiceUnavailable)
			}
			// Fell through to HalfOpen.
			fallthrough

		case HalfOpen:
			used := brk.halfOpenUsed.Add(1)
			if used > brk.halfOpenMax {
				retryAfter := int64(1)
				c.SetHeader("retry-after", strconv.FormatInt(retryAfter, 10))
				return errHandler(c, ErrServiceUnavailable)
			}

		case Closed:
			// Let through.
		}

		err := c.Next()

		status := responseStatus(c, err)
		isFailure := brk.isError(err, status)

		if isFailure {
			brk.window.recordFailure()
		} else {
			brk.window.recordSuccess()
		}

		currentState := State(brk.state.Load())
		switch currentState {
		case Closed:
			total, failures := brk.window.counts()
			if total >= int64(brk.minRequests) && float64(failures)/float64(total) >= brk.threshold {
				brk.mu.Lock()
				// Double-check under lock.
				if State(brk.state.Load()) == Closed {
					brk.state.Store(int32(Open))
					brk.openedAt.Store(time.Now().UnixNano())
					brk.window.reset()
					if brk.onStateChange != nil {
						brk.onStateChange(Closed, Open)
					}
				}
				brk.mu.Unlock()
			}

		case HalfOpen:
			brk.mu.Lock()
			if State(brk.state.Load()) == HalfOpen {
				if isFailure {
					brk.state.Store(int32(Open))
					brk.openedAt.Store(time.Now().UnixNano())
					brk.halfOpenUsed.Store(0)
					brk.window.reset()
					if brk.onStateChange != nil {
						brk.onStateChange(HalfOpen, Open)
					}
				} else {
					brk.state.Store(int32(Closed))
					brk.halfOpenUsed.Store(0)
					brk.window.reset()
					if brk.onStateChange != nil {
						brk.onStateChange(HalfOpen, Closed)
					}
				}
			}
			brk.mu.Unlock()
		}

		return err
	}

	return handler, brk
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
