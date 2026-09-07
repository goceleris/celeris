package session

import (
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// nowNano is the _abs_exp value a freshly created session would carry.
func nowNano() int64 { return time.Now().UnixNano() }

// The tests in this file pin the Set-Cookie-before-body contract: celeris
// materialises the response headers on the wire the moment a handler calls
// c.JSON/String/Blob (Context.written flips to true), so a Set-Cookie added
// after the handler returned is silently dropped. The session middleware
// must therefore put the cookie on the response at the first mutation, not
// after c.Next(). The recorder's Headers are the wire snapshot; for handlers
// that write no body the router writes the default response later, so the
// pending ctx.ResponseHeaders() is the observable in that case.

// effectiveHeaders returns the headers a client would receive: the wire
// snapshot when the handler wrote, the pending headers otherwise.
func effectiveHeaders(ctx *celeris.Context, rec *celeristest.ResponseRecorder) [][2]string {
	if ctx.IsWritten() {
		return rec.Headers
	}
	return ctx.ResponseHeaders()
}

func headerValues(hdrs [][2]string, key string) []string {
	var out []string
	for _, h := range hdrs {
		if h[0] == key {
			out = append(out, h[1])
		}
	}
	return out
}

// assertSingleSessionCookie asserts exactly one Set-Cookie header for the
// default cookie name reached the client and that it carries sid.
func assertSingleSessionCookie(t *testing.T, hdrs [][2]string, sid string) string {
	t.Helper()
	if sid == "" {
		t.Fatal("test bug: empty sid")
	}
	scs := headerValues(hdrs, "set-cookie")
	if len(scs) != 1 {
		t.Fatalf("got %d Set-Cookie headers %q, want exactly 1", len(scs), scs)
	}
	if !strings.HasPrefix(scs[0], "celeris_session="+sid+";") {
		t.Fatalf("Set-Cookie %q does not carry session id %s", scs[0], sid)
	}
	return scs[0]
}

// assertSingleClearingCookie asserts exactly one Set-Cookie header reached
// the client and that it deletes the session cookie.
func assertSingleClearingCookie(t *testing.T, hdrs [][2]string) {
	t.Helper()
	scs := headerValues(hdrs, "set-cookie")
	if len(scs) != 1 {
		t.Fatalf("got %d Set-Cookie headers %q, want exactly 1 clearing cookie", len(scs), scs)
	}
	if !strings.HasPrefix(scs[0], "celeris_session=;") || !strings.Contains(scs[0], "Max-Age=0") {
		t.Fatalf("Set-Cookie %q is not a clearing cookie", scs[0])
	}
}

// (a) sess.Set + c.JSON: the cookie must be on the wire, and a second
// request replaying it must see the data without a re-emission.
func TestSetThenBodyEmitsCookieOnWire(t *testing.T) {
	kv := NewMemoryStore()
	mw := New(Config{Store: kv})

	var sid string
	login := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("user", "alice")
		sid = s.ID()
		return c.JSON(200, map[string]string{"sid": s.ID()})
	}
	ctx, rec := celeristest.NewContextT(t, "POST", "/login", celeristest.WithHandlers(mw, login))
	testutil.AssertNoError(t, ctx.Next())
	if rec.StatusCode != 200 {
		t.Fatalf("status %d, want 200", rec.StatusCode)
	}
	assertSingleSessionCookie(t, rec.Headers, sid)

	var got string
	me := func(c *celeris.Context) error {
		got = FromContext(c).GetString("user")
		return c.JSON(200, map[string]string{"user": got})
	}
	ctx2, rec2 := celeristest.NewContextT(t, "GET", "/me",
		celeristest.WithCookie("celeris_session", sid),
		celeristest.WithHandlers(mw, me))
	testutil.AssertNoError(t, ctx2.Next())
	if got != "alice" {
		t.Fatalf("second request did not see the session data: user=%q", got)
	}
	if scs := headerValues(rec2.Headers, "set-cookie"); len(scs) != 0 {
		t.Fatalf("unmodified follow-up request re-emitted Set-Cookie %q", scs)
	}
}

