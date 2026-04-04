package secure_test

import (
	"github.com/goceleris/celeris/middleware/secure"
)

func ExampleNew() {
	// Default OWASP-recommended security headers.
	_ = secure.New()
}

func ExampleNew_custom() {
	// Custom CSP and HSTS with preload.
	_ = secure.New(secure.Config{
		ContentSecurityPolicy: "default-src 'self'; script-src 'self'",
		HSTSMaxAge:            31536000,
		HSTSPreload:           true,
	})
}

func ExampleNew_permissionsPolicy() {
	// Restrict browser features via Permissions-Policy.
	_ = secure.New(secure.Config{
		PermissionsPolicy: "geolocation=(), camera=(), microphone=()",
	})
}
