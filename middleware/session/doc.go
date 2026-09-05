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
// # When the cookie is emitted
//
// celeris puts the response headers on the wire the moment a handler calls
// c.JSON, c.String, c.Blob or another body writer, so the session cookie
// cannot be added after the handler returns. The middleware therefore emits
// the Set-Cookie header (or, with a non-cookie [Extractor], the session-id
// response header) at the FIRST of: a mutation ([Session.Set],
// [Session.Delete], [Session.Clear], [Session.SetIdleTimeout]), an explicit
// [Session.Save], [Session.Regenerate] (which re-emits with the new ID),
// [Session.Destroy] (which emits the clearing cookie), or — when
// [Config.SaveUnmodified] is set — before the handler runs for a fresh
// session. Exactly one session Set-Cookie is ever present on a response.
//
// Mutate the session before writing the response body:
//
//	s := session.FromContext(c)
//	s.Set("user", id)           // cookie goes on the response here
//	return c.JSON(200, payload) // headers, cookie included, hit the wire
//
// A mutation made after the body was written still persists the session,
// but no cookie can reach the client. When that cookie would have changed
// what the client holds — a fresh session's id, a regenerated id, or the
// clearing cookie — the request is counted in [DroppedCookies] (and
// validation.SessionCookieDrops under -tags=validation) and the client
// starts a new session on its next request. Mutating a loaded session late
// is not counted: the client already holds that id and the data is saved
// under it; only the cookie's Max-Age refresh is lost.
//
// When the post-handler save fails (store.Set or JSON encoding), the error
// goes to [Config.ErrorHandler] and any session cookie emitted earlier in the
// request for an id the store never received — a fresh session's, or the
// new id after [Session.Regenerate] — is taken back off the response, so
// the failed response carries no session cookie, exactly as it did when the
// cookie was emitted after the save. A loaded session's own id stays on the
// response: it is still valid server-side. With [Config.WriteBehind] the
// save cannot fail synchronously (the store error reaches ErrorHandler
// later, with a nil Context) and the cookie stays.
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
