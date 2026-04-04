// Package csrf provides Cross-Site Request Forgery protection middleware
// for celeris using the double-submit cookie pattern, with optional
// server-side token storage for enhanced security.
//
// On safe HTTP methods (GET, HEAD, OPTIONS, TRACE by default) the
// middleware generates or reuses a CSRF token, sets it as a cookie,
// and stores it in the request context. On unsafe methods (POST, PUT,
// DELETE, PATCH) it performs defense-in-depth checks (Sec-Fetch-Site,
// Origin, Referer) then compares the cookie token against the request
// token using constant-time comparison.
//
// Default usage (token in X-CSRF-Token header):
//
//	server.Use(csrf.New())
//
// Custom token lookup from a form field:
//
//	server.Use(csrf.New(csrf.Config{
//	    TokenLookup: "form:_csrf",
//	}))
//
// # Retrieving the Token
//
// Use [TokenFromContext] to read the token in downstream handlers:
//
//	token := csrf.TokenFromContext(c)
//
// # Server-Side Token Storage
//
// For enhanced security, configure [Config].Storage to validate tokens
// against a server-side store (signed double-submit):
//
//	store := csrf.NewMemoryStorage()
//	server.Use(csrf.New(csrf.Config{
//	    Storage:    store,
//	    Expiration: 2 * time.Hour,
//	}))
//
// # Session Cookies
//
// Set [Config].CookieMaxAge to 0 for a browser-session-scoped cookie:
//
//	server.Use(csrf.New(csrf.Config{
//	    CookieMaxAge: 0,
//	}))
//
// # Trusted Origins
//
// [Config].TrustedOrigins allows cross-origin requests from specific
// origins. Wildcard subdomain patterns are supported:
//
//	server.Use(csrf.New(csrf.Config{
//	    TrustedOrigins: []string{
//	        "https://app.example.com",
//	        "https://*.example.com",
//	    },
//	}))
//
// # Sentinel Errors
//
// The package exports sentinel errors ([ErrForbidden], [ErrMissingToken],
// [ErrTokenNotFound], [ErrOriginMismatch], [ErrRefererMissing],
// [ErrRefererMismatch], [ErrSecFetchSite]) for use with errors.Is.
//
// # Security
//
// CookieSecure defaults to false for development convenience. Production
// deployments MUST set CookieSecure: true to prevent cookie transmission
// over unencrypted connections.
package csrf
