// Package circuitbreaker provides circuit breaker middleware for celeris.
//
// The circuit breaker monitors downstream error rates using a sliding window
// and automatically stops sending requests when failure thresholds are exceeded,
// giving the failing service time to recover.
//
// # Three-State Machine
//
// The breaker operates in three states:
//
//   - Closed: All requests pass through. The sliding window tracks successes
//     and failures. When the failure ratio meets or exceeds [Config].Threshold
//     (and at least [Config].MinRequests have been observed), the breaker
//     trips to Open.
//
//   - Open: All requests are immediately rejected with 503 Service Unavailable.
//     A Retry-After header is set with the remaining cooldown seconds. After
//     [Config].CooldownPeriod elapses, the breaker transitions to HalfOpen.
//
//   - HalfOpen: Up to [Config].HalfOpenMax probe requests are allowed through.
//     If a probe succeeds, the breaker returns to Closed. If a probe fails,
//     the breaker returns to Open. Excess requests beyond HalfOpenMax are
//     rejected with 503.
//
// # Sliding Window Algorithm
//
// The observation window is divided into 10 time buckets spanning
// [Config].WindowSize. Each bucket tracks successes and failures using atomic
// counters. Expired buckets (older than WindowSize) are excluded from the
// error rate calculation. This provides a smooth, time-decaying view of
// the failure rate without requiring a global lock on the hot path.
//
// # Response Classification
//
// By default, responses with status >= 500 are counted as failures. Use
// [Config].IsError to customize classification (e.g., treat 429 as a failure,
// or ignore certain 5xx codes).
//
// # Retry-After Header
//
// When the breaker is open, the middleware sets a Retry-After header with
// the number of seconds until the cooldown period expires. Clients that
// respect this header avoid unnecessary retries during the cooldown.
//
// # Programmatic Access
//
// Use [NewWithBreaker] to obtain a reference to the underlying [Breaker]
// struct. This allows programmatic state inspection ([Breaker.State]),
// window counter export ([Breaker.Counts]) for dashboards and Prometheus,
// and forced reset ([Breaker.Reset]) for health checks, admin endpoints,
// or integration tests.
//
// # Middleware Ordering
//
// Place the circuit breaker after rate limiting and before timeout middleware:
//
//	server.Use(ratelimit.New())
//	server.Use(circuitbreaker.New())
//	server.Use(timeout.New())
//
// This ensures rate-limited requests are rejected before reaching the breaker,
// and timed-out requests are properly classified by the breaker.
//
// # In-Flight Requests During Transition
//
// Requests that are already executing when the breaker transitions to Open
// continue to completion — only NEW requests are rejected. This is by design:
// interrupting in-flight requests could cause data corruption or incomplete
// operations.
//
// # Per-Endpoint Breakers
//
// To use separate breakers for different services or route groups, create
// multiple instances and apply them to the appropriate groups:
//
//	payments := s.Group("/api/payments")
//	payments.Use(circuitbreaker.New(circuitbreaker.Config{Threshold: 0.3}))
//
// # Thread Safety
//
// The breaker is safe for concurrent use. State reads and counter updates
// use atomic operations (lock-free hot path). State transitions acquire a
// mutex with double-check locking to prevent duplicate transitions.
//
// Basic usage with defaults (50% threshold, minimum 10 requests, 10s window):
//
//	server.Use(circuitbreaker.New())
//
// Custom threshold and cooldown:
//
//	server.Use(circuitbreaker.New(circuitbreaker.Config{
//	    Threshold:      0.3,
//	    MinRequests:    20,
//	    CooldownPeriod: time.Minute,
//	}))
//
// Programmatic access for health checks:
//
//	mw, breaker := circuitbreaker.NewWithBreaker()
//	server.Use(mw)
//	server.GET("/health", func(c *celeris.Context) error {
//	    if breaker.State() == circuitbreaker.Open {
//	        return c.JSON(503, map[string]string{"circuit": "open"})
//	    }
//	    return c.JSON(200, map[string]string{"circuit": "closed"})
//	})
//
// [ErrServiceUnavailable] is the exported sentinel error (503) returned when
// the breaker is open, usable with errors.Is for error handling in upstream
// middleware.
package circuitbreaker
