// Package secure provides OWASP security headers middleware for celeris.
//
// The middleware sets a comprehensive suite of HTTP security headers on every
// response. All non-empty header values are pre-computed into a flat slice at
// initialization; the hot path iterates this slice with zero allocations.
//
// Default headers set on every response:
//
//   - X-Content-Type-Options: "nosniff"
//   - X-Frame-Options: "SAMEORIGIN"
//   - X-XSS-Protection: "0" (disables legacy XSS auditor)
//   - Strict-Transport-Security (HTTPS only, 2-year max-age, includeSubDomains)
//   - Referrer-Policy: "strict-origin-when-cross-origin"
//   - Cross-Origin-Opener-Policy: "same-origin"
//   - Cross-Origin-Resource-Policy: "same-origin"
//   - X-DNS-Prefetch-Control: "off"
//   - X-Permitted-Cross-Domain-Policies: "none"
//   - Origin-Agent-Cluster: "?1"
//
// Cross-Origin-Embedder-Policy and X-Download-Options are opt-in (off by
// default). Content-Security-Policy and Permissions-Policy are only emitted
// when their [Config] fields are non-empty.
//
// Use [New] to create the middleware. Pass a [Config] to override defaults.
// Set any string field to [Suppress] ("-") to omit that individual header.
// Use [Config].DisableHSTS to omit Strict-Transport-Security entirely.
// Use [Config].Skip or [Config].SkipPaths to bypass the middleware per request.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-security
package secure
