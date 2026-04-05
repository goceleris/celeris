package csrf_test

import (
	"time"

	"github.com/goceleris/celeris"

	"github.com/goceleris/celeris/middleware/csrf"
)

func ExampleNew() {
	// Default: token extracted from X-CSRF-Token header.
	_ = csrf.New()
}

func ExampleNew_formLookup() {
	// Extract token from a form field instead of a header.
	_ = csrf.New(csrf.Config{
		TokenLookup: "form:_csrf",
	})
}

func ExampleNew_multipleExtractors() {
	// Try header first, then fall back to form field.
	_ = csrf.New(csrf.Config{
		TokenLookup: "header:X-CSRF-Token,form:_csrf",
	})
}

func ExampleNew_customErrorHandler() {
	// Custom error handling for CSRF failures.
	_ = csrf.New(csrf.Config{
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return celeris.NewHTTPError(403, "CSRF validation failed")
		},
	})
}

func ExampleNew_serverSideStorage() {
	// Enable server-side token storage for enhanced security.
	store := csrf.NewMemoryStorage()
	_ = csrf.New(csrf.Config{
		Storage:    store,
		Expiration: 2 * time.Hour,
	})
}

func ExampleNew_singleUseToken() {
	// Single-use tokens: each token is consumed after validation.
	_ = csrf.New(csrf.Config{
		Storage:        csrf.NewMemoryStorage(),
		SingleUseToken: true,
	})
}

func ExampleNew_wildcardTrustedOrigins() {
	// Allow all subdomains of example.com.
	_ = csrf.New(csrf.Config{
		TrustedOrigins: []string{"https://*.example.com"},
	})
}

func ExampleNew_sessionCookie() {
	// Browser-session-scoped cookie (MaxAge 0).
	_ = csrf.New(csrf.Config{
		CookieMaxAge: 0,
	})
}

func ExampleNew_customContextKey() {
	// Store token under a custom context key.
	_ = csrf.New(csrf.Config{
		ContextKey: "my_csrf_token",
	})
}

func ExampleTokenFromContext() {
	// Retrieve the CSRF token inside a handler to pass to templates.
	_ = func(c *celeris.Context) error {
		token := csrf.TokenFromContext(c)
		return c.String(200, "token: %s", token)
	}
}

func ExampleDeleteToken() {
	// Delete token during logout.
	_ = func(c *celeris.Context) error {
		_ = csrf.DeleteToken(c)
		return c.String(200, "logged out")
	}
}

func ExampleHandlerFromContext() {
	// Retrieve handler for more control.
	_ = func(c *celeris.Context) error {
		h := csrf.HandlerFromContext(c)
		if h != nil {
			_ = h.DeleteToken(c)
		}
		return c.String(200, "done")
	}
}
