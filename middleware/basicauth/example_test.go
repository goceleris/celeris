package basicauth_test

import (
	"github.com/goceleris/celeris"

	"github.com/goceleris/celeris/middleware/basicauth"
)

func ExampleNew() {
	// Simple static credentials with the Users map.
	_ = basicauth.New(basicauth.Config{
		Users: map[string]string{
			"admin": "secret",
			"user":  "password",
		},
	})
}

func ExampleNew_validator() {
	// Custom validator for dynamic credential checking.
	_ = basicauth.New(basicauth.Config{
		Validator: func(user, pass string) bool {
			// Check against a database or external service.
			return user == "admin" && pass == "secret"
		},
	})
}

func ExampleNew_hashedUsers() {
	// PBKDF2-HMAC-SHA256 hashed passwords — avoids storing plaintext in
	// source/config. In practice generate the strings once and paste them
	// into config; HashedUsersFunc defaults to basicauth.VerifyPassword
	// when every hash is pbkdf2-sha256.
	_ = basicauth.New(basicauth.Config{
		HashedUsers: map[string]string{
			"admin": basicauth.HashPasswordPBKDF2("secret"),
			"user":  basicauth.HashPasswordPBKDF2("password"),
		},
	})
}

func ExampleVerifyPassword() {
	// Migrating a store that still holds legacy HashPassword digests:
	// VerifyPassword accepts both formats, so entries can be re-hashed one
	// at a time.
	_ = basicauth.New(basicauth.Config{
		HashedUsers: map[string]string{
			"admin":  basicauth.HashPasswordPBKDF2("secret"),
			"legacy": "2bb80d537b1da3e38bd30361aa855686bde0eacd7162fef6a25fe97bf527a25b", // sha256("secret")
		},
		HashedUsersFunc: basicauth.VerifyPassword,
	})
}

func ExampleNew_contextValidator() {
	// Context-aware validator for per-request auth decisions.
	_ = basicauth.New(basicauth.Config{
		ValidatorWithContext: func(c *celeris.Context, user, _ string) bool {
			tenant := c.Header("x-tenant")
			return tenant == "acme" && user == "admin"
		},
	})
}
