package timeout

import (
	"time"

	"github.com/goceleris/celeris"
)

// Config defines the timeout middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// Timeout is the request timeout duration. Default: 5s.
	// Used as the fallback when TimeoutFunc is nil or returns zero.
	Timeout time.Duration

	// TimeoutFunc computes a per-request timeout duration. When non-nil,
	// its return value is used instead of the static Timeout. If it
	// returns zero or a negative duration, the static Timeout is used
	// as a fallback.
	//
	// TimeoutFunc is called before BufferResponse in preemptive mode,
	// so it runs on the request goroutine (not the handler goroutine).
	TimeoutFunc func(c *celeris.Context) time.Duration

	// ErrorHandler handles timeout errors. The err parameter is
	// context.DeadlineExceeded for a deadline timeout, the matched
	// TimeoutErrors entry for a semantic timeout, or a panic-wrapped error
	// for a recovered panic. Default: returns 503 Service Unavailable.
	ErrorHandler func(c *celeris.Context, err error) error

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string

	// Preemptive enables preemptive timeout mode. When true, the handler
	// runs in a goroutine and the response is buffered. If the handler
	// does not complete within the timeout, the middleware waits for the
	// goroutine to finish, discards the buffered response, and returns
	// the error handler result. Handlers MUST respect context cancellation
	// (select on c.Context().Done()) to avoid blocking the response.
	//
	// Preemptive mode is incompatible with StreamWriter: buffered mode
	// captures the full response in memory, defeating streaming and
	// potentially causing OOM on large payloads. Use non-preemptive
	// mode (the default) for streaming endpoints.
	//
	// Cost: preemptive mode allocates a goroutine and a context.WithTimeout
	// per request — measurable at very high RPS (~1-3% throughput on
	// the reference benchmark suite). Reach for it only when you
	// genuinely need to interrupt CPU-bound or blocking handlers; the
	// non-preemptive default uses a single context.WithTimeout and lets
	// handlers cooperatively check c.Context().Done().
	Preemptive bool

	// TimeoutErrors lists errors that should be treated as timeouts even
	// if the deadline has not been reached. When the handler returns an
	// error matching any entry via errors.Is, the ErrorHandler is invoked
	// as though the request timed out. This is useful for treating
	// upstream timeout errors (e.g. database query timeout) as request
	// timeouts.
	TimeoutErrors []error
}

var defaultConfig = Config{
	Timeout: 5 * time.Second,
}

// ErrServiceUnavailable is returned when the request timeout is exceeded.
var ErrServiceUnavailable = celeris.NewHTTPError(503, "Service Unavailable")

func defaultErrorHandler(_ *celeris.Context, _ error) error {
	return ErrServiceUnavailable
}

func applyDefaults(cfg Config) Config {
	if cfg.TimeoutFunc == nil && cfg.Timeout <= 0 {
		panic("timeout: Timeout must be positive or TimeoutFunc must be set")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultConfig.Timeout
	}
	if cfg.ErrorHandler == nil {
		cfg.ErrorHandler = defaultErrorHandler
	}
	return cfg
}
