// Package circuitbreaker provides circuit breaker middleware for celeris.
//
// The breaker monitors downstream error rates using a sliding window and
// automatically stops sending requests when failure thresholds are exceeded,
// giving the failing service time to recover. It operates as a three-state
// machine: Closed (normal), Open (rejecting all requests with 503), and
// HalfOpen (allowing limited probe requests to test recovery).
//
// Use [New] to create middleware with default settings (50% threshold,
// minimum 10 requests, 10 s window, 30 s cooldown). Pass a [Config] to
// tune [Config.Threshold], [Config.MinRequests], [Config.WindowSize],
// [Config.CooldownPeriod], [Config.HalfOpenMax], [Config.IsError], and
// [Config.OnStateChange].
//
// Use [NewWithBreaker] to obtain a [*Breaker] reference for programmatic
// state inspection ([Breaker.State]), sliding-window counter export
// ([Breaker.Counts]), and forced reset ([Breaker.Reset]) from health-check
// or admin handlers.
//
// [ErrServiceUnavailable] is the sentinel error (503) returned when the
// breaker is open; use errors.Is to match it in upstream middleware.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-traffic
package circuitbreaker
