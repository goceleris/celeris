// Package keyauth provides API key authentication middleware for celeris.
//
// The middleware extracts an API key from a configurable source (header,
// query parameter, cookie, form field, or URL parameter), validates it via
// a user-supplied function, and stores the authenticated key in the context
// store under [ContextKey] ("keyauth_key"). Failed authentication returns
// 401 with a WWW-Authenticate header.
//
// [Config].Validator is required; omitting it panics at initialization.
//
// Simple usage with static keys (constant-time comparison):
//
//	server.Use(keyauth.New(keyauth.Config{
//	    Validator: keyauth.StaticKeys("key-1", "key-2"),
//	}))
//
// Custom validator with context access:
//
//	server.Use(keyauth.New(keyauth.Config{
//	    Validator: func(c *celeris.Context, key string) (bool, error) {
//	        return checkDB(c, key)
//	    },
//	    KeyLookup: "query:api_key",
//	}))
//
// Multiple sources can be comma-separated for fallback:
//
//	KeyLookup: "header:Authorization:Bearer ,query:api_key"
//
// Use [KeyFromContext] to retrieve the authenticated key downstream.
//
// [Config].ChallengeParams controls RFC 6750 parameters in the
// WWW-Authenticate header (error, error_description, error_uri, scope).
//
// [StaticKeys] uses constant-time comparison via [crypto/subtle] to
// prevent timing side-channels.
//
// [ErrUnauthorized] and [ErrMissingKey] are exported sentinel errors
// (both 401) usable with errors.Is for upstream error handling.
//
// Set [Config].Skip to bypass the middleware dynamically, or
// [Config].SkipPaths for exact-match path exclusions.
//
// Security: API keys in query strings (`query:api_key`) are logged in
// access logs, browser history, and Referer headers. Prefer header-based
// key lookup for sensitive environments.
package keyauth
