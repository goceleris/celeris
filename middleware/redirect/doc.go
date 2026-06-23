// Package redirect provides HTTP redirect middleware for common URL
// normalization patterns in Celeris.
//
// Nine constructor functions return a [celeris.HandlerFunc] and accept an
// optional [Config] to override the redirect status code or skip requests:
//
//   - [HTTPSRedirect] — redirects HTTP to HTTPS
//   - [WWWRedirect] — redirects non-www to www
//   - [NonWWWRedirect] — redirects www to non-www
//   - [TrailingSlashRedirect] — adds a trailing slash when missing
//   - [RemoveTrailingSlashRedirect] — strips a trailing slash
//   - [HTTPSWWWRedirect] — redirects to HTTPS + www in one hop
//   - [HTTPSNonWWWRedirect] — redirects to HTTPS + non-www in one hop
//   - [TrailingSlashRewrite] — adds a trailing slash in-place (no redirect)
//   - [RemoveTrailingSlashRewrite] — strips a trailing slash in-place (no redirect)
//
// All constructors default to HTTP 301. Set [Config].Code to 308 to
// preserve the original request method across the redirect. Query strings
// are always preserved. Register with [celeris.Server.Pre] so normalization
// runs before route lookup.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-routing-helpers
package redirect
