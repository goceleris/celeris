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
// # Hash formats
//
// [VerifyPassword] accepts only the pbkdf2-sha256 strings
// [HashPasswordPBKDF2] produces (PBKDF2-HMAC-SHA256, random 16-byte salt,
// 600,000 iterations, 32-byte key; stdlib crypto/pbkdf2, no new
// dependencies). v1.6.0 removed the HashPassword helper, which produced an
// unsalted, fast SHA-256 digest (identical passwords collide and the digest
// is brute-forceable at GPU speed), and VerifyPassword no longer accepts
// such digests (celeris#826). Re-hash any with HashPasswordPBKDF2. For
// another credential-grade format — bcrypt, argon2id, scrypt — supply
// [Config].HashedUsersFunc.
//
// Verification costs one PBKDF2 derivation per request (hundreds of
// milliseconds at 600k iterations) for every input, unknown users and
// malformed entries included, so response time does not reveal whether a
// stored hash is well formed; keep a session or token layer in front of hot
// endpoints rather than lowering the count. Stored pbkdf2-sha256 parameters
// are honoured within 600,000–10,000,000 iterations and a salt of at least
// 16 bytes; a value outside that window never verifies and, in an
// auto-wired store, makes [New] panic naming the entry. A HashedUsers map
// with any entry that is not pbkdf2-sha256 and no HashedUsersFunc panics at
// [New], by design.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-auth
package basicauth
