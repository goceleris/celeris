// Package session provides server-side session management middleware for
// celeris.
//
// Sessions are identified by a cookie (default), header, or query parameter,
// and data is stored server-side in a pluggable [Store]. The built-in
// [NewMemoryStore] uses sharded maps with a background cleanup goroutine,
// suitable for single-instance deployments.
//
// Attach the middleware with defaults (in-memory store, 24 h sessions):
//
//	server.Use(session.New())
//
// Retrieve and mutate the session in downstream handlers via [FromContext]:
//
//	s := session.FromContext(c)
//	s.Set("user", "admin")
//	name, ok := s.Get("user")
//
// Modified sessions are saved automatically after the handler chain returns.
// Call [Session.Destroy] to invalidate a session, [Session.Regenerate] to
// issue a new session ID (required after any authentication state change to
// prevent session fixation), or [Session.SetIdleTimeout] to override the
// per-session idle window ("remember me" flows).
//
// [CookieExtractor], [HeaderExtractor], [QueryExtractor], and
// [ChainExtractor] control where the session ID is read from. For
// out-of-band access (admin tools, background jobs) use [NewHandler], which
// exposes the middleware via [Handler.Middleware] and direct lookup via
// [Handler.GetByID]. Implement the [Store] interface to back sessions with
// any storage backend.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/data-stores
package session
