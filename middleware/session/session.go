package session

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/store"
)

// ErrSessionDestroyed is returned when Save is called after Destroy.
var ErrSessionDestroyed = errors.New("session: cannot save a destroyed session")

var sessionPool = sync.Pool{New: func() any { return &Session{} }}

// sessionDataPool recycles the decoded session map so repeat Get paths
// avoid allocating a fresh map on every request. The map is cleared
// before handing out and after the request releases the session, so
// no data leaks between requests.
var sessionDataPool = sync.Pool{New: func() any {
	m := make(map[string]any, 8)
	return &m
}}

// Session holds per-request session data backed by a [Store].
//
// Session is designed for single-goroutine-per-request access and is NOT
// safe for concurrent use from multiple goroutines within the same request.
// If you need to access the session from multiple goroutines (e.g., a
// background task spawned mid-request), you must synchronize access
// externally.
type Session struct {
	id           string
	data         map[string]any
	store        store.KV
	ctx          context.Context
	releaseCtx   *celeris.Context // set by middleware entry so returnToPool is a closureless method value
	expiry       time.Duration
	idleOverride time.Duration // per-session idle timeout override; 0 = use config default
	keyGen       func() string
	// onReleaseFn is a pre-bound method value pointing at returnToPool.
	// Stored at pool.New time so the middleware hot path can register it
	// with c.OnRelease without re-allocating a fresh funcval per request.
	onReleaseFn func()
	// dataPoolPtr is the original *map[string]any drawn from
	// sessionDataPool; retained so returnToPool can Put the same pointer
	// back, avoiding the classic Put(&localMap) heap alloc trap.
	dataPoolPtr *map[string]any
	modified    bool
	fresh       bool // true for newly created sessions
	destroyed   bool
	readOnly    bool // true when session was obtained via Handler.GetByID
	pooledMap   bool // data slice was drawn from sessionDataPool and must be returned
}

// returnToPool is the cleanup hook registered with c.OnRelease on the
// middleware entry. Converting the original closure to a method value
// avoids capturing the celeris.Context in an anonymous function's
// backing struct — the receiver (*Session) already carries everything
// the cleanup needs (via s.releaseCtx). The method value allocation is
// a single 2-word runtime.funcval; Go does not currently elide it
// here, but it's smaller and more cache-friendly than the capture-
// struct the closure generated.
func (s *Session) returnToPool() {
	if s.releaseCtx != nil {
		s.releaseCtx.Set(ContextKey, nil) // issue #1: clear context key before pool Put
	}
	if s.data != nil && s.pooledMap {
		clear(s.data)
		if s.dataPoolPtr != nil {
			*s.dataPoolPtr = s.data
			sessionDataPool.Put(s.dataPoolPtr)
		} else {
			// Defensive: pooledMap set but no pool ptr (shouldn't
			// happen after this refactor; handle gracefully anyway).
			m := s.data
			sessionDataPool.Put(&m)
		}
	}
	s.data = nil
	s.dataPoolPtr = nil
	s.pooledMap = false
	s.ctx = nil
	s.releaseCtx = nil
	sessionPool.Put(s)
}

// Get returns the value for key from the session data.
func (s *Session) Get(key string) (any, bool) {
	v, ok := s.data[key]
	return v, ok
}

// GetString returns the value for key as a string. Returns "" if the key
// is missing or the value is not a string.
func (s *Session) GetString(key string) string {
	v := s.data[key]
	str, _ := v.(string)
	return str
}

// GetInt returns the value for key as an int. Returns 0 if the key is
// missing or the value is not an int.
func (s *Session) GetInt(key string) int {
	v := s.data[key]
	i, _ := v.(int)
	return i
}

// GetBool returns the value for key as a bool. Returns false if the key
// is missing or the value is not a bool.
func (s *Session) GetBool(key string) bool {
	v := s.data[key]
	b, _ := v.(bool)
	return b
}

// GetFloat64 returns the value for key as a float64. Returns 0 if the key
// is missing or the value is not a float64.
func (s *Session) GetFloat64(key string) float64 {
	v := s.data[key]
	f, _ := v.(float64)
	return f
}

