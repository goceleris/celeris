package circuitbreaker

import (
	"time"

	"github.com/goceleris/celeris"
)

// State represents the circuit breaker state.
type State int

const (
	// Closed allows all requests through and monitors the error rate.
	Closed State = iota
	// Open rejects all requests immediately with 503.
	Open
	// HalfOpen allows a limited number of probe requests through.
	HalfOpen
)

// String returns the human-readable state name.
func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Config defines the circuit breaker middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string

	// Threshold is the failure ratio (failures/total) that trips the breaker.
	// Must be between 0 and 1 (exclusive). Default: 0.5.
	Threshold float64

	// MinRequests is the minimum number of requests in the current window
	// before the breaker can trip. Prevents premature tripping on small
	// sample sizes. Default: 10.
	MinRequests int

	// WindowSize is the duration of the sliding observation window.
	// Default: 10s.
	WindowSize time.Duration

	// CooldownPeriod is how long the breaker stays open before transitioning
	// to half-open. Default: 30s.
	CooldownPeriod time.Duration

	// HalfOpenMax is the maximum number of probe requests allowed through
	// in the half-open state. Default: 1.
	HalfOpenMax int

	// IsError determines whether a response should be counted as a failure.
	// Receives the error returned by the handler and the response status code.
	// Default: status >= 500.
	IsError func(err error, statusCode int) bool

	// OnStateChange is called when the breaker transitions between states.
	// Called under a mutex; keep the callback fast.
	OnStateChange func(from, to State)

	// ErrorHandler is called to produce a response when the breaker is open.
	// Default: returns ErrServiceUnavailable (503).
	ErrorHandler func(c *celeris.Context, err error) error
}

// ErrServiceUnavailable is returned when the circuit breaker is open.
var ErrServiceUnavailable = celeris.NewHTTPError(503, "Service Unavailable")

var defaultConfig = Config{
	Threshold:      0.5,
	MinRequests:    10,
	WindowSize:     10 * time.Second,
	CooldownPeriod: 30 * time.Second,
	HalfOpenMax:    1,
}

func applyDefaults(cfg Config) Config {
	if cfg.Threshold == 0 {
		cfg.Threshold = defaultConfig.Threshold
	}
	if cfg.MinRequests == 0 {
		cfg.MinRequests = defaultConfig.MinRequests
	}
	if cfg.WindowSize == 0 {
		cfg.WindowSize = defaultConfig.WindowSize
	}
	if cfg.CooldownPeriod == 0 {
		cfg.CooldownPeriod = defaultConfig.CooldownPeriod
	}
	if cfg.HalfOpenMax == 0 {
		cfg.HalfOpenMax = defaultConfig.HalfOpenMax
	}
	if cfg.IsError == nil {
		cfg.IsError = func(_ error, statusCode int) bool {
			return statusCode >= 500
		}
	}
	if cfg.ErrorHandler == nil {
		cfg.ErrorHandler = func(_ *celeris.Context, _ error) error {
			return ErrServiceUnavailable
		}
	}
	return cfg
}

func validate(cfg Config) {
	if cfg.Threshold <= 0 || cfg.Threshold > 1 {
		panic("circuitbreaker: Threshold must be in (0, 1]")
	}
}
