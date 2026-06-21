// Package basicauth provides HTTP Basic Authentication middleware for
// celeris.
//
// The middleware parses the Authorization header via [celeris.Context.BasicAuth],
// validates credentials via a user-supplied function, and stores the
// authenticated username in the context store under [UsernameKey]. Failed
// authentication returns 401 with WWW-Authenticate, Cache-Control, and Vary
// headers.
//
// Exactly one credential source is required; [New] panics otherwise:
//   - [Config].Users — plaintext map, auto-generates a constant-time HMAC validator.
//   - [Config].HashedUsers + [Config].HashedUsersFunc — opaque hash strings with
//     a caller-supplied compare function (bcrypt, argon2id, scrypt, etc.).
//     HashedUsersFunc is mandatory when HashedUsers is set.
//   - [Config].Validator — arbitrary func(user, pass string) bool.
//   - [Config].ValidatorWithContext — same, with request context access.
//
// Minimal usage with a Users map:
//
//	server.Use(basicauth.New(basicauth.Config{
//	    Users: map[string]string{
//	        "admin": "secret",
//	    },
//	}))
//
// Use [UsernameFromContext] to retrieve the authenticated username downstream.
// Set [Config].Skip or [Config].SkipPaths to bypass the middleware selectively.
//
// Note: [HashPassword] (SHA-256) is deprecated and credential-grade only with a
// modern KDF. Use bcrypt or argon2 via [Config].HashedUsersFunc instead.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-auth
package basicauth
