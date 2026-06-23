// Package keyauth provides API key authentication middleware for celeris.
//
// The middleware extracts an API key from a configurable source (header,
// query parameter, cookie, form field, or URL parameter), validates it via
// a user-supplied [Config].Validator function, and stores the authenticated
// key in the context store under [ContextKey] ("keyauth_key"). Failed
// authentication returns 401 with a WWW-Authenticate header.
//
// Key exported symbols:
//   - [New] — constructs the middleware handler from a [Config].
//   - [Config] — controls key lookup, validation, skip rules, realm, auth
//     scheme, RFC 6750 challenge parameters, and success/error callbacks.
//   - [StaticKeys] — constant-time validator for a fixed set of API keys.
//   - [KeyFromContext] — retrieves the validated key from a request context.
//   - [ErrMissingKey], [ErrUnauthorized] — sentinel 401 errors usable with
//     errors.Is.
//
// [Config].Validator is required; omitting it panics at initialization.
// [Config].KeyLookup supports comma-separated fallback sources, e.g.
// "header:Authorization:Bearer ,query:api_key". Set [Config].Skip or
// [Config].SkipPaths to bypass the middleware selectively; OPTIONS requests
// are always skipped to allow CORS preflight through.
//
// Security note: keys extracted from query strings appear in access logs,
// browser history, and Referer headers. Prefer header-based lookup for
// sensitive environments.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-auth
package keyauth
