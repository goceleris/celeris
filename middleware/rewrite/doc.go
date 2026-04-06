// Package rewrite provides URL rewrite middleware for celeris using
// regular expression pattern matching.
//
// The middleware matches the request path against a set of regex rules
// and either rewrites the path in-place (silent rewrite) or sends an
// HTTP redirect response.
//
// Basic usage (silent rewrite -- use anchored patterns):
//
//	server.Pre(rewrite.New(rewrite.Config{
//	    Rules: []rewrite.Rule{
//	        {Pattern: "^/old$", Replacement: "/new"},
//	    },
//	}))
//
// Redirect mode:
//
//	server.Pre(rewrite.New(rewrite.Config{
//	    Rules: []rewrite.Rule{
//	        {Pattern: "^/old$", Replacement: "/new"},
//	    },
//	    RedirectCode: 301,
//	}))
//
// # Regex and Capture Groups
//
// Rule patterns are Go regular expressions compiled with [regexp.MustCompile].
// The replacement string supports capture group substitution ($1, $2, ...)
// using [regexp.Regexp.ReplaceAllString] semantics:
//
//	server.Pre(rewrite.New(rewrite.Config{
//	    Rules: []rewrite.Rule{
//	        {Pattern: `/users/(\d+)/posts`, Replacement: "/api/v2/users/$1/posts"},
//	    },
//	}))
//
// # First-Match-Wins
//
// Rules are evaluated in the order provided. The first matching regex wins
// and subsequent rules are not checked:
//
//	Rules: []rewrite.Rule{
//	    {Pattern: "/a/.*", Replacement: "/alpha"},  // checked first
//	    {Pattern: "/b/.*", Replacement: "/beta"},   // checked second
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
//	    Rules: []rewrite.Rule{
//	        {Pattern: "/old", Replacement: "/new"},
//	    },
//	}))
//
// # Query String Preservation
//
// In redirect mode, the original query string is appended to the redirect
// URL. In silent rewrite mode, the query string is unmodified since only
// the path is rewritten.
//
// # Conditional Rewriting
//
// Rules can be restricted to specific HTTP methods or hosts:
//
//	server.Pre(rewrite.New(rewrite.Config{
//	    Rules: []rewrite.Rule{
//	        {
//	            Pattern:     "^/api/v1/(.*)$",
//	            Replacement: "/api/v2/$1",
//	            Methods:     []string{"GET", "HEAD"},
//	        },
//	        {
//	            Pattern:     "^/admin/(.*)$",
//	            Replacement: "/internal/$1",
//	            Host:        "admin.example.com",
//	        },
//	    },
//	}))
//
// When Methods is empty, the rule matches all methods. When Host is
// empty, the rule matches all hosts.
//
// # Init-Time Validation
//
// [New] panics if Rules is empty, if RedirectCode is non-zero and not a
// valid redirect status, or if any rule pattern is an invalid regex (via
// [regexp.MustCompile]).
//
// # Security
//
// Regex patterns are compiled once at init time via [regexp.MustCompile],
// not per-request. Avoid catastrophic backtracking patterns (e.g.,
// `(a+)+$`) which can cause high CPU during compilation.
//
// In redirect mode, the redirect URL is constructed from [celeris.Context.Host]
// and [celeris.Context.Scheme], which are derived from request headers.
// Without a reverse proxy that validates the Host header, an attacker
// can send a crafted Host to produce an open redirect. Ensure your
// deployment validates the Host header upstream (e.g., via the proxy
// middleware) or use silent rewrite mode (RedirectCode: 0) which only
// modifies the internal path.
//
// # Skipping
//
// Use [Config].Skip for dynamic skip logic or [Config].SkipPaths for
// exact-match path exclusions. Skipped requests call c.Next() without
// any rewrite.
package rewrite
