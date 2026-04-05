// Package basicauth provides HTTP Basic Authentication middleware for
// celeris.
//
// The middleware parses the Authorization header via the framework's
// [celeris.Context.BasicAuth] method, validates credentials via a
// user-supplied function, and stores the authenticated username in the
// context store under [UsernameKey]. Failed authentication returns 401
// with a WWW-Authenticate header.
//
// One of [Config].Validator, [Config].ValidatorWithContext, [Config].Users,
// or [Config].HashedUsers is required; omitting all four panics at
// initialization.
//
// Simple usage with a Users map (auto-generates a constant-time validator):
//
//	server.Use(basicauth.New(basicauth.Config{
//	    Users: map[string]string{
//	        "admin": "secret",
//	        "user":  "pass",
//	    },
//	}))
//
// Hashed passwords with SHA-256 (avoids storing plaintext):
//
//	server.Use(basicauth.New(basicauth.Config{
//	    HashedUsers: map[string]string{
//	        "admin": basicauth.HashPassword("secret"),
//	    },
//	}))
//
// WARNING: SHA-256 is a fast hash with no work factor. For high-security
// password storage, use bcrypt, scrypt, or Argon2 via a custom
// [Config].Validator or [Config].HashedUsersFunc.
//
// Plugging in bcrypt via HashedUsersFunc:
//
//	server.Use(basicauth.New(basicauth.Config{
//	    HashedUsers: map[string]string{
//	        "admin": "$2a$10$...", // bcrypt hash
//	    },
//	    HashedUsersFunc: func(hash, password string) bool {
//	        return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
//	    },
//	}))
//
// # Retrieving the Username
//
// Use [UsernameFromContext] to retrieve the authenticated username from
// downstream handlers:
//
//	name := basicauth.UsernameFromContext(c)
//
// # Skipping
//
// Set [Config].Skip to bypass the middleware dynamically, or
// [Config].SkipPaths for exact-match path exclusions.
package basicauth
