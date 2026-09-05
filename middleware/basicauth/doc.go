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
//     a compare function: the built-in [VerifyPassword], or a caller-supplied
//     one (bcrypt, argon2id, scrypt, etc.). HashedUsersFunc may be omitted
//     only when every hash was produced by [HashPasswordPBKDF2].
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
// Hashed credentials without a third-party KDF dependency:
//
//	// Generate once (e.g. `go run` a tiny tool) and paste the string into
//	// config; every call yields a different salt.
//	hash := basicauth.HashPasswordPBKDF2("secret")
//	// -> pbkdf2-sha256$600000$<salt-b64>$<hash-b64>
//
//	server.Use(basicauth.New(basicauth.Config{
//	    HashedUsers: map[string]string{"admin": hash},
//	    // HashedUsersFunc defaults to basicauth.VerifyPassword when every
//	    // hash is pbkdf2-sha256.
//	}))
//
// Use [UsernameFromContext] to retrieve the authenticated username downstream.
// Set [Config].Skip or [Config].SkipPaths to bypass the middleware selectively.
//
// # Migrating from HashPassword (SHA-256)
//
// [HashPassword] is deprecated: it produces an unsalted, fast SHA-256 digest,
// which is not a credential-storage hash (identical passwords collide and
// the digest is brute-forceable at GPU speed). Nothing breaks for existing
// deployments — HashPassword's output is unchanged and any HashedUsersFunc
// you already supply keeps working — but new hashes should come from
// [HashPasswordPBKDF2] (PBKDF2-HMAC-SHA256, random 16-byte salt, 600,000
// iterations, 32-byte key; stdlib crypto/pbkdf2, no new dependencies).
//
// To migrate an existing HashedUsers store incrementally:
//
//  1. Set HashedUsersFunc to [VerifyPassword]. It detects the format of each
//     stored hash — "pbkdf2-sha256$..." or a bare hex SHA-256 digest — and
//     compares with crypto/subtle.ConstantTimeCompare either way, so mixed
//     stores authenticate correctly.
//  2. Re-hash each user with HashPasswordPBKDF2 (at the next password
//     change, or in one sweep if you hold the plaintexts) and replace the
//     stored value.
//  3. Once no legacy digests remain, drop the explicit HashedUsersFunc — the
//     default kicks in for all-PBKDF2 stores. Mixed or legacy-only stores
//     without a HashedUsersFunc still panic at [New], by design.
//
// Verification costs one PBKDF2 derivation per request (hundreds of
// milliseconds at 600k iterations); keep a session or token layer in front
// of hot endpoints rather than lowering the count.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-auth
package basicauth
