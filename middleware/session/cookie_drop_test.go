package session

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// (f) A mutation after the body was written cannot set a cookie any more.
// It must not panic, must not queue a header the engine would drop anyway,
// must count exactly once per request, and must still persist the data
// (the post-chain save is unchanged).
func TestMutationAfterBodyWrittenDropsCookieOnce(t *testing.T) {
	kv := NewMemoryStore()
	mw := New(Config{Store: kv})
	before := DroppedCookies()

	var sid string
	h := func(c *celeris.Context) error {
		if err := c.JSON(200, map[string]string{"ok": "1"}); err != nil {
			return err
		}
		s := FromContext(c)
		s.Set("user", "late") // too late: the body is already on the wire
		s.Set("other", "x")   // a second mutation must not double count
		if err := s.Save(); err != nil {
			return err
		}
		sid = s.ID()
		return nil
	}
	ctx, rec := celeristest.NewContextT(t, "POST", "/late", celeristest.WithHandlers(mw, h))
	testutil.AssertNoError(t, ctx.Next())

	if scs := headerValues(rec.Headers, "set-cookie"); len(scs) != 0 {
		t.Fatalf("Set-Cookie reached the wire after the body was written: %q", scs)
	}
	if scs := headerValues(ctx.ResponseHeaders(), "set-cookie"); len(scs) != 0 {
		t.Fatalf("post-chain path queued an undeliverable Set-Cookie %q", scs)
	}
	if got := DroppedCookies() - before; got != 1 {
		t.Fatalf("DroppedCookies advanced by %d, want exactly 1", got)
	}
	if data, ok := loadMap(t, kv, sid); !ok || data["user"] != "late" {
		t.Fatalf("post-chain persistence changed: %v (ok=%v)", data, ok)
	}

	// Destroy after the write is the same story: no clearing cookie, one
	// count, store entry gone.
	sid2 := hexID(0x3c)
	saveMap(t, kv, sid2, map[string]any{"user": "alice", absExpKey: nowNano()}, 0)
	before = DroppedCookies()
	logout := func(c *celeris.Context) error {
		if err := c.NoContent(204); err != nil {
			return err
		}
		return FromContext(c).Destroy()
	}
	ctx2, rec2 := celeristest.NewContextT(t, "POST", "/logout",
		celeristest.WithCookie("celeris_session", sid2),
		celeristest.WithHandlers(mw, logout))
	testutil.AssertNoError(t, ctx2.Next())
	if scs := headerValues(rec2.Headers, "set-cookie"); len(scs) != 0 {
		t.Fatalf("clearing cookie reached the wire after the body was written: %q", scs)
	}
	if got := DroppedCookies() - before; got != 1 {
		t.Fatalf("DroppedCookies advanced by %d after late Destroy, want exactly 1", got)
	}
	if _, ok := loadMap(t, kv, sid2); ok {
		t.Fatalf("destroyed session %s still in store", sid2)
	}

	// The pooled Session must come back clean: a following normal request
	// on the same middleware emits its cookie as usual.
	var sid3 string
	normal := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("user", "bob")
		sid3 = s.ID()
		return c.JSON(200, "ok")
	}
	ctx3, rec3 := celeristest.NewContextT(t, "POST", "/login", celeristest.WithHandlers(mw, normal))
	testutil.AssertNoError(t, ctx3.Next())
	assertSingleSessionCookie(t, rec3.Headers, sid3)
}

// A LOADED session mutated after the body was written is not a drop: the
// client already holds exactly the cookie the middleware would re-send and
// the data is persisted under that same id, so nothing is orphaned. Only a
// late change of id (Regenerate) or a late Destroy counts.
func TestLoadedSessionLateMutationIsNotADrop(t *testing.T) {
	kv := NewMemoryStore()
	mw := New(Config{Store: kv})
	sid := hexID(0x5e)
	saveMap(t, kv, sid, map[string]any{"user": "alice", absExpKey: nowNano()}, 0)

	before := DroppedCookies()
	touch := func(c *celeris.Context) error {
		if err := c.JSON(200, map[string]string{"user": "alice"}); err != nil {
			return err
		}
		s := FromContext(c)
		s.Set("last_seen", "now") // refresh-after-write: the client holds sid already
		s.SetIdleTimeout(0)
		return s.Save()
	}
	ctx, rec := celeristest.NewContextT(t, "GET", "/me",
		celeristest.WithCookie("celeris_session", sid),
		celeristest.WithHandlers(mw, touch))
	testutil.AssertNoError(t, ctx.Next())
	if scs := headerValues(rec.Headers, "set-cookie"); len(scs) != 0 {
		t.Fatalf("Set-Cookie reached the wire after the body was written: %q", scs)
	}
	if got := DroppedCookies() - before; got != 0 {
		t.Fatalf("DroppedCookies advanced by %d for a loaded-session refresh, want 0", got)
	}
	data, ok := loadMap(t, kv, sid)
	if !ok || data["last_seen"] != "now" || data["user"] != "alice" {
		t.Fatalf("late mutation not persisted under the presented id %s: %v (ok=%v)", sid, data, ok)
	}

	// Regenerate after the write changes the id the client should hold:
	// that IS a drop (the client keeps a cookie for a deleted session).
	before = DroppedCookies()
	rotate := func(c *celeris.Context) error {
		if err := c.JSON(200, "ok"); err != nil {
			return err
		}
		return FromContext(c).Regenerate()
	}
	ctx2, _ := celeristest.NewContextT(t, "POST", "/login",
		celeristest.WithCookie("celeris_session", sid),
		celeristest.WithHandlers(mw, rotate))
	testutil.AssertNoError(t, ctx2.Next())
	if got := DroppedCookies() - before; got != 1 {
		t.Fatalf("DroppedCookies advanced by %d after a late Regenerate, want exactly 1", got)
	}
	if _, ok := loadMap(t, kv, sid); ok {
		t.Fatalf("old session %s still in store after Regenerate", sid)
	}
}
