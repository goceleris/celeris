package session

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/store"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// setFailStore loads and deletes through inner but fails every Set, so a
// session can be LOADED from a valid cookie and still hit the failed-save
// path (failStore cannot load anything).
type setFailStore struct {
	inner store.KV
	err   error
}

func (s *setFailStore) Get(ctx context.Context, id string) ([]byte, error) {
	return s.inner.Get(ctx, id)
}

func (s *setFailStore) Set(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return s.err
}

func (s *setFailStore) Delete(ctx context.Context, id string) error {
	return s.inner.Delete(ctx, id)
}

// The tests in this file pin what the FAILED response carries when the
// post-chain save fails. Before the cookie moved to the first mutation, a
// failed store.Set / EncodeJSON returned through ErrorHandler with no
// Set-Cookie at all. Early emission puts the cookie on the response before
// the save runs, so the middleware must take it back when the id it carries
// was never persisted — and leave it when the client already holds that id.

// A fresh session whose save fails must not hand the client an id the store
// never received: the error response carries no session cookie, no drop is
// counted, and the store stays empty.
func TestFailedSaveRetractsFreshSessionCookie(t *testing.T) {
	saveErr := errors.New("boom: store down")
	shapes := []struct {
		name    string
		handler func(c *celeris.Context, s *Session) error
	}{
		{"set-no-body", func(_ *celeris.Context, s *Session) error { s.Set("k", "v"); return nil }},
		{"set-then-chain-error", func(_ *celeris.Context, s *Session) error {
			s.Set("k", "v")
			return errors.New("handler failed")
		}},
		{"regenerate-no-body", func(_ *celeris.Context, s *Session) error { return s.Regenerate() }},
	}
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			var gotErr error
			mw := New(Config{
				Store: &setFailStore{inner: NewMemoryStore(), err: saveErr},
				ErrorHandler: func(_ *celeris.Context, err error) error {
					gotErr = err
					return err
				},
			})
			before := DroppedCookies()
			h := func(c *celeris.Context) error { return sh.handler(c, FromContext(c)) }
			ctx, _ := celeristest.NewContextT(t, "POST", "/", celeristest.WithHandlers(mw, h))
			err := ctx.Next()
			if !errors.Is(err, saveErr) || !errors.Is(gotErr, saveErr) {
				t.Fatalf("save error not surfaced: chain=%v handler=%v", err, gotErr)
			}
			if scs := headerValues(ctx.ResponseHeaders(), "set-cookie"); len(scs) != 0 {
				t.Fatalf("failed save left Set-Cookie %q on the error response", scs)
			}
			if got := DroppedCookies() - before; got != 0 {
				t.Fatalf("DroppedCookies advanced by %d on a retraction, want 0", got)
			}
		})
	}

	// SaveUnmodified emits the fresh cookie before the handler runs; the
	// failed save must take that one back too.
	t.Run("saveunmodified-untouched", func(t *testing.T) {
		mw := New(Config{Store: &setFailStore{inner: NewMemoryStore(), err: saveErr}, SaveUnmodified: true})
		h := func(_ *celeris.Context) error { return nil }
		ctx, _ := celeristest.NewContextT(t, "GET", "/", celeristest.WithHandlers(mw, h))
		if err := ctx.Next(); !errors.Is(err, saveErr) {
			t.Fatalf("chain error %v, want %v", err, saveErr)
		}
		if scs := headerValues(ctx.ResponseHeaders(), "set-cookie"); len(scs) != 0 {
			t.Fatalf("failed save left Set-Cookie %q on the error response", scs)
		}
	})

	// Header-extractor mode: the session-id response header is retracted
	// the same way.
	t.Run("header-extractor", func(t *testing.T) {
		mw := New(Config{
			Store:     &setFailStore{inner: NewMemoryStore(), err: saveErr},
			Extractor: HeaderExtractor("X-Session-ID"),
		})
		h := func(c *celeris.Context) error {
			FromContext(c).Set("k", "v")
			c.SetHeader("X-Other", "kept")
			return nil
		}
		ctx, _ := celeristest.NewContextT(t, "POST", "/", celeristest.WithHandlers(mw, h))
		if err := ctx.Next(); !errors.Is(err, saveErr) {
			t.Fatalf("chain error %v, want %v", err, saveErr)
		}
		if got := headerValues(ctx.ResponseHeaders(), "celeris_session"); len(got) != 0 {
			t.Fatalf("failed save left session-id header %q on the error response", got)
		}
		if got := headerValues(ctx.ResponseHeaders(), "x-other"); len(got) != 1 || got[0] != "kept" {
			t.Fatalf("retraction disturbed an unrelated header: %v", ctx.ResponseHeaders())
		}
	})
}

