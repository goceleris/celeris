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
//
// CookieHTTPOnly is always enforced as true regardless of the user-supplied
// Config value. This prevents client-side JavaScript from reading the CSRF
// cookie, which is a defense-in-depth measure against XSS token theft.
//
// # Method Override Interaction
//
// When using the methodoverride middleware (registered via Server.Pre()),
// the HTTP method is rewritten before CSRF validation. A POST request with
// X-HTTP-Method-Override: PUT becomes a PUT by the time CSRF runs.
//
// IMPORTANT: Do not add PUT, DELETE, or PATCH to SafeMethods if you use
// method override. Doing so would allow form submissions to bypass CSRF
// token validation by tunneling through POST → PUT/DELETE/PATCH.
//
// The default SafeMethods (GET, HEAD, OPTIONS, TRACE) are safe because
// methodoverride only overrides POST requests and its default AllowedMethods
// (PUT, DELETE, PATCH) are not in the safe set.
package csrf
