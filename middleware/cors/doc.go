// Package cors provides Cross-Origin Resource Sharing (CORS) middleware
// for celeris.
//
// The middleware handles preflight OPTIONS requests (detected via
// OPTIONS method + Access-Control-Request-Method header) and sets the
// appropriate Access-Control-* response headers. Header values are
// pre-joined at initialization for zero-alloc responses on the hot path.
//
// Allow all origins (default):
//
//	server.Use(cors.New())
//
// Restrict to specific origins with credentials:
//
//	server.Use(cors.New(cors.Config{
//	    AllowOrigins:     []string{"https://example.com"},
//	    AllowCredentials: true,
//	    MaxAge:           3600,
//	}))
//
// # Dynamic Origin Validation
//
// [Config].AllowOriginsFunc validates origins with a custom function.
// [Config].AllowOriginRequestFunc provides the full request context for
// tenant-based or header-dependent origin checks. Neither may be combined
// with a wildcard ("*") AllowOrigins entry.
//
//	server.Use(cors.New(cors.Config{
//	    AllowOriginsFunc: func(origin string) bool {
//	        return strings.HasSuffix(origin, ".example.com")
//	    },
//	}))
//
// # Subdomain Wildcards
//
// Origins may contain a single wildcard for subdomain matching
// (e.g., "https://*.example.com"). Patterns with more than one wildcard
// panic at initialization. By default, the wildcard matches a single
// subdomain level only: "https://*.example.com" matches
// "https://api.example.com" but NOT "https://a.b.example.com". This
// prevents unintended deep-subdomain matching that could weaken origin
// restrictions. The depth limit is enforced by counting dots in the
// wildcard portion (max 0 additional dots for depth 1).
//
// # Cache Poisoning Prevention
//
// When specific origins (not wildcard "*") are configured, the middleware
// adds a Vary: Origin header even on non-CORS requests (those without an
// Origin header). This prevents intermediate caches from serving a
// response generated for one origin to a different origin.
//
// # Header Mirroring
//
// When [Config].MirrorRequestHeaders is true, the middleware mirrors the
// value of the Access-Control-Request-Headers header from the preflight
// request back in Access-Control-Allow-Headers. This means any header
// the client requests will be reflected as allowed without validation.
// When MirrorRequestHeaders is false (the default), the AllowHeaders
// list is used (defaults to Origin, Content-Type, Accept, Authorization).
//
// # Private Network Access
//
// Set [Config].AllowPrivateNetwork to true to enable the Private Network
// Access spec (Access-Control-Allow-Private-Network header on preflight).
//
// # Null Origin Warning
//
// The literal "null" origin is sent by sandboxed iframes, data: URLs,
// file:// pages, and redirected requests. Because all of these share the
// same "null" string, allowing it effectively grants access to any
// sandboxed context. Treat "null" as an opaque origin and avoid adding
// it to AllowOrigins unless you fully understand the implications.
// AllowOriginsFunc is a safer alternative for case-by-case decisions.
//
// Using AllowCredentials with a wildcard origin ("*") panics at
// initialization, as this combination is forbidden by the CORS spec.
package cors
