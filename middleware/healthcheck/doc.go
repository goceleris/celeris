// Package healthcheck provides Kubernetes-style health probe middleware
// for celeris.
//
// The middleware intercepts GET/HEAD requests matching configurable probe
// paths (default "/livez", "/readyz", "/startupz") and returns a JSON
// status response. Non-matching requests pass through with zero overhead.
//
// Basic usage (all probes return 200):
//
//	server.Use(healthcheck.New())
//
// Custom readiness check:
//
//	server.Use(healthcheck.New(healthcheck.Config{
//	    ReadyChecker: func(_ *celeris.Context) bool {
//	        return db.Ping() == nil
//	    },
//	}))
//
// Each checker is guarded by [Config].CheckerTimeout (default 5s); if it
// does not return in time the probe responds 503. Set CheckerTimeout to
// [FastPathTimeout] for trivial checkers that cannot block (runs inline,
// no goroutine overhead). Panicking checkers are recovered and return 503.
//
// Setting a probe path to "" disables that probe. [Config].Skip bypasses
// the middleware before path matching.
//
// [Config].validate() panics on invalid paths (missing leading '/') or
// overlapping enabled paths. These are programming errors caught at init.
//
// Response format: 200 {"status":"ok"} / 503 {"status":"unavailable"}.
// HEAD requests return the status code with no body.
package healthcheck
