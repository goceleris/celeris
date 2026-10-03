package basicauth_test

import (
	"fmt"

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
	// VerifyPassword checks a password against a HashPasswordPBKDF2 string
	// in constant time. New wires it in when every HashedUsers entry is
	// pbkdf2-sha256; call it directly to check a credential outside the
	// middleware.
	stored := basicauth.HashPasswordPBKDF2("secret")
	fmt.Println(basicauth.VerifyPassword(stored, "secret"))
	fmt.Println(basicauth.VerifyPassword(stored, "wrong"))
	// Output:
	// true
	// false
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
