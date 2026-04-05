package redirect_test

import (
	"github.com/goceleris/celeris/middleware/redirect"
)

func ExampleHTTPSRedirect() {
	// Redirect all HTTP traffic to HTTPS.
	_ = redirect.HTTPSRedirect()
}

func ExampleHTTPSRedirect_permanent() {
	// Use 308 Permanent Redirect to preserve request method.
	_ = redirect.HTTPSRedirect(redirect.Config{Code: 308})
}

func ExampleWWWRedirect() {
	// Redirect non-www to www subdomain.
	_ = redirect.WWWRedirect()
}

func ExampleNonWWWRedirect() {
	// Redirect www to non-www host.
	_ = redirect.NonWWWRedirect()
}

func ExampleTrailingSlashRedirect() {
	// Add a trailing slash to paths that are missing one.
	_ = redirect.TrailingSlashRedirect()
}

func ExampleRemoveTrailingSlashRedirect() {
	// Strip the trailing slash from paths.
	_ = redirect.RemoveTrailingSlashRedirect()
}

func ExampleHTTPSWWWRedirect() {
	// Redirect to HTTPS + www in a single redirect.
	_ = redirect.HTTPSWWWRedirect()
}

func ExampleHTTPSNonWWWRedirect() {
	// Redirect to HTTPS + non-www in a single redirect.
	_ = redirect.HTTPSNonWWWRedirect()
}

func ExampleTrailingSlashRewrite() {
	// Add trailing slash in-place without sending a redirect.
	_ = redirect.TrailingSlashRewrite()
}

func ExampleRemoveTrailingSlashRewrite() {
	// Strip trailing slash in-place without sending a redirect.
	_ = redirect.RemoveTrailingSlashRewrite()
}

func ExampleHTTPSRedirect_skipPaths() {
	// Skip HTTPS redirect for health check and readiness probe endpoints.
	_ = redirect.HTTPSRedirect(redirect.Config{
		SkipPaths: []string{"/health", "/healthz", "/ready"},
	})
}
