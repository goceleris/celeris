// Package middleware provides production-ready middleware for celeris.
//
// # Pre-Routing Middleware (Server.Pre)
//
// These run before the router matches the request and can mutate the
// request method, path, scheme, host, or client IP:
//
//	proxy          — extract real client IP/scheme/host from trusted proxy headers
//	redirect       — HTTPS, www, trailing-slash URL normalization
//	methodoverride — override POST method via _method form field or header
//
// # Recommended Middleware Ordering (Server.Use)
//
// Install middleware in this order so each layer sees the right context:
//
//	healthcheck — health probes bypass all middleware; respond immediately
//	requestid   — assign request ID first; all downstream logs include it
//	metrics/otel — Prometheus / OpenTelemetry: can also go after logger
//	logger      — log every request with the ID from requestid
//	recovery    — catch panics from everything below; logger records the 500
//	secure      — set OWASP security headers before any response can escape
//	cors        — handle preflight; must run before auth rejects OPTIONS
//	bodylimit   — reject oversized bodies before parsing begins
//	ratelimit   — shed load before expensive auth/business logic
//	[auth]      — jwt / keyauth / basicauth (see Auth Stacking below)
//	csrf        — validate CSRF token after authentication is established
//	session     — load session (may depend on authenticated user)
//	debug       — intercepts by path prefix (e.g. /debug/); can go anywhere
//	timeout     — bound handler execution; innermost wrapper before the route
//	compress    — response compression; wraps etag (computes on uncompressed body)
//	etag        — conditional responses (304 Not Modified); innermost transform
//	[handler]   — route handler
//
// # Measurement System Roles
//
// Three complementary systems serve different operational needs:
//
// Core Collector (github.com/goceleris/celeris/observe) — Built into the
// framework. Tracks total requests, error counts, and latency percentiles
// including unmatched routes and panic recoveries. Zero external dependencies.
// Use for lightweight internal diagnostics and health checks.
//
// Prometheus middleware (github.com/goceleris/celeris/middleware/metrics) — Production
// metrics pipeline. Emits per-path, per-method, per-status histograms and
// counters to Prometheus. Integrates with Grafana dashboards and alerting.
// Use for production monitoring and SLO tracking.
//
// OpenTelemetry middleware (github.com/goceleris/celeris/middleware/otel) — Distributed
// tracing and metrics. Creates spans per request with W3C trace context
// propagation. Exports to any OTLP-compatible backend (Jaeger, Tempo, etc.).
// Use for cross-service request correlation and latency breakdown.
//
// # Auth Stacking Pattern
//
// JWT and keyauth both support ContinueOnIgnoredError, which calls c.Next()
// when ErrorHandler returns nil (i.e., the error was intentionally ignored).
// Chain them for JWT-preferred authentication with API key fallback:
//
//	jwtAuth := jwt.New(jwt.Config{
//	    SigningKey:             hmacSecret,
//	    ContinueOnIgnoredError: true,
//	    ErrorHandler: func(c *celeris.Context, err error) error {
//	        return nil // ignore JWT failure, let keyauth try
//	    },
//	})
//	keyAuth := keyauth.New(keyauth.Config{
//	    Validator: func(c *celeris.Context, key string) (bool, error) {
//	        return key == apiKey, nil
//	    },
//	})
//	api := s.Group("/api", jwtAuth, keyAuth)
//
// Requests with a valid JWT proceed after jwtAuth. Requests without a JWT
// (or with an invalid one) fall through to keyAuth. If neither succeeds,
// keyAuth returns 401.
//
// # Vary Header Convention
//
// Several middleware set the Vary response header:
//
//   - cors: Vary: Origin
//   - compress: Vary: Accept-Encoding
//
// All middleware use AddHeader (not SetHeader) for Vary to preserve values
// set by other middleware. Handlers that need to set Vary MUST also use
// AddHeader to avoid clobbering middleware-set values:
//
//	c.AddHeader("vary", "Accept-Language")  // correct
//	c.SetHeader("vary", "Accept-Language")  // WRONG: clobbers cors/compress Vary
package middleware
