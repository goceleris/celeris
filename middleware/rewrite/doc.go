// Package rewrite provides URL rewrite middleware for celeris using
// regular expression pattern matching.
//
// The middleware matches the request path against a set of regex rules
// and either rewrites the path in-place (silent rewrite) or sends an
// HTTP redirect response.
//
// Basic usage (silent rewrite):
//
//	server.Pre(rewrite.New(rewrite.Config{
//	    Rules: map[string]string{
//	        "/old": "/new",
//	    },
//	}))
//
// Redirect mode:
//
//	server.Pre(rewrite.New(rewrite.Config{
//	    Rules: map[string]string{
//	        "/old": "/new",
//	    },
//	    RedirectCode: 301,
//	}))
//
// # Regex and Capture Groups
//
// Rule keys are Go regular expressions compiled with [regexp.MustCompile].
// The replacement string supports capture group substitution ($1, $2, ...)
// using [regexp.Regexp.ReplaceAllString] semantics:
//
//	server.Pre(rewrite.New(rewrite.Config{
//	    Rules: map[string]string{
//	        `/users/(\d+)/posts`: "/api/v2/users/$1/posts",
//	    },
//	}))
//
// # First-Match-Wins (Alphabetical Key Order)
//
// When multiple rules are defined, keys are sorted alphabetically at init
// time. The first matching regex wins and subsequent rules are not checked.
// This provides deterministic behavior regardless of Go map iteration order:
//
//	Rules: map[string]string{
//	    "/a/.*": "/alpha",  // checked first (alphabetical)
//	    "/b/.*": "/beta",   // checked second
//	}
//
// # Silent Rewrite vs Redirect
//
// When RedirectCode is 0 (default), the middleware calls [celeris.Context.SetPath]
// to modify the request path in-place. The client URL remains unchanged and
// downstream handlers see the rewritten path.
//
// When RedirectCode is a valid redirect status (301, 302, 303, 307, 308),
// the middleware sends an HTTP redirect response. The redirect URL preserves
// the original scheme, host, and query string. Downstream handlers are not
// executed.
//
// # Pre-Routing Middleware
//
// This middleware is designed to run via [celeris.Server.Pre] so that URL
// rewriting occurs before route lookup:
//
//	server.Pre(rewrite.New(rewrite.Config{
//	    Rules: map[string]string{"/old": "/new"},
//	}))
//
// # Query String Preservation
//
// In redirect mode, the original query string is appended to the redirect
// URL. In silent rewrite mode, the query string is unmodified since only
// the path is rewritten.
//
// # Init-Time Validation
//
// [New] panics if Rules is empty, if RedirectCode is non-zero and not a
// valid redirect status, or if any rule key is an invalid regex (via
// [regexp.MustCompile]).
//
// # Skipping
//
// Use [Config].Skip for dynamic skip logic or [Config].SkipPaths for
// exact-match path exclusions. Skipped requests call c.Next() without
// any rewrite.
package rewrite
