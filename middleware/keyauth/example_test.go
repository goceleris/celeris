package keyauth_test

import (
	"github.com/goceleris/celeris"

	"github.com/goceleris/celeris/middleware/keyauth"
)

func ExampleNew() {
	// Static API keys with constant-time comparison.
	_ = keyauth.New(keyauth.Config{
		Validator: keyauth.StaticKeys("key-1", "key-2"),
	})
}

func ExampleNew_queryParam() {
	// Extract key from query parameter instead of header.
	_ = keyauth.New(keyauth.Config{
		KeyLookup: "query:api_key",
		Validator: keyauth.StaticKeys("my-secret-key"),
	})
}

func ExampleNew_formField() {
	// Extract key from a form field (URL-encoded or multipart).
	_ = keyauth.New(keyauth.Config{
		KeyLookup: "form:api_key",
		Validator: keyauth.StaticKeys("form-secret"),
	})
}

func ExampleNew_urlParam() {
	// Extract key from a URL path parameter (e.g., /api/:token/data).
	_ = keyauth.New(keyauth.Config{
		KeyLookup: "param:token",
		Validator: keyauth.StaticKeys("param-secret"),
	})
}

func ExampleNew_customValidator() {
	// Custom validator with context access.
	_ = keyauth.New(keyauth.Config{
		Validator: func(c *celeris.Context, key string) (bool, error) {
			tenant := c.Header("x-tenant")
			_ = tenant // look up key in tenant-specific store
			return key == "expected", nil
		},
	})
}

func ExampleNew_cookie() {
	// Extract key from a cookie.
	_ = keyauth.New(keyauth.Config{
		KeyLookup: "cookie:auth_token",
		Validator: keyauth.StaticKeys("token-value"),
	})
}

func ExampleNew_successHandler() {
	// Enrich context after successful auth.
	_ = keyauth.New(keyauth.Config{
		Validator: keyauth.StaticKeys("admin-key"),
		SuccessHandler: func(c *celeris.Context) {
			c.Set("role", "admin")
		},
	})
}

func ExampleNew_wwwAuthenticate() {
	// Custom WWW-Authenticate header with Bearer scheme.
	_ = keyauth.New(keyauth.Config{
		Validator:  keyauth.StaticKeys("secret"),
		AuthScheme: "Bearer",
		Realm:      "MyAPI",
	})
}

func ExampleNew_errorHandler() {
	// Custom error handler for JSON responses.
	_ = keyauth.New(keyauth.Config{
		Validator: keyauth.StaticKeys("secret"),
		ErrorHandler: func(c *celeris.Context, err error) error {
			return c.JSON(401, map[string]string{"error": err.Error()})
		},
	})
}
