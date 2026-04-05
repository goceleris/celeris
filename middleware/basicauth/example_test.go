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
	// SHA-256 hashed passwords — avoids storing plaintext in source/config.
	_ = basicauth.New(basicauth.Config{
		HashedUsers: map[string]string{
			"admin": basicauth.HashPassword("secret"),
			"user":  basicauth.HashPassword("password"),
		},
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