// An EncodeJSON failure is the same story: nothing was persisted, so the
// cookie comes off the error response.
func TestFailedEncodeRetractsFreshSessionCookie(t *testing.T) {
	mw := New(Config{Store: NewMemoryStore()})
	h := func(c *celeris.Context) error {
		FromContext(c).Set("bad", make(chan int)) // json: unsupported type
		return nil
	}
	ctx, _ := celeristest.NewContextT(t, "POST", "/", celeristest.WithHandlers(mw, h))
	if err := ctx.Next(); err == nil {
		t.Fatal("expected an encode error from the post-chain save")
	}
	if scs := headerValues(ctx.ResponseHeaders(), "set-cookie"); len(scs) != 0 {
		t.Fatalf("failed encode left Set-Cookie %q on the error response", scs)
	}
}

// A LOADED session whose save fails keeps its cookie only while the cookie
// still names the id the client arrived with (a pure Max-Age refresh of a
// valid id); after Regenerate the new id was never persisted, so it is
// retracted and the client keeps whatever it held.
func TestFailedSaveKeepsLoadedSessionCookieOnlyForPresentedID(t *testing.T) {
	saveErr := errors.New("boom: store down")
	sid := hexID(0x4d)

	newMW := func(t *testing.T) (celeris.HandlerFunc, store.KV) {
		t.Helper()
		inner := NewMemoryStore()
		saveMap(t, inner, sid, map[string]any{"user": "alice", absExpKey: nowNano()}, 0)
		return New(Config{Store: &setFailStore{inner: inner, err: saveErr}}), inner
	}

	t.Run("refresh-kept", func(t *testing.T) {
		mw, inner := newMW(t)
		h := func(c *celeris.Context) error {
			FromContext(c).Set("last_seen", "now")
			return nil
		}
		ctx, _ := celeristest.NewContextT(t, "POST", "/touch",
			celeristest.WithCookie("celeris_session", sid),
			celeristest.WithHandlers(mw, h))
		if err := ctx.Next(); !errors.Is(err, saveErr) {
			t.Fatalf("chain error %v, want %v", err, saveErr)
		}
		// The id is still valid server-side, so the refresh cookie stays.
		assertSingleSessionCookie(t, ctx.ResponseHeaders(), sid)
		if data, ok := loadMap(t, inner, sid); !ok || data["user"] != "alice" {
			t.Fatalf("loaded session %s changed on a failed save: %v (ok=%v)", sid, data, ok)
		}
	})

	t.Run("regenerate-retracted", func(t *testing.T) {
		mw, _ := newMW(t)
		var newID string
		h := func(c *celeris.Context) error {
			s := FromContext(c)
			if err := s.Regenerate(); err != nil {
				return err
			}
			newID = s.ID()
			return nil
		}
		ctx, _ := celeristest.NewContextT(t, "POST", "/login",
			celeristest.WithCookie("celeris_session", sid),
			celeristest.WithHandlers(mw, h))
		if err := ctx.Next(); !errors.Is(err, saveErr) {
			t.Fatalf("chain error %v, want %v", err, saveErr)
		}
		if newID == "" || newID == sid {
			t.Fatalf("Regenerate did not rotate the id: %q", newID)
		}
		if scs := headerValues(ctx.ResponseHeaders(), "set-cookie"); len(scs) != 0 {
			t.Fatalf("failed save left Set-Cookie %q for the never-persisted id %s", scs, newID)
		}
	})
}

// Write-behind cannot fail synchronously: enqueue always succeeds and the
// store error surfaces later through ErrorHandler (with a nil Context). The
// cookie therefore stays on the response — the request succeeded from the
// client's point of view — which is the documented "fire and forget" trade.
func TestWriteBehindFailedSaveKeepsCookie(t *testing.T) {
	saveErr := errors.New("boom: store down")
	var deferred atomic.Int64
	mw, closer := NewWithCloser(Config{
		Store:       &setFailStore{inner: NewMemoryStore(), err: saveErr},
		WriteBehind: true,
		ErrorHandler: func(c *celeris.Context, err error) error {
			if c == nil && errors.Is(err, saveErr) {
				deferred.Add(1)
			}
			return err
		},
	})
	var sid string
	h := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("k", "v")
		sid = s.ID()
		return nil
	}
	ctx, _ := celeristest.NewContextT(t, "POST", "/", celeristest.WithHandlers(mw, h))
	testutil.AssertNoError(t, ctx.Next())
	assertSingleSessionCookie(t, ctx.ResponseHeaders(), sid)
	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if deferred.Load() != 1 {
		t.Fatalf("deferred failures reaching ErrorHandler: %d, want 1", deferred.Load())
	}
}