// (b) The login shape from probatorium's auth_session_ratelimit refapp:
// sess.Set + sess.Save + c.JSON, with and without write-behind.
func TestLoginShapeSetSaveBodyEmitsCookie(t *testing.T) {
	for _, wb := range []bool{false, true} {
		name := "sync"
		if wb {
			name = "write-behind"
		}
		t.Run(name, func(t *testing.T) {
			kv := NewMemoryStore()
			mw, closer := NewWithCloser(Config{Store: kv, WriteBehind: wb})
			defer func() { _ = closer.Close() }()

			var sid string
			login := func(c *celeris.Context) error {
				s := FromContext(c)
				s.Set("user", "alice")
				if err := s.Save(); err != nil {
					return err
				}
				sid = s.ID()
				return c.JSON(200, map[string]any{"sid": s.ID()})
			}
			ctx, rec := celeristest.NewContextT(t, "POST", "/login", celeristest.WithHandlers(mw, login))
			testutil.AssertNoError(t, ctx.Next())
			assertSingleSessionCookie(t, rec.Headers, sid)
			_ = closer.Close()
			data, ok := loadMap(t, kv, sid)
			if !ok || data["user"] != "alice" {
				t.Fatalf("session %s not persisted with user=alice: %v", sid, data)
			}
		})
	}
}

// (c) SaveUnmodified=true: a fresh session that the handler never touches
// still gets its cookie even though the handler writes a body.
func TestSaveUnmodifiedFreshWithBodyEmitsCookie(t *testing.T) {
	kv := NewMemoryStore()
	mw := New(Config{Store: kv, SaveUnmodified: true})

	var sid string
	h := func(c *celeris.Context) error {
		sid = FromContext(c).ID()
		return c.String(200, "ok")
	}
	ctx, rec := celeristest.NewContextT(t, "GET", "/", celeristest.WithHandlers(mw, h))
	testutil.AssertNoError(t, ctx.Next())
	assertSingleSessionCookie(t, rec.Headers, sid)
	if _, ok := loadMap(t, kv, sid); !ok {
		t.Fatalf("fresh session %s not persisted under SaveUnmodified", sid)
	}
}

// (d) Destroy + c.JSON: the clearing cookie must be on the wire.
func TestDestroyThenBodyEmitsClearingCookie(t *testing.T) {
	kv := NewMemoryStore()
	mw := New(Config{Store: kv})
	sid := hexID(0x1a)
	saveMap(t, kv, sid, map[string]any{"user": "alice", absExpKey: nowNano()}, 0)

	logout := func(c *celeris.Context) error {
		if err := FromContext(c).Destroy(); err != nil {
			return err
		}
		return c.JSON(200, map[string]string{"status": "bye"})
	}
	ctx, rec := celeristest.NewContextT(t, "POST", "/logout",
		celeristest.WithCookie("celeris_session", sid),
		celeristest.WithHandlers(mw, logout))
	testutil.AssertNoError(t, ctx.Next())
	assertSingleClearingCookie(t, rec.Headers)
	if _, ok := loadMap(t, kv, sid); ok {
		t.Fatalf("destroyed session %s still in store", sid)
	}

	// SaveUnmodified emits the fresh cookie before the handler runs; a
	// Destroy in the handler must replace it, not add a second header.
	t.Run("fresh-under-SaveUnmodified", func(t *testing.T) {
		mw := New(Config{Store: NewMemoryStore(), SaveUnmodified: true})
		ctx, rec := celeristest.NewContextT(t, "POST", "/logout", celeristest.WithHandlers(mw, logout))
		testutil.AssertNoError(t, ctx.Next())
		assertSingleClearingCookie(t, rec.Headers)
	})
}

