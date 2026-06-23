// Package healthcheck provides Kubernetes-style health probe middleware
// for celeris.
//
// The middleware intercepts GET/HEAD requests matching configurable probe
// paths (default "/livez", "/readyz", "/startupz") and returns a JSON
// status response. Non-matching requests pass through with zero overhead.
//
// Key exported symbols: [New] constructs the handler; [Config] controls probe
// paths, per-probe [Checker] functions, the [Config].Skip bypass predicate,
// and [Config].CheckerTimeout (default [DefaultCheckerTimeout], 5 s). Set
// [Config].CheckerTimeout to [FastPathTimeout] for trivial checkers that
// cannot block (runs inline, no goroutine overhead). Panicking checkers are
// recovered and return 503.
//
// Setting a probe path to "" disables that probe. Invalid paths (missing
// leading '/') or overlapping enabled paths cause a panic at initialization.
//
// Response format: 200 {"status":"ok"} / 503 {"status":"unavailable"}.
// HEAD requests return the status code with no body.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware
package healthcheck
