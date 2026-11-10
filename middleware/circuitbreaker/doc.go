// Package circuitbreaker provides circuit breaker middleware for celeris.
//
// The breaker monitors downstream error rates using a sliding window and
// automatically stops sending requests when failure thresholds are exceeded,
// giving the failing service time to recover. It operates as a three-state
// machine: Closed (normal), Open (rejecting all requests with 503), and
// HalfOpen (allowing limited probe requests to test recovery). A handler
// that panics is a failure, with the same transition as a returned one (a
// half-open probe that panics reopens the breaker); the panic continues to
// the recovery middleware.
//
// A request no route matched (the router answers it with a 404, a 405 or the
// automatic OPTIONS answer, and runs the middleware mounted with Server.Use
// on the way) reaches no downstream: it is passed on untouched, neither
// refused while the breaker is Open nor counted, so a stray request for
// /favicon.ico cannot close a HalfOpen breaker. Mount the breaker on the
// routes (or a group) it protects, or with Server.Use, as before.
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