// (e) Regenerate + c.JSON: the wire cookie carries the NEW id and only the
// new id, regardless of whether the mutation happened before or after.
func TestRegenerateThenBodyEmitsNewID(t *testing.T) {
	cases := []struct {
		name string
		fn   func(s *Session) error
	}{
		{"set-then-regenerate", func(s *Session) error { s.Set("user", "alice"); return s.Regenerate() }},
		{"regenerate-then-set", func(s *Session) error {
			if err := s.Regenerate(); err != nil {
				return err
			}
			s.Set("user", "alice")
			return nil
		}},
		{"reset", func(s *Session) error {
			if err := s.Reset(); err != nil {
				return err
			}
			s.Set("user", "alice")
			return nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kv := NewMemoryStore()
			mw := New(Config{Store: kv})
			oldID := hexID(0x2b)
			saveMap(t, kv, oldID, map[string]any{"user": "old", absExpKey: nowNano()}, 0)

			var newID string
			h := func(c *celeris.Context) error {
				s := FromContext(c)
				if err := tc.fn(s); err != nil {
					return err
				}
				newID = s.ID()
				return c.JSON(200, map[string]string{"sid": s.ID()})
			}
			ctx, rec := celeristest.NewContextT(t, "POST", "/login",
				celeristest.WithCookie("celeris_session", oldID),
				celeristest.WithHandlers(mw, h))
			testutil.AssertNoError(t, ctx.Next())
			if newID == oldID || newID == "" {
				t.Fatalf("Regenerate did not rotate the id: %q", newID)
			}
			sc := assertSingleSessionCookie(t, rec.Headers, newID)
			if strings.Contains(sc, oldID) {
				t.Fatalf("Set-Cookie %q still carries the old id", sc)
			}
			if _, ok := loadMap(t, kv, oldID); ok {
				t.Fatalf("old session %s still in store after Regenerate", oldID)
			}
			data, ok := loadMap(t, kv, newID)
			if !ok || data["user"] != "alice" {
				t.Fatalf("data not persisted under new id %s: %v", newID, data)
			}
		})
	}
}

// (g) Exactly one session Set-Cookie in every shape, body or no body.
func TestExactlyOneSessionCookiePerResponse(t *testing.T) {
	type shape struct {
		name           string
		saveUnmodified bool
		handler        func(c *celeris.Context, s *Session) error
	}
	shapes := []shape{
		{"set-no-body", false, func(_ *celeris.Context, s *Session) error { s.Set("k", "v"); return nil }},
		{"save-only-no-body", false, func(_ *celeris.Context, s *Session) error { return s.Save() }},
		{"set-nocontent", false, func(c *celeris.Context, s *Session) error { s.Set("k", "v"); return c.NoContent(204) }},
		{"set-json", false, func(c *celeris.Context, s *Session) error { s.Set("k", "v"); return c.JSON(200, "x") }},
		{"set-save-json", false, func(c *celeris.Context, s *Session) error {
			s.Set("k", "v")
			if err := s.Save(); err != nil {
				return err
			}
			return c.JSON(200, "x")
		}},
		{"set-delete-clear-set-json", false, func(c *celeris.Context, s *Session) error {
			s.Set("k", "v")
			s.Delete("k")
			s.Clear()
			s.Set("k2", "v2")
			s.SetIdleTimeout(0)
			return c.JSON(200, "x")
		}},
		{"set-regenerate-json", false, func(c *celeris.Context, s *Session) error {
			s.Set("k", "v")
			if err := s.Regenerate(); err != nil {
				return err
			}
			return c.String(200, "x")
		}},
		{"saveunmodified-untouched-json", true, func(c *celeris.Context, _ *Session) error { return c.JSON(200, "x") }},
		{"saveunmodified-set-json", true, func(c *celeris.Context, s *Session) error { s.Set("k", "v"); return c.JSON(200, "x") }},
		{"saveunmodified-regenerate-json", true, func(c *celeris.Context, s *Session) error {
			if err := s.Regenerate(); err != nil {
				return err
			}
			return c.JSON(200, "x")
		}},
		{"saveunmodified-no-body", true, func(_ *celeris.Context, _ *Session) error { return nil }},
	}
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			kv := NewMemoryStore()
			mw := New(Config{Store: kv, SaveUnmodified: sh.saveUnmodified})
			var sid string
			h := func(c *celeris.Context) error {
				s := FromContext(c)
				err := sh.handler(c, s)
				sid = s.ID()
				return err
			}
			ctx, rec := celeristest.NewContextT(t, "GET", "/", celeristest.WithHandlers(mw, h))
			testutil.AssertNoError(t, ctx.Next())
			assertSingleSessionCookie(t, effectiveHeaders(ctx, rec), sid)
			// For no-body shapes the pending list IS what the router will
			// write; it must not hold a second copy from the post-chain
			// fallback. (After a write the Context reuses its inline header
			// buffer for the wire headers, so the pending list is no longer
			// meaningful — the recorder snapshot above covers those shapes.)
			if !ctx.IsWritten() {
				if n := len(headerValues(ctx.ResponseHeaders(), "set-cookie")); n != 1 {
					t.Fatalf("pending response headers hold %d Set-Cookie entries, want 1: %v", n, ctx.ResponseHeaders())
				}
			}
			if _, ok := loadMap(t, kv, sid); !ok {
				t.Fatalf("session %s not persisted", sid)
			}
		})
	}
}