// Set stores a key-value pair in the session and marks it as modified.
// The reserved key "_abs_exp" is silently rejected to protect internal
// absolute-timeout bookkeeping.
func (s *Session) Set(key string, value any) {
	if key == absExpKey {
		return
	}
	s.data[key] = value
	s.modified = true
}

// Delete removes a key from the session. The session is marked as modified
// only if the key was actually present.
func (s *Session) Delete(key string) {
	if _, ok := s.data[key]; !ok {
		return
	}
	delete(s.data, key)
	s.modified = true
}

// Clear removes all user data from the session and marks it as modified.
// The internal _abs_exp timestamp is preserved.
func (s *Session) Clear() {
	absExp, hasAbsExp := s.data[absExpKey]
	clear(s.data)
	if hasAbsExp {
		s.data[absExpKey] = absExp
	}
	s.modified = true
}

// ID returns the session identifier.
func (s *Session) ID() string { return s.id }

// IsFresh returns true if this is a newly created session (no prior cookie).
func (s *Session) IsFresh() bool { return s.fresh }

// Keys returns a sorted list of all user-visible keys in the session data.
// The internal "_abs_exp" key is excluded.
func (s *Session) Keys() []string {
	keys := make([]string, 0, len(s.data))
	for k := range s.data {
		if k == absExpKey {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Len returns the number of user-visible key-value pairs in the session.
// The internal "_abs_exp" key is excluded from the count.
func (s *Session) Len() int {
	n := len(s.data)
	if _, ok := s.data[absExpKey]; ok {
		n--
	}
	return n
}

// Save persists the session data to the store. This is called automatically
// after the handler chain when the session has been modified; call it
// explicitly only if you need to guarantee persistence mid-handler.
// Returns [ErrSessionDestroyed] if the session has been destroyed.
func (s *Session) Save() error {
	if s.destroyed {
		return ErrSessionDestroyed
	}
	if s.readOnly {
		panic("session: Save called on a read-only session returned by GetByID; use the middleware pipeline for writes")
	}
	buf, err := store.EncodeJSON(s.data)
	if err != nil {
		return err
	}
	return s.store.Set(s.ctx, s.id, buf, s.expiry)
}

// Destroy invalidates the session by clearing data and deleting it from
// the store.
func (s *Session) Destroy() error {
	if s.readOnly {
		panic("session: Destroy called on a read-only session returned by GetByID; use the middleware pipeline for writes")
	}
	s.data = make(map[string]any)
	s.modified = false
	s.destroyed = true
	return s.store.Delete(s.ctx, s.id)
}

// Regenerate issues a new session ID while preserving data. The old session
// is deleted from the store. The data is saved under the new ID by the
// post-handler save (since modified is set to true).
//
// The internal _abs_exp timestamp is reset to the current time so the
// regenerated session gets a fresh absolute timeout window.
//
// Applications MUST call Regenerate after authentication state changes
// (e.g., login, privilege escalation) to prevent session fixation attacks.
func (s *Session) Regenerate() error {
	if s.readOnly {
		panic("session: Regenerate called on a read-only session returned by GetByID; use the middleware pipeline for writes")
	}
	oldID := s.id
	s.id = s.keyGen()
	if err := s.store.Delete(s.ctx, oldID); err != nil {
		return err
	}
	// Reset _abs_exp so the regenerated session gets a fresh absolute
	// timeout window.
	if _, ok := s.data[absExpKey]; ok {
		s.data[absExpKey] = time.Now().UnixNano()
	}
	s.modified = true
	return nil
}

// SetIdleTimeout overrides the idle timeout for this individual session.
// The post-handler save uses this value instead of the config-level
// [Config].IdleTimeout. Pass 0 to revert to the config default.
func (s *Session) SetIdleTimeout(d time.Duration) {
	s.idleOverride = d
	s.modified = true
}

// Reset is a convenience method that combines [Session.Clear],
// [Session.Regenerate], and marks the session as modified in a single call.
// Useful for "log out and start fresh" flows.
//
// Note: Reset is not atomic. If Clear succeeds but Regenerate fails
// (e.g., store error on Delete), the session data is already cleared
// but the old session ID is retained. Callers should handle the returned
// error accordingly.
func (s *Session) Reset() error {
	s.Clear()
	return s.Regenerate()
}

// FromContext retrieves the [Session] from the request context.
// Returns nil if the session middleware was not applied or the request was
// skipped.
func FromContext(c *celeris.Context) *Session {
	v, ok := c.Get(ContextKey)
	if !ok {
		return nil
	}
	s, _ := v.(*Session)
	return s
}

// validSessionID checks that s is exactly 64 lowercase hex characters
// [0-9a-f]. Uppercase hex (A-F) is intentionally rejected because the
// default [bufferedKeyGenerator] produces lowercase output; accepting
// uppercase would double the ID space without security benefit and could
// mask tampered cookies.
func validSessionID(s string) bool {
	if len(s) != sessionIDHexLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Handler holds the session middleware and provides methods for out-of-band
// session access (e.g., admin tools, background jobs).
type Handler struct {
	store store.KV
	mw    celeris.HandlerFunc
}

// Middleware returns the [celeris.HandlerFunc] to pass to server.Use().
func (h *Handler) Middleware() celeris.HandlerFunc { return h.mw }

// GetByID retrieves a session by its raw ID without an HTTP context.
// This is intended for admin tools, background jobs, or WebSocket handlers
// that need to inspect session data outside of the middleware pipeline.
//
// The returned [Session] is read-only: use Get, GetString, GetInt,
// GetBool, GetFloat64, Keys, Len, ID, and IsFresh. Calling Save,
// Regenerate, or Destroy on a GetByID session panics with a descriptive
// message because no store or context is bound.
// Returns nil and no error if the session does not exist.
func (h *Handler) GetByID(ctx context.Context, id string) (*Session, error) {
	raw, err := h.store.Get(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	data := make(map[string]any)
	if derr := store.DecodeJSON(raw, &data); derr != nil {
		return nil, derr
	}
	return &Session{
		id:       id,
		data:     data,
		readOnly: true,
	}, nil
}

// NewHandler creates a session [Handler] that exposes both the middleware
// and out-of-band session access via [Handler.GetByID].
func NewHandler(config ...Config) *Handler {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	h := &Handler{store: cfg.Store}
	h.mw = newMiddleware(cfg)
	return h
}

// New creates a session middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()
	return newMiddleware(cfg)
}

func newMiddleware(cfg Config) celeris.HandlerFunc {
	skipMap := make(map[string]struct{}, len(cfg.SkipPaths))
	for _, p := range cfg.SkipPaths {
		skipMap[p] = struct{}{}
	}

	cookieName := cfg.CookieName
	keyGen := cfg.KeyGenerator
	idleTimeout := cfg.IdleTimeout
	absTimeout := cfg.AbsoluteTimeout
	absDisabled := absTimeout < 0
	errorHandler := cfg.ErrorHandler
	if errorHandler == nil {
		errorHandler = func(_ *celeris.Context, err error) error { return err }
	}

	extract := cfg.Extractor
	useCookieExtractor := extract == nil
	if useCookieExtractor {
		extract = CookieExtractor(cookieName)
	}

	cookie := celeris.Cookie{
		Name:     cfg.CookieName,
		Path:     cfg.CookiePath,
		Domain:   cfg.CookieDomain,
		MaxAge:   *cfg.CookieMaxAge,
		Secure:   cfg.CookieSecure,
		HTTPOnly: *cfg.CookieHTTPOnly,
		SameSite: cfg.CookieSameSite,
	}

	kv := cfg.Store

	return func(c *celeris.Context) error {
		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
		}

		if _, ok := skipMap[c.Path()]; ok {
			return c.Next()
		}

		reqCtx := c.Context()

		sess := sessionPool.Get().(*Session)
		sess.store = kv
		sess.ctx = reqCtx
		sess.expiry = idleTimeout
		sess.keyGen = keyGen
		sess.data = nil // set from store.Get or make below
		sess.id = ""
		sess.modified = false
		sess.destroyed = false
		sess.fresh = false
		sess.idleOverride = 0
		sess.readOnly = false

		// Register cleanup callback to return session to pool on all exit
		// paths — including panics (issue #12: pool leak on error paths).
		// Uses a method value over *Session (sess.releaseCtx provides the
		// context back-pointer) instead of an anonymous closure so the
		// escape cost is one funcval, not a multi-capture struct.
		sess.releaseCtx = c
		// Lazy-bind the method-value on first use of a pooled Session so
		// subsequent requests reuse the stored funcval without
		// re-materialising it. Go's init cycle detector prevents binding
		// inside sessionPool.New since returnToPool references the pool.
		if sess.onReleaseFn == nil {
			sess.onReleaseFn = sess.returnToPool
		}
		c.OnRelease(sess.onReleaseFn)

		loaded := false
		sid := extract(c)
		if sid != "" && !validSessionID(sid) {
			sid = ""
		}
		if sid != "" {
			raw, loadErr := kv.Get(reqCtx, sid)
			var data map[string]any
			if loadErr != nil && !errors.Is(loadErr, store.ErrNotFound) {
				return errorHandler(c, loadErr)
			}
			if loadErr == nil {
				mp := sessionDataPool.Get().(*map[string]any)
				data = *mp
				if derr := store.DecodeJSON(raw, &data); derr != nil {
					clear(data)
					sessionDataPool.Put(mp)
					return errorHandler(c, derr)
				}
				sess.pooledMap = true
				sess.dataPoolPtr = mp
			}
			if data != nil && !absDisabled {
				// Check absolute timeout.
				expired := false
				ts, hasAbsExp := data[absExpKey]
				if hasAbsExp {
					// Handle both int64 (native) and float64 (JSON-decoded)
					// type assertions (issue #4).
					var created int64
					switch v := ts.(type) {
					case int64:
						created = v
					case float64:
						created = int64(v)
					}
					if created > 0 && time.Since(time.Unix(0, created)) > absTimeout {
						expired = true
					}
				} else {
					// _abs_exp missing from a loaded session: treat as
					// expired (issue #3).
					expired = true
				}
				if expired {
					_ = kv.Delete(reqCtx, sid)
					data = nil
				}
			}
			if data != nil {
				sess.id = sid
				sess.data = data
				loaded = true
			}
		}

		if !loaded {
			sess.id = keyGen()
			sess.fresh = true
			// Reuse pooled maps on the fresh-session path too so both
			// cold-start and warm sessions share alloc amortisation.
			mp := sessionDataPool.Get().(*map[string]any)
			sess.data = *mp
			sess.pooledMap = true
			sess.dataPoolPtr = mp
			if !absDisabled {
				sess.data[absExpKey] = time.Now().UnixNano()
			}
		}

		c.Set(ContextKey, sess)

		chainErr := c.Next()

		// validateAdmission is a no-op in production (see
		// validation_default.go); under -tags=validation it asserts
		// session-owner binding and increments
		// validation.SessionOwnerMismatches on violation. Runs AFTER
		// c.Next() so the handler chain has had a chance to populate
		// the owner data the assertion compares against.
		validateAdmission(sess)

		if sess.destroyed {
			if useCookieExtractor {
				ck := cookie
				ck.Value = ""
				ck.MaxAge = -1
				if c.IsTLS() || c.Scheme() == "https" {
					ck.Secure = true
				}
				c.SetCookie(&ck)
			} else {
				c.SetHeader(cookieName, "")
			}
		} else if sess.modified || sess.fresh {
			expiry := idleTimeout
			if sess.idleOverride > 0 {
				expiry = sess.idleOverride
			}
			buf, encErr := store.EncodeJSON(sess.data)
			if encErr != nil {
				if chainErr != nil {
					return errorHandler(c, fmt.Errorf("%w; handler chain error: %w", encErr, chainErr))
				}
				return errorHandler(c, encErr)
			}
			saveErr := kv.Set(reqCtx, sess.id, buf, expiry)
			if saveErr != nil {
				// Issue #11: wrap chainErr with save error so neither
				// is swallowed.
				if chainErr != nil {
					return errorHandler(c, fmt.Errorf("%w; handler chain error: %w", saveErr, chainErr))
				}
				return errorHandler(c, saveErr)
			}
			if useCookieExtractor {
				ck := cookie
				ck.Value = sess.id
				if c.IsTLS() || c.Scheme() == "https" {
					ck.Secure = true
				}
				c.SetCookie(&ck)
			} else {
				c.SetHeader(cookieName, sess.id)
			}
		}

		return chainErr
	}
}
