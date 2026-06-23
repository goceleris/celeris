// Package rewrite provides pre-routing URL rewrite middleware for celeris,
// matching the request path against ordered regular-expression rules.
//
// Construct the middleware with [New] and a [Config], then register it via
// [celeris.Server.Pre] so rewriting happens before route lookup. Each [Rule]
// has a Pattern (a Go regexp) and a Replacement that supports capture-group
// substitution ($1, $2, ...). Rules are evaluated in order and the first
// match wins.
//
// A rule rewrites silently or redirects depending on RedirectCode. When zero
// (the default), the path is rewritten in place via [celeris.Context.SetPath]
// and downstream handlers see the new path. When set to a redirect status
// (301, 302, 303, 307, 308), the middleware sends an HTTP redirect preserving
// the scheme, host, and query string. RedirectCode may be set on [Config] as
// the default or overridden per [Rule]. A rule can be scoped with Methods and
// Host; both default to matching everything. Use [Config].Skip or
// [Config].SkipPaths to bypass requests.
//
// [New] compiles patterns once via [regexp.MustCompile] and panics on an empty
// Rules slice, an invalid pattern, or an invalid non-zero RedirectCode.
//
//	server.Pre(rewrite.New(rewrite.Config{
//	    Rules: []rewrite.Rule{
//	        {Pattern: `^/api/v1/(.*)$`, Replacement: "/api/v2/$1"},
//	    },
//	}))
//
// In redirect mode the redirect URL is built from the request's Host and
// Scheme headers; validate the Host header upstream (or use silent rewrite)
// to avoid open redirects.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-routing-helpers
package rewrite