// Header-extractor mode: the session id travels in a response header named
// after the cookie; it must be on the wire before the body just the same.
func TestHeaderExtractorEmitsIDBeforeBody(t *testing.T) {
	kv := NewMemoryStore()
	mw := New(Config{Store: kv, Extractor: HeaderExtractor("X-Session-ID")})

	var sid string
	login := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("user", "alice")
		sid = s.ID()
		return c.JSON(200, "ok")
	}
	ctx, rec := celeristest.NewContextT(t, "POST", "/login", celeristest.WithHandlers(mw, login))
	testutil.AssertNoError(t, ctx.Next())
	if got := headerValues(rec.Headers, "celeris_session"); len(got) != 1 || got[0] != sid {
		t.Fatalf("celeris_session response header on the wire: %q, want [%s]", got, sid)
	}
	if scs := headerValues(rec.Headers, "set-cookie"); len(scs) != 0 {
		t.Fatalf("header-extractor mode must not emit Set-Cookie, got %q", scs)
	}

	var got string
	me := func(c *celeris.Context) error {
		got = FromContext(c).GetString("user")
		return c.JSON(200, "ok")
	}
	ctx2, _ := celeristest.NewContextT(t, "GET", "/me",
		celeristest.WithHeader("X-Session-ID", sid),
		celeristest.WithHandlers(mw, me))
	testutil.AssertNoError(t, ctx2.Next())
	if got != "alice" {
		t.Fatalf("second request did not see the session data: user=%q", got)
	}

	// Destroy in header mode clears the header value.
	logout := func(c *celeris.Context) error {
		_ = FromContext(c).Destroy()
		return c.JSON(200, "bye")
	}
	ctx3, rec3 := celeristest.NewContextT(t, "POST", "/logout",
		celeristest.WithHeader("X-Session-ID", sid),
		celeristest.WithHandlers(mw, logout))
	testutil.AssertNoError(t, ctx3.Next())
	if got := headerValues(rec3.Headers, "celeris_session"); len(got) != 1 || got[0] != "" {
		t.Fatalf("celeris_session header after Destroy: %q, want one empty value", got)
	}
}

// The early cookie must honour the TLS/https Secure upgrade exactly like the
// post-chain cookie did.
func TestEarlyCookieSecureUnderHTTPS(t *testing.T) {
	mw := New(Config{Store: NewMemoryStore()})
	h := func(c *celeris.Context) error {
		FromContext(c).Set("k", "v")
		return c.JSON(200, "ok")
	}
	ctx, rec := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithScheme("https"),
		celeristest.WithHandlers(mw, h))
	testutil.AssertNoError(t, ctx.Next())
	scs := headerValues(rec.Headers, "set-cookie")
	if len(scs) != 1 || !strings.Contains(scs[0], "; Secure") {
		t.Fatalf("Set-Cookie under https: %q, want one cookie with Secure", scs)
	}
}

// A read-only request must stay cookieless: reading never emits.
func TestReadOnlyRequestWithBodyEmitsNothing(t *testing.T) {
	mw := New(Config{Store: NewMemoryStore()})
	h := func(c *celeris.Context) error {
		s := FromContext(c)
		_ = s.GetString("user")
		_, _ = s.Get("x")
		_ = s.Keys()
		return c.JSON(401, "unauthenticated")
	}
	ctx, rec := celeristest.NewContextT(t, "GET", "/me", celeristest.WithHandlers(mw, h))
	testutil.AssertNoError(t, ctx.Next())
	if scs := headerValues(rec.Headers, "set-cookie"); len(scs) != 0 {
		t.Fatalf("read-only request emitted Set-Cookie %q", scs)
	}
	if scs := headerValues(ctx.ResponseHeaders(), "set-cookie"); len(scs) != 0 {
		t.Fatalf("read-only request queued Set-Cookie %q", scs)
	}
}
