// Package secure provides OWASP security headers middleware for celeris.
//
// The middleware sets a comprehensive suite of HTTP security headers on
// every response. All non-empty header values are pre-computed into a flat
// slice at initialization. The hot path iterates this slice with zero
// allocations.
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
//   - Cross-Origin-Embedder-Policy: "require-corp"
//   - X-DNS-Prefetch-Control: "off"
//   - X-Permitted-Cross-Domain-Policies: "none"
//   - Origin-Agent-Cluster: "?1"
//   - X-Download-Options: "noopen"
//
// Content-Security-Policy and Permissions-Policy are only included when
// their respective [Config] fields are non-empty.
//
// # Usage
//
// Default configuration (all OWASP-recommended headers):
//
//	server.Use(secure.New())
//
// Custom configuration:
//
//	server.Use(secure.New(secure.Config{
//	    ContentSecurityPolicy: "default-src 'self'",
//	    HSTSMaxAge:            31536000,
//	    HSTSPreload:           true,
//	}))
//
// # Suppressing Individual Headers
//
// Set any string field to [Suppress] ("-") to omit that header:
//
//	server.Use(secure.New(secure.Config{
//	    XFrameOptions: secure.Suppress,
//	}))
//
// For HSTS, set [Config].DisableHSTS to true to omit the header entirely.
// HSTSMaxAge defaults to 2 years (63072000 seconds) whether or not other
// fields are customized — there is no zero-value trap.
//
// # HSTS Preload Validation
//
// When [Config].HSTSPreload is true, validate() panics at initialization
// if HSTSMaxAge < 31536000 or HSTSExcludeSubdomains is true, enforcing
// HSTS preload list requirements.
//
// # Skipping
//
// Use [Config].Skip for dynamic skip logic or [Config].SkipPaths for
// exact-match path exclusions. Skipped requests receive no security
// headers at all.
//
// # CORS Interaction
//
// When using CORS middleware alongside secure, note that the default
// CrossOriginEmbedderPolicy (require-corp) and CrossOriginResourcePolicy
// (same-origin) block cross-origin resource loading. APIs serving
// cross-origin requests should set CrossOriginResourcePolicy: "cross-origin"
// and CrossOriginEmbedderPolicy: [Suppress].
package secure
