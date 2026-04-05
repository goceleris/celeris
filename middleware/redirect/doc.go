// Package redirect provides HTTP redirect middleware for common URL
// normalization patterns in celeris.
//
// Nine constructor functions cover redirect and rewrite scenarios:
//
// # Redirect Functions
//
//   - [HTTPSRedirect] — redirects HTTP traffic to HTTPS
//   - [WWWRedirect] — redirects non-www to www subdomain
//   - [NonWWWRedirect] — redirects www to non-www host
//   - [TrailingSlashRedirect] — adds trailing slash when missing
//   - [RemoveTrailingSlashRedirect] — strips trailing slash
//
// # Combined Redirect Functions
//
//   - [HTTPSWWWRedirect] — redirects to HTTPS + www in a single redirect
//   - [HTTPSNonWWWRedirect] — redirects to HTTPS + non-www in a single redirect
//
// # Rewrite Functions
//
//   - [TrailingSlashRewrite] — adds trailing slash in-place (no redirect)
//   - [RemoveTrailingSlashRewrite] — strips trailing slash in-place (no redirect)
//
// Each constructor accepts an optional [Config] to override the redirect
// status code, skip specific paths, or provide a dynamic skip function.
// The rewrite functions accept [Config] for skip logic but ignore the Code
// field since they do not send a redirect response.
//
// # Redirect Code (301 vs 308)
//
// The default redirect code is 301 (Moved Permanently). This is appropriate
// for GET requests but can cause problems for POST/PUT/DELETE: browsers and
// HTTP clients are allowed to change the request method to GET when following
// a 301 redirect (RFC 7231 §6.4.2). Set [Config].Code to 308 (Permanent
// Redirect) to guarantee the client preserves the original request method
// (RFC 7538 §3). Use 307 for the temporary equivalent that also preserves
// the method.
//
// # Query String Preservation
//
// All redirect functions preserve the original query string. The query is
// appended to the redirect URL when non-empty.
//
// # Short-Circuit Behavior
//
// When a redirect occurs, the middleware returns immediately via
// [celeris.Context.Redirect] without calling c.Next(). Downstream
// handlers are not executed.
//
// # Empty Host
//
// If [celeris.Context.Host] returns an empty string (malformed request),
// all redirect and rewrite functions pass through to c.Next() without
// modification. This prevents generating malformed redirect URLs.
//
// # Root Path Handling
//
// The trailing slash variants treat the root path "/" as a no-op. "/" is
// never modified by [TrailingSlashRedirect] (already has a slash) or
// [RemoveTrailingSlashRedirect] (root must keep its slash).
//
// # Pre-Routing Middleware
//
// This middleware is designed to run via [celeris.Server.Pre] so that URL
// normalization occurs before route lookup:
//
//	server.Pre(redirect.HTTPSRedirect())
//	server.Pre(redirect.TrailingSlashRedirect())
//
// # Redirect Loops
//
// Be careful not to combine conflicting redirect middleware. For example,
// using both [TrailingSlashRedirect] and [RemoveTrailingSlashRedirect]
// creates an infinite redirect loop. Similarly, using both [WWWRedirect]
// and [NonWWWRedirect] causes a loop. The combined constructors
// [HTTPSWWWRedirect] and [HTTPSNonWWWRedirect] avoid the double-redirect
// that would occur from chaining [HTTPSRedirect] with [WWWRedirect] or
// [NonWWWRedirect].
//
// # Skipping
//
// Use [Config].Skip for dynamic skip logic or [Config].SkipPaths for
// exact-match path exclusions. Skipped requests call c.Next() without
// any redirect.
package redirect
