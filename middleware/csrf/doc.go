// Package csrf provides Cross-Site Request Forgery protection middleware
// for celeris using the double-submit cookie pattern, with optional
// server-side token storage for enhanced security.
//
// On safe methods (GET, HEAD, OPTIONS, TRACE) the middleware generates or
// reuses a CSRF token, sets it as a cookie, and stores it in the request
// context. On unsafe methods it performs defense-in-depth checks
// (Sec-Fetch-Site, Origin, Referer) then compares the cookie token against
// the submitted token using constant-time comparison.
//
// Key exported symbols:
//   - [New] — constructs the middleware; accepts an optional [Config].
//   - [Config] — controls token lookup (header/form/query), cookie attributes,
//     trusted origins, server-side [Config.Storage], and [Config.SingleUseToken].
//   - [TokenFromContext] — retrieves the current CSRF token from a handler.
//   - [HandlerFromContext] — returns the [Handler] for the active middleware
//     instance; use [Handler.DeleteToken] on logout.
//   - [DeleteToken] — package-level convenience wrapper for the above.
//   - Sentinel errors ([ErrForbidden], [ErrMissingToken], [ErrTokenNotFound],
//     [ErrOriginMismatch], [ErrRefererMissing], [ErrRefererMismatch],
//     [ErrSecFetchSite]) for use with errors.Is.
//
// Production note: [Config.CookieSecure] defaults to false for development
// convenience; set it to true in production. [Config.CookieHTTPOnly] is
// always enforced as true regardless of the supplied value. When using the
// methodoverride middleware, do not add PUT, DELETE, or PATCH to
// [Config.SafeMethods] — doing so would allow bypass via POST tunneling.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-security
package csrf
