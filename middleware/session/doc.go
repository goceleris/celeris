// Package session provides server-side session management middleware for
// celeris.
//
// Sessions are identified by a cookie (default), header, or query parameter,
// and data is stored server-side in a pluggable [Store]. The default
// [MemoryStore] uses sharded maps with a background cleanup goroutine,
// suitable for single-instance deployments.
//
// Basic usage with defaults (in-memory store, 24h sessions):
//
//	server.Use(session.New())
//
// Custom cookie name and timeouts:
//
//	server.Use(session.New(session.Config{
//	    CookieName:      "myapp_sid",
//	    IdleTimeout:     15 * time.Minute,
//	    AbsoluteTimeout: 2 * time.Hour,
//	}))
//
// # Accessing the Session
//
// Use [FromContext] to retrieve the session from downstream handlers:
//
//	s := session.FromContext(c)
//	s.Set("user", "admin")
//	name, ok := s.Get("user")
//
// Modified sessions are automatically saved after the handler chain returns.
// Call [Session.Destroy] to invalidate a session, or [Session.Regenerate] to
// issue a new session ID.
//
// # Session Fixation Prevention
//
// Applications MUST call [Session.Regenerate] (or [Session.Reset]) after
// any authentication state change to prevent session fixation attacks:
//
//	func loginHandler(c *celeris.Context) error {
//	    // ... validate credentials ...
//	    s := session.FromContext(c)
//	    if err := s.Regenerate(); err != nil {
//	        return err
//	    }
//	    s.Set("user", authenticatedUser)
//	    return nil
//	}
//
// # Pluggable Extractors
//
// [CookieExtractor], [HeaderExtractor], [QueryExtractor], and
// [ChainExtractor] control where the session ID is read from. When a
// non-cookie extractor is used, the session ID is returned in a response
// header named after [Config].CookieName so API clients can capture it.
//
// # Out-of-Band Access
//
// [NewHandler] returns a [Handler] providing both the middleware and
// out-of-band session access via [Handler.GetByID] for admin tools or
// background jobs.
//
// # Timeout Semantics
//
// IdleTimeout (default 30m) controls server-side expiry per session.
// AbsoluteTimeout (default 24h) caps total session lifetime. Set
// AbsoluteTimeout to -1 to disable it. [Session.SetIdleTimeout]
// overrides idle timeout for individual sessions ("remember me" flows).
//
// # Custom Stores
//
// Implement the [Store] interface to back sessions with any storage
// backend. All [Store] methods receive a [context.Context] propagated
// from the request for cancellation and deadline support.
//
// # Security
//
// CookieSecure defaults to false for development convenience. Production
// deployments MUST set CookieSecure: true to prevent cookie transmission
// over unencrypted connections.
package session
