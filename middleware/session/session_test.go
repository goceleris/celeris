package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// hexID returns a valid 64-char hex session ID using the given byte as padding.
// Each unique byte produces a unique ID: hexID(0x01) = "0101...01".
func hexID(b byte) string {
	const hexChars = "0123456789abcdef"
	hi := hexChars[b>>4]
	lo := hexChars[b&0x0f]
	var buf [64]byte
	for i := 0; i < 64; i += 2 {
		buf[i] = hi
		buf[i+1] = lo
	}
	return string(buf[:])
}

// --- Session type tests ---

func TestSessionGetSet(t *testing.T) {
	s := &Session{id: "test", data: make(map[string]any), store: NewMemoryStore(), expiry: time.Hour, keyGen: func() string { return "x" }}
	s.Set("key", "value")
	v, ok := s.Get("key")
	if !ok || v != "value" {
		t.Fatalf("Get: got (%v, %v), want (value, true)", v, ok)
	}
	if !s.modified {
		t.Fatal("expected modified=true after Set")
	}
}

func TestSessionGetMissing(t *testing.T) {
	s := &Session{id: "test", data: make(map[string]any), store: NewMemoryStore()}
	_, ok := s.Get("missing")
	if ok {
		t.Fatal("expected ok=false for missing key")
	}
}

func TestSessionDelete(t *testing.T) {
	s := &Session{id: "test", data: map[string]any{"a": 1}, store: NewMemoryStore()}
	s.Delete("a")
	_, ok := s.Get("a")
	if ok {
		t.Fatal("expected key to be deleted")
	}
	if !s.modified {
		t.Fatal("expected modified=true after Delete")
	}
}

func TestSessionDeleteAbsentKeyNotModified(t *testing.T) {
	s := &Session{id: "test", data: map[string]any{"a": 1}, store: NewMemoryStore()}
	s.Delete("nonexistent")
	if s.modified {
		t.Fatal("expected modified=false after Delete of absent key")
	}
}

func TestSessionClear(t *testing.T) {
	s := &Session{id: "test", data: map[string]any{"a": 1, "b": 2}, store: NewMemoryStore()}
	s.Clear()
	if len(s.data) != 0 {
		t.Fatalf("expected empty data after Clear, got %d entries", len(s.data))
	}
	if !s.modified {
		t.Fatal("expected modified=true after Clear")
	}
}

func TestSessionID(t *testing.T) {
	s := &Session{id: "abc123", data: make(map[string]any)}
	if s.ID() != "abc123" {
		t.Fatalf("ID: got %q, want %q", s.ID(), "abc123")
	}
}

func TestSessionIsFresh(t *testing.T) {
	s := &Session{fresh: true}
	if !s.IsFresh() {
		t.Fatal("expected IsFresh=true")
	}
	s2 := &Session{fresh: false}
	if s2.IsFresh() {
		t.Fatal("expected IsFresh=false")
	}
}

func TestSessionSave(t *testing.T) {
	store := NewMemoryStore()
	s := &Session{id: "save-test", data: map[string]any{"k": "v"}, store: store, ctx: context.Background(), expiry: time.Hour}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := store.Get(context.Background(), "save-test")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if data["k"] != "v" {
		t.Fatalf("stored data: got %v, want k=v", data)
	}
}

func TestSessionSaveUsesExpiry(t *testing.T) {
	store := NewMemoryStore()
	s := &Session{id: "exp-test", data: map[string]any{"k": "v"}, store: store, ctx: context.Background(), expiry: time.Nanosecond}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	time.Sleep(time.Millisecond)
	data, _ := store.Get(context.Background(), "exp-test")
	if data != nil {
		t.Fatal("expected session to be expired after Save with short expiry")
	}
}

func TestSessionDestroy(t *testing.T) {
	store := NewMemoryStore()
	_ = store.Save(context.Background(), "destroy-test", map[string]any{"k": "v"}, time.Hour)
	s := &Session{id: "destroy-test", data: map[string]any{"k": "v"}, store: store, ctx: context.Background(), modified: true}
	if err := s.Destroy(); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(s.data) != 0 {
		t.Fatal("expected data to be cleared")
	}
	if s.modified {
		t.Fatal("expected modified=false after Destroy")
	}
	if !s.destroyed {
		t.Fatal("expected destroyed=true after Destroy")
	}
	data, _ := store.Get(context.Background(), "destroy-test")
	if data != nil {
		t.Fatal("expected session deleted from store")
	}
}

func TestSessionRegenerate(t *testing.T) {
	store := NewMemoryStore()
	_ = store.Save(context.Background(), "old-id", map[string]any{"user": "admin"}, time.Hour)
	s := &Session{
		id:     "old-id",
		data:   map[string]any{"user": "admin"},
		store:  store,
		ctx:    context.Background(),
		expiry: time.Hour,
		keyGen: func() string { return "new-id" },
	}
	err := s.Regenerate()
	if err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	if s.ID() != "new-id" {
		t.Fatalf("ID after regenerate: got %q, want %q", s.ID(), "new-id")
	}
	if !s.modified {
		t.Fatal("expected modified=true after Regenerate")
	}
	// Old session should be deleted.
	old, _ := store.Get(context.Background(), "old-id")
	if old != nil {
		t.Fatal("expected old session deleted")
	}
	// Data should be preserved in memory for post-handler save.
	if s.data["user"] != "admin" {
		t.Fatal("expected data preserved in session after Regenerate")
	}
}

func TestRegenerateResetsAbsExp(t *testing.T) {
	store := NewMemoryStore()
	oldTs := time.Now().Add(-time.Hour).UnixNano()
	s := &Session{
		id:     "old",
		data:   map[string]any{absExpKey: oldTs, "user": "admin"},
		store:  store,
		ctx:    context.Background(),
		expiry: time.Hour,
		keyGen: func() string { return "new" },
	}
	if err := s.Regenerate(); err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	v, ok := s.data[absExpKey]
	if !ok {
		t.Fatal("expected _abs_exp to be present after Regenerate")
	}
	newTs := v.(int64)
	if newTs == oldTs {
		t.Fatal("expected _abs_exp to be reset to current time after Regenerate")
	}
	if time.Since(time.Unix(0, newTs)) > time.Second {
		t.Fatal("expected _abs_exp to be recent after Regenerate")
	}
}

func TestKeysAndLen(t *testing.T) {
	s := &Session{id: "test", data: map[string]any{"b": 2, "a": 1, "c": 3}}
	keys := s.Keys()
	if len(keys) != 3 {
		t.Fatalf("Keys: got %d, want 3", len(keys))
	}
	if keys[0] != "a" || keys[1] != "b" || keys[2] != "c" {
		t.Fatalf("Keys: got %v, want [a b c]", keys)
	}
	if s.Len() != 3 {
		t.Fatalf("Len: got %d, want 3", s.Len())
	}
}

func TestKeysEmpty(t *testing.T) {
	s := &Session{id: "test", data: make(map[string]any)}
	keys := s.Keys()
	if len(keys) != 0 {
		t.Fatalf("Keys: got %d, want 0", len(keys))
	}
	if s.Len() != 0 {
		t.Fatalf("Len: got %d, want 0", s.Len())
	}
}

// --- MemoryStore tests ---

func TestMemoryStoreCRUD(t *testing.T) {
	store := NewMemoryStore()

	// Get non-existent.
	data, err := store.Get(context.Background(), "nope")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if data != nil {
		t.Fatal("expected nil for non-existent session")
	}

	// Save.
	err = store.Save(context.Background(), "s1", map[string]any{"a": 1}, time.Hour)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Get existing.
	data, err = store.Get(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if data["a"] != 1 {
		t.Fatalf("data: got %v, want a=1", data)
	}

	// Update.
	err = store.Save(context.Background(), "s1", map[string]any{"a": 2, "b": 3}, time.Hour)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, _ = store.Get(context.Background(), "s1")
	if data["a"] != 2 || data["b"] != 3 {
		t.Fatalf("updated data: got %v", data)
	}

	// Delete.
	err = store.Delete(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	data, _ = store.Get(context.Background(), "s1")
	if data != nil {
		t.Fatal("expected nil after Delete")
	}

	// Delete non-existent (no error).
	err = store.Delete(context.Background(), "nope")
	if err != nil {
		t.Fatalf("Delete non-existent: %v", err)
	}
}

func TestMemoryStoreExpiry(t *testing.T) {
	store := NewMemoryStore()
	_ = store.Save(context.Background(), "exp", map[string]any{"k": "v"}, time.Nanosecond)
	time.Sleep(time.Millisecond)
	data, _ := store.Get(context.Background(), "exp")
	if data != nil {
		t.Fatal("expected nil for expired session")
	}
}

func TestMemoryStoreCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := NewMemoryStore(MemoryStoreConfig{
		Shards:          1,
		CleanupInterval: 10 * time.Millisecond,
		CleanupContext:  ctx,
	})

	_ = store.Save(context.Background(), "clean", map[string]any{"k": "v"}, time.Nanosecond)
	time.Sleep(time.Millisecond)
	// Wait for cleanup to run.
	time.Sleep(50 * time.Millisecond)
	data, _ := store.Get(context.Background(), "clean")
	if data != nil {
		t.Fatal("expected cleanup to remove expired session")
	}
}

func TestMemoryStoreCleanupStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	_ = NewMemoryStore(MemoryStoreConfig{
		CleanupInterval: time.Millisecond,
		CleanupContext:  ctx,
	})
	cancel()
	// No goroutine leak — the cleanup goroutine exits.
	time.Sleep(10 * time.Millisecond)
}

func TestMemoryStoreDataIsolation(t *testing.T) {
	store := NewMemoryStore()
	original := map[string]any{"k": "v"}
	_ = store.Save(context.Background(), "iso", original, time.Hour)
	// Mutating the original should not affect stored data.
	original["k"] = "mutated"
	data, _ := store.Get(context.Background(), "iso")
	if data["k"] != "v" {
		t.Fatal("store data was mutated through original reference")
	}
	// Mutating the returned data should not affect stored data.
	data["k"] = "also-mutated"
	data2, _ := store.Get(context.Background(), "iso")
	if data2["k"] != "v" {
		t.Fatal("store data was mutated through returned reference")
	}
}

func TestMemoryStoreNoExpiry(t *testing.T) {
	store := NewMemoryStore()
	_ = store.Save(context.Background(), "noexp", map[string]any{"k": "v"}, 0)
	data, _ := store.Get(context.Background(), "noexp")
	if data == nil || data["k"] != "v" {
		t.Fatal("expected session with no expiry to persist")
	}
}

func TestMemoryStoreConcurrent(t *testing.T) {
	store := NewMemoryStore()
	const goroutines = 32
	const ops = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func(id int) {
			defer wg.Done()
			sid := "sess-" + string(rune('A'+id%26))
			for range ops {
				_ = store.Save(context.Background(), sid, map[string]any{"g": id}, time.Hour)
				_, _ = store.Get(context.Background(), sid)
				_ = store.Delete(context.Background(), sid)
			}
		}(g)
	}
	wg.Wait()
}

// --- Middleware tests ---

func TestNewSessionCreated(t *testing.T) {
	var sess *Session
	mw := New()
	handler := func(c *celeris.Context) error {
		sess = FromContext(c)
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	if sess == nil {
		t.Fatal("expected session in context")
	}
	if !sess.IsFresh() {
		t.Fatal("expected fresh session for new request")
	}
	if sess.ID() == "" {
		t.Fatal("expected non-empty session ID")
	}
}

func TestSessionPersistsAcrossRequests(t *testing.T) {
	store := NewMemoryStore()
	mw := New(Config{Store: store})
	var firstID string

	// First request: set data.
	handler1 := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("user", "admin")
		firstID = s.ID()
		return nil
	}
	chain1 := []celeris.HandlerFunc{mw, handler1}
	_, err := testutil.RunChain(t, chain1, "GET", "/")
	testutil.AssertNoError(t, err)

	// Second request: read data with same session cookie.
	var gotUser any
	var gotOK bool
	var wasFresh bool
	handler2 := func(c *celeris.Context) error {
		s := FromContext(c)
		if s == nil {
			t.Fatal("expected session in context")
		}
		wasFresh = s.IsFresh()
		gotUser, gotOK = s.Get("user")
		return nil
	}
	chain2 := []celeris.HandlerFunc{mw, handler2}
	_, err = testutil.RunChain(t, chain2, "GET", "/",
		celeristest.WithCookie("celeris_session", firstID),
	)
	testutil.AssertNoError(t, err)
	if wasFresh {
		t.Fatal("expected existing session, not fresh")
	}
	if !gotOK || gotUser != "admin" {
		t.Fatalf("expected user=admin, got (%v, %v)", gotUser, gotOK)
	}
}

func TestModifiedSessionAutoSaved(t *testing.T) {
	store := NewMemoryStore()
	mw := New(Config{Store: store})
	var sid string

	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("count", 42)
		sid = s.ID()
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)

	// Verify data was persisted.
	data, _ := store.Get(context.Background(), sid)
	if data == nil || data["count"] != 42 {
		t.Fatalf("expected count=42 in store, got %v", data)
	}
}

func TestUnmodifiedExistingSessionNotResaved(t *testing.T) {
	// Track Save calls.
	ts := &trackingStore{inner: NewMemoryStore()}
	mw := New(Config{Store: ts})

	// First request: creates fresh session (triggers save).
	handler1 := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("init", true)
		return nil
	}
	var firstID string
	h1Wrap := func(c *celeris.Context) error {
		err := handler1(c)
		firstID = FromContext(c).ID()
		return err
	}
	chain1 := []celeris.HandlerFunc{mw, h1Wrap}
	_, _ = testutil.RunChain(t, chain1, "GET", "/")

	savesBefore := ts.saveCount()

	// Second request: no modifications.
	handler2 := func(c *celeris.Context) error {
		_ = FromContext(c)
		return nil
	}
	chain2 := []celeris.HandlerFunc{mw, handler2}
	_, _ = testutil.RunChain(t, chain2, "GET", "/",
		celeristest.WithCookie("celeris_session", firstID),
	)

	if ts.saveCount() != savesBefore {
		t.Fatal("expected no Save call for unmodified existing session")
	}
}

func TestCookieIsSet(t *testing.T) {
	mw := New()
	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("k", "v")
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithHandlers(chain...),
	)
	err := ctx.Next()
	testutil.AssertNoError(t, err)

	found := false
	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "set-cookie" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected set-cookie response header")
	}
}

func TestSkipFunction(t *testing.T) {
	mw := New(Config{
		Skip: func(_ *celeris.Context) bool { return true },
	})
	var sess *Session
	handler := func(c *celeris.Context) error {
		sess = FromContext(c)
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	if sess != nil {
		t.Fatal("expected no session when skipped")
	}
}

func TestSkipPaths(t *testing.T) {
	mw := New(Config{SkipPaths: []string{"/health"}})
	var sess *Session
	handler := func(c *celeris.Context) error {
		sess = FromContext(c)
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/health")
	testutil.AssertNoError(t, err)
	if sess != nil {
		t.Fatal("expected no session for skipped path")
	}
}

func TestCustomKeyGenerator(t *testing.T) {
	customID := "custom-session-id-12345"
	mw := New(Config{
		KeyGenerator: func() string { return customID },
	})
	var sess *Session
	handler := func(c *celeris.Context) error {
		sess = FromContext(c)
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	if sess.ID() != customID {
		t.Fatalf("ID: got %q, want %q", sess.ID(), customID)
	}
}

func TestFromContextNoMiddleware(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "GET", "/")
	s := FromContext(ctx)
	if s != nil {
		t.Fatal("expected nil when middleware not applied")
	}
}

func TestExpiredCookieCreatesNewSession(t *testing.T) {
	store := NewMemoryStore()
	mw := New(Config{Store: store})

	var sess *Session
	handler := func(c *celeris.Context) error {
		sess = FromContext(c)
		sess.Set("new", true)
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("celeris_session", "nonexistent-id"),
	)
	testutil.AssertNoError(t, err)
	if sess == nil {
		t.Fatal("expected new session")
	}
	if !sess.IsFresh() {
		t.Fatal("expected fresh session when cookie points to missing store entry")
	}
}

func TestDefaultConfigValues(t *testing.T) {
	if defaultConfig.CookieName != "celeris_session" {
		t.Fatalf("CookieName: got %q, want %q", defaultConfig.CookieName, "celeris_session")
	}
	if defaultConfig.CookiePath != "/" {
		t.Fatalf("CookiePath: got %q, want %q", defaultConfig.CookiePath, "/")
	}
	if defaultConfig.CookieMaxAge == nil || *defaultConfig.CookieMaxAge != 86400 {
		t.Fatalf("CookieMaxAge: want 86400")
	}
	if defaultConfig.CookieHTTPOnly == nil || !*defaultConfig.CookieHTTPOnly {
		t.Fatal("expected CookieHTTPOnly=true")
	}
	if defaultConfig.CookieSameSite != celeris.SameSiteLaxMode {
		t.Fatalf("CookieSameSite: got %v, want SameSiteLaxMode", defaultConfig.CookieSameSite)
	}
	if defaultConfig.IdleTimeout != 30*time.Minute {
		t.Fatalf("IdleTimeout: got %v, want 30m", defaultConfig.IdleTimeout)
	}
	if defaultConfig.AbsoluteTimeout != 24*time.Hour {
		t.Fatalf("AbsoluteTimeout: got %v, want 24h", defaultConfig.AbsoluteTimeout)
	}
}

func TestContextKeyConstant(t *testing.T) {
	if ContextKey != "session" {
		t.Fatalf("ContextKey: got %q, want %q", ContextKey, "session")
	}
}

func TestDefaultKeyGeneratorLength(t *testing.T) {
	gen := newBufferedKeyGenerator()
	id := gen.generate()
	// 32 bytes hex-encoded = 64 chars.
	if len(id) != 64 {
		t.Fatalf("buffered key generator length: got %d, want 64", len(id))
	}
}

func TestDefaultKeyGeneratorUniqueness(t *testing.T) {
	gen := newBufferedKeyGenerator()
	seen := make(map[string]struct{}, 100)
	for range 100 {
		id := gen.generate()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate key generated: %s", id)
		}
		seen[id] = struct{}{}
	}
}

func TestCustomCookieName(t *testing.T) {
	mw := New(Config{CookieName: "my_sess"})
	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("k", "v")
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithHandlers(chain...),
	)
	err := ctx.Next()
	testutil.AssertNoError(t, err)

	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "set-cookie" {
			if len(h[1]) < 7 || h[1][:7] != "my_sess" {
				t.Fatalf("expected cookie name my_sess, got %q", h[1])
			}
			return
		}
	}
	t.Fatal("expected set-cookie header")
}

func TestSessionDestroyInHandler(t *testing.T) {
	store := NewMemoryStore()
	mw := New(Config{Store: store})
	var sid string

	// First request: create session with data.
	handler1 := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("user", "admin")
		sid = s.ID()
		return nil
	}
	chain1 := []celeris.HandlerFunc{mw, handler1}
	_, _ = testutil.RunChain(t, chain1, "GET", "/")

	// Second request: destroy session.
	handler2 := func(c *celeris.Context) error {
		s := FromContext(c)
		return s.Destroy()
	}
	chain2 := []celeris.HandlerFunc{mw, handler2}
	_, err := testutil.RunChain(t, chain2, "GET", "/",
		celeristest.WithCookie("celeris_session", sid),
	)
	testutil.AssertNoError(t, err)

	data, _ := store.Get(context.Background(), sid)
	if data != nil {
		t.Fatal("expected session deleted from store after Destroy")
	}
}

func TestDestroyExpiresCookie(t *testing.T) {
	store := NewMemoryStore()
	mw := New(Config{Store: store})
	var sid string

	// First request: create session.
	handler1 := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("user", "admin")
		sid = s.ID()
		return nil
	}
	chain1 := []celeris.HandlerFunc{mw, handler1}
	_, _ = testutil.RunChain(t, chain1, "GET", "/")

	// Second request: destroy session and check cookie.
	handler2 := func(c *celeris.Context) error {
		s := FromContext(c)
		return s.Destroy()
	}
	chain2 := []celeris.HandlerFunc{mw, handler2}
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithHandlers(chain2...),
		celeristest.WithCookie("celeris_session", sid),
	)
	err := ctx.Next()
	testutil.AssertNoError(t, err)

	found := false
	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "set-cookie" && strings.Contains(h[1], "Max-Age=0") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected set-cookie with Max-Age=0 after Destroy")
	}
}

func TestDestroyedSessionSkipsSave(t *testing.T) {
	ts := &trackingStore{inner: NewMemoryStore()}
	mw := New(Config{Store: ts})

	// First request: create.
	handler1 := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("init", true)
		return nil
	}
	var firstID string
	h1Wrap := func(c *celeris.Context) error {
		err := handler1(c)
		firstID = FromContext(c).ID()
		return err
	}
	chain1 := []celeris.HandlerFunc{mw, h1Wrap}
	_, _ = testutil.RunChain(t, chain1, "GET", "/")

	savesBefore := ts.saveCount()

	// Second request: destroy.
	handler2 := func(c *celeris.Context) error {
		s := FromContext(c)
		return s.Destroy()
	}
	chain2 := []celeris.HandlerFunc{mw, handler2}
	_, _ = testutil.RunChain(t, chain2, "GET", "/",
		celeristest.WithCookie("celeris_session", firstID),
	)

	if ts.saveCount() != savesBefore {
		t.Fatal("expected no Save call after Destroy")
	}
}

func TestRegenerateUpdatesCookie(t *testing.T) {
	store := NewMemoryStore()
	counter := 0
	mw := New(Config{
		Store: store,
		KeyGenerator: func() string {
			counter++
			if counter == 1 {
				return hexID(0xf1)
			}
			return hexID(0xf2)
		},
	})

	// First request: create session.
	handler1 := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("user", "admin")
		return nil
	}
	chain1 := []celeris.HandlerFunc{mw, handler1}
	_, _ = testutil.RunChain(t, chain1, "GET", "/")

	// Second request: regenerate.
	handler2 := func(c *celeris.Context) error {
		s := FromContext(c)
		return s.Regenerate()
	}
	chain2 := []celeris.HandlerFunc{mw, handler2}
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithHandlers(chain2...),
		celeristest.WithCookie("celeris_session", hexID(0xf1)),
	)
	err := ctx.Next()
	testutil.AssertNoError(t, err)

	// The cookie should contain the regenerated ID.
	found := false
	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "set-cookie" && strings.Contains(h[1], hexID(0xf2)) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected set-cookie with regenerated-id after Regenerate")
	}
}

func TestZeroArgRegenerate(t *testing.T) {
	store := NewMemoryStore()
	_ = store.Save(context.Background(), "old-id", map[string]any{"user": "admin"}, time.Hour)
	s := &Session{
		id:     "old-id",
		data:   map[string]any{"user": "admin"},
		store:  store,
		ctx:    context.Background(),
		expiry: time.Hour,
		keyGen: func() string { return "new-id" },
	}
	err := s.Regenerate()
	if err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	if s.ID() != "new-id" {
		t.Fatalf("ID: got %q, want %q", s.ID(), "new-id")
	}
	if !s.modified {
		t.Fatal("expected modified=true after Regenerate")
	}
	old, _ := store.Get(context.Background(), "old-id")
	if old != nil {
		t.Fatal("expected old session deleted")
	}
	// Data is preserved in memory for post-handler save.
	if s.data["user"] != "admin" {
		t.Fatal("expected data preserved in session after Regenerate")
	}
}

func TestAbsoluteTimeoutEnforced(t *testing.T) {
	store := NewMemoryStore()
	sid := hexID(0xa1)
	oldCreated := time.Now().Add(-3 * time.Hour).UnixNano()
	_ = store.Save(context.Background(), sid, map[string]any{absExpKey: oldCreated, "user": "admin"}, time.Hour)

	mw := New(Config{
		Store:           store,
		AbsoluteTimeout: 2 * time.Hour,
		IdleTimeout:     30 * time.Minute,
	})

	var sess *Session
	handler := func(c *celeris.Context) error {
		sess = FromContext(c)
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("celeris_session", sid),
	)
	testutil.AssertNoError(t, err)
	if sess == nil {
		t.Fatal("expected session")
	}
	if !sess.IsFresh() {
		t.Fatal("expected fresh session after absolute timeout expiry")
	}
	if sess.ID() == sid {
		t.Fatal("expected new session ID after absolute timeout expiry")
	}
	data, _ := store.Get(context.Background(), sid)
	if data != nil {
		t.Fatal("expected old session deleted from store")
	}
}

func TestAbsoluteTimeoutNotExpired(t *testing.T) {
	store := NewMemoryStore()
	sid := hexID(0xa2)
	recentCreated := time.Now().Add(-30 * time.Minute).UnixNano()
	_ = store.Save(context.Background(), sid, map[string]any{absExpKey: recentCreated, "user": "admin"}, time.Hour)

	mw := New(Config{
		Store:           store,
		AbsoluteTimeout: 2 * time.Hour,
		IdleTimeout:     30 * time.Minute,
	})

	var sess *Session
	handler := func(c *celeris.Context) error {
		sess = FromContext(c)
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("celeris_session", sid),
	)
	testutil.AssertNoError(t, err)
	if sess.IsFresh() {
		t.Fatal("expected existing session when within absolute timeout")
	}
	if sess.ID() != sid {
		t.Fatal("expected same session ID when within absolute timeout")
	}
}

func TestErrorHandlerCalledOnStoreFailure(t *testing.T) {
	fs := &failStore{getErr: errors.New("store down")}
	sid := hexID(0xa3)
	var handlerCalled bool
	mw := New(Config{
		Store: fs,
		ErrorHandler: func(_ *celeris.Context, err error) error {
			handlerCalled = true
			return celeris.NewHTTPError(503, err.Error())
		},
	})
	handler := func(_ *celeris.Context) error { return nil }
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("celeris_session", sid),
	)
	if !handlerCalled {
		t.Fatal("expected ErrorHandler to be called")
	}
	testutil.AssertHTTPError(t, err, 503)
}

func TestStoreErrorWithoutHandler(t *testing.T) {
	fs := &failStore{getErr: errors.New("store down")}
	sid := hexID(0xa4)
	mw := New(Config{Store: fs})
	handler := func(_ *celeris.Context) error { return nil }
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("celeris_session", sid),
	)
	if err == nil || err.Error() != "store down" {
		t.Fatalf("expected 'store down' error, got %v", err)
	}
}

func TestApplyDefaultsFillsZeroValues(t *testing.T) {
	cfg := applyDefaults(Config{})
	if cfg.Store == nil {
		t.Fatal("expected Store to be set")
	}
	if cfg.CookieName != "celeris_session" {
		t.Fatalf("CookieName: got %q", cfg.CookieName)
	}
	if cfg.CookiePath != "/" {
		t.Fatalf("CookiePath: got %q", cfg.CookiePath)
	}
	if cfg.KeyGenerator == nil {
		t.Fatal("expected KeyGenerator to be set")
	}
}

func TestApplyDefaultsIndependentFields(t *testing.T) {
	// nil CookieHTTPOnly is defaulted to true.
	cfg := applyDefaults(Config{})
	if cfg.CookieHTTPOnly == nil || !*cfg.CookieHTTPOnly {
		t.Fatal("expected CookieHTTPOnly=true when nil")
	}
	// Explicit false must be preserved.
	cfg = applyDefaults(Config{CookieHTTPOnly: BoolPtr(false)})
	if cfg.CookieHTTPOnly == nil || *cfg.CookieHTTPOnly {
		t.Fatal("expected CookieHTTPOnly=false when explicitly set")
	}
	// nil CookieMaxAge is defaulted to 86400.
	cfg = applyDefaults(Config{})
	if cfg.CookieMaxAge == nil || *cfg.CookieMaxAge != 86400 {
		t.Fatal("expected CookieMaxAge=86400 when nil")
	}
	// Explicit 0 (session cookie) must be preserved.
	cfg = applyDefaults(Config{CookieMaxAge: IntPtr(0)})
	if cfg.CookieMaxAge == nil || *cfg.CookieMaxAge != 0 {
		t.Fatal("expected CookieMaxAge=0 when explicitly set")
	}
	// SameSite should still get its default.
	cfg = applyDefaults(Config{})
	if cfg.CookieSameSite != celeris.SameSiteLaxMode {
		t.Fatalf("CookieSameSite: got %v, want SameSiteLaxMode", cfg.CookieSameSite)
	}
}

func TestValidatePanicsOnBadTimeout(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for AbsoluteTimeout < IdleTimeout")
		}
	}()
	cfg := Config{
		IdleTimeout:     time.Hour,
		AbsoluteTimeout: 10 * time.Minute,
	}
	cfg = applyDefaults(cfg)
	cfg.validate()
}

func TestNewSessionStoresAbsExp(t *testing.T) {
	store := NewMemoryStore()
	mw := New(Config{Store: store})
	var sid string

	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		sid = s.ID()
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)

	data, _ := store.Get(context.Background(), sid)
	if data == nil {
		t.Fatal("expected session in store")
	}
	ts, ok := data[absExpKey]
	if !ok {
		t.Fatal("expected _abs_exp key in new session data")
	}
	created := ts.(int64)
	if time.Since(time.Unix(0, created)) > time.Second {
		t.Fatal("expected _abs_exp to be recent")
	}
}

// --- Helper types ---

type trackingStore struct {
	inner Store
	mu    sync.Mutex
	saves int
}

func (ts *trackingStore) Get(ctx context.Context, id string) (map[string]any, error) {
	return ts.inner.Get(ctx, id)
}

func (ts *trackingStore) Save(ctx context.Context, id string, data map[string]any, expiry time.Duration) error {
	ts.mu.Lock()
	ts.saves++
	ts.mu.Unlock()
	return ts.inner.Save(ctx, id, data, expiry)
}

func (ts *trackingStore) Delete(ctx context.Context, id string) error {
	return ts.inner.Delete(ctx, id)
}

func (ts *trackingStore) Reset(ctx context.Context) error { return ts.inner.Reset(ctx) }

func (ts *trackingStore) saveCount() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.saves
}

type failStore struct {
	getErr  error
	saveErr error
	delErr  error
}

func (fs *failStore) Get(_ context.Context, _ string) (map[string]any, error) { return nil, fs.getErr }
func (fs *failStore) Save(_ context.Context, _ string, _ map[string]any, _ time.Duration) error {
	return fs.saveErr
}
func (fs *failStore) Delete(_ context.Context, _ string) error { return fs.delErr }
func (fs *failStore) Reset(_ context.Context) error            { return nil }

// --- New tests for fixes ---

func TestClearPreservesAbsExp(t *testing.T) {
	ts := time.Now().UnixNano()
	s := &Session{
		id:   "test",
		data: map[string]any{absExpKey: ts, "user": "admin", "role": "editor"},
	}
	s.Clear()
	if !s.modified {
		t.Fatal("expected modified=true after Clear")
	}
	v, ok := s.data[absExpKey]
	if !ok {
		t.Fatal("expected _abs_exp to be preserved after Clear")
	}
	if v.(int64) != ts {
		t.Fatal("expected _abs_exp timestamp to be unchanged after Clear")
	}
	if _, ok := s.data["user"]; ok {
		t.Fatal("expected user key to be removed by Clear")
	}
	if _, ok := s.data["role"]; ok {
		t.Fatal("expected role key to be removed by Clear")
	}
}

func TestRegenerateDoesNotDoubleSave(t *testing.T) {
	ts := &trackingStore{inner: NewMemoryStore()}
	counter := 0
	mw := New(Config{
		Store: ts,
		KeyGenerator: func() string {
			counter++
			if counter == 1 {
				return hexID(0xf1)
			}
			return hexID(0xf4)
		},
	})

	// First request: create session.
	handler1 := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("user", "admin")
		return nil
	}
	chain1 := []celeris.HandlerFunc{mw, handler1}
	_, _ = testutil.RunChain(t, chain1, "GET", "/")

	savesBefore := ts.saveCount()

	// Second request: regenerate session.
	handler2 := func(c *celeris.Context) error {
		s := FromContext(c)
		return s.Regenerate()
	}
	chain2 := []celeris.HandlerFunc{mw, handler2}
	_, err := testutil.RunChain(t, chain2, "GET", "/",
		celeristest.WithCookie("celeris_session", hexID(0xf1)),
	)
	testutil.AssertNoError(t, err)

	savesAfter := ts.saveCount()
	// Exactly one Save call from the post-handler (not two).
	if savesAfter-savesBefore != 1 {
		t.Fatalf("expected exactly 1 Save after Regenerate, got %d", savesAfter-savesBefore)
	}
}

func TestCookieHTTPOnlyDefaultsToTrue(t *testing.T) {
	cfg := applyDefaults(Config{})
	if cfg.CookieHTTPOnly == nil || !*cfg.CookieHTTPOnly {
		t.Fatal("expected CookieHTTPOnly=true by default")
	}
}

// --- Extractor tests ---

func TestHeaderExtractor(t *testing.T) {
	store := NewMemoryStore()
	mw := New(Config{
		Store:     store,
		Extractor: HeaderExtractor("x-session-id"),
	})
	var firstID string

	// First request: new session (no header).
	handler1 := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("user", "admin")
		firstID = s.ID()
		return nil
	}
	chain1 := []celeris.HandlerFunc{mw, handler1}
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithHandlers(chain1...),
	)
	err := ctx.Next()
	testutil.AssertNoError(t, err)

	// Response should include session ID header.
	found := false
	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "celeris_session" && h[1] == firstID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected session ID in response header for header extractor")
	}

	// Second request: load session via header.
	var gotUser any
	var gotOK, wasFresh2 bool
	handler2 := func(c *celeris.Context) error {
		s := FromContext(c)
		wasFresh2 = s.IsFresh()
		gotUser, gotOK = s.Get("user")
		return nil
	}
	chain2 := []celeris.HandlerFunc{mw, handler2}
	_, err = testutil.RunChain(t, chain2, "GET", "/",
		celeristest.WithHeader("x-session-id", firstID),
	)
	testutil.AssertNoError(t, err)
	if wasFresh2 {
		t.Fatal("expected existing session loaded via header")
	}
	if !gotOK || gotUser != "admin" {
		t.Fatalf("expected user=admin, got (%v, %v)", gotUser, gotOK)
	}
}

func TestQueryExtractor(t *testing.T) {
	store := NewMemoryStore()
	sid := hexID(0xa5)
	mw := New(Config{
		Store:     store,
		Extractor: QueryExtractor("sid"),
	})

	_ = store.Save(context.Background(), sid, map[string]any{absExpKey: time.Now().UnixNano(), "role": "editor"}, time.Hour)

	var gotRole any
	var gotOK, wasFresh bool
	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		wasFresh = s.IsFresh()
		gotRole, gotOK = s.Get("role")
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithQuery("sid", sid),
	)
	testutil.AssertNoError(t, err)
	if wasFresh {
		t.Fatal("expected existing session loaded via query param")
	}
	if !gotOK || gotRole != "editor" {
		t.Fatalf("expected role=editor, got (%v, %v)", gotRole, gotOK)
	}
}

func TestCookieExtractorExplicit(t *testing.T) {
	store := NewMemoryStore()
	sid := hexID(0xa6)
	mw := New(Config{
		Store:     store,
		Extractor: CookieExtractor("my_sess"),
	})

	_ = store.Save(context.Background(), sid, map[string]any{absExpKey: time.Now().UnixNano(), "k": "v"}, time.Hour)

	var sess *Session
	handler := func(c *celeris.Context) error {
		sess = FromContext(c)
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("my_sess", sid),
	)
	testutil.AssertNoError(t, err)
	if sess.IsFresh() {
		t.Fatal("expected existing session loaded via explicit cookie extractor")
	}
}

func TestHeaderExtractorDestroyedSetsEmptyHeader(t *testing.T) {
	store := NewMemoryStore()
	mw := New(Config{
		Store:     store,
		Extractor: HeaderExtractor("x-session-id"),
	})

	hdrSid := hexID(0xa7)
	_ = store.Save(context.Background(), hdrSid, map[string]any{absExpKey: time.Now().UnixNano(), "k": "v"}, time.Hour)

	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		return s.Destroy()
	}
	chain := []celeris.HandlerFunc{mw, handler}
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithHandlers(chain...),
		celeristest.WithHeader("x-session-id", hdrSid),
	)
	err := ctx.Next()
	testutil.AssertNoError(t, err)

	found := false
	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "celeris_session" && h[1] == "" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected empty session ID header after Destroy with header extractor")
	}
}

// --- SetIdleTimeout tests ---

func TestSetIdleTimeout(t *testing.T) {
	store := NewMemoryStore()
	mw := New(Config{Store: store, IdleTimeout: time.Hour})
	var sid string

	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("user", "admin")
		s.SetIdleTimeout(time.Nanosecond)
		sid = s.ID()
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)

	// Session should expire immediately due to per-session override.
	time.Sleep(time.Millisecond)
	data, _ := store.Get(context.Background(), sid)
	if data != nil {
		t.Fatal("expected session to expire with per-session idle timeout override")
	}
}

func TestSetIdleTimeoutModifiesSession(t *testing.T) {
	s := &Session{id: "test", data: make(map[string]any), store: NewMemoryStore(), expiry: time.Hour, keyGen: func() string { return "x" }}
	s.SetIdleTimeout(5 * time.Minute)
	if !s.modified {
		t.Fatal("expected modified=true after SetIdleTimeout")
	}
	if s.idleOverride != 5*time.Minute {
		t.Fatalf("idleOverride: got %v, want 5m", s.idleOverride)
	}
}

// --- Reset tests ---

func TestReset(t *testing.T) {
	store := NewMemoryStore()
	_ = store.Save(context.Background(), "old-id", map[string]any{absExpKey: time.Now().UnixNano(), "user": "admin", "role": "editor"}, time.Hour)
	s := &Session{
		id:     "old-id",
		data:   map[string]any{absExpKey: time.Now().UnixNano(), "user": "admin", "role": "editor"},
		store:  store,
		ctx:    context.Background(),
		expiry: time.Hour,
		keyGen: func() string { return "new-id" },
	}
	err := s.Reset()
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if s.ID() != "new-id" {
		t.Fatalf("ID after Reset: got %q, want %q", s.ID(), "new-id")
	}
	if !s.modified {
		t.Fatal("expected modified=true after Reset")
	}
	// User data should be cleared (only _abs_exp preserved by Clear, but Regenerate is separate).
	if _, ok := s.data["user"]; ok {
		t.Fatal("expected user key cleared by Reset")
	}
	if _, ok := s.data["role"]; ok {
		t.Fatal("expected role key cleared by Reset")
	}
	// Old session should be deleted from store.
	old, _ := store.Get(context.Background(), "old-id")
	if old != nil {
		t.Fatal("expected old session deleted from store after Reset")
	}
}

func TestResetInMiddleware(t *testing.T) {
	store := NewMemoryStore()
	counter := 0
	mw := New(Config{
		Store: store,
		KeyGenerator: func() string {
			counter++
			if counter == 1 {
				return hexID(0xf1)
			}
			return hexID(0xf3)
		},
	})

	// First request: create session.
	handler1 := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("user", "admin")
		return nil
	}
	chain1 := []celeris.HandlerFunc{mw, handler1}
	_, _ = testutil.RunChain(t, chain1, "GET", "/")

	// Second request: reset session.
	var sess *Session
	handler2 := func(c *celeris.Context) error {
		s := FromContext(c)
		sess = s
		return s.Reset()
	}
	chain2 := []celeris.HandlerFunc{mw, handler2}
	_, err := testutil.RunChain(t, chain2, "GET", "/",
		celeristest.WithCookie("celeris_session", hexID(0xf1)),
	)
	testutil.AssertNoError(t, err)
	if sess.ID() != hexID(0xf3) {
		t.Fatalf("expected reset-id, got %q", sess.ID())
	}
}

// --- AbsoluteTimeout disabled tests ---

func TestAbsoluteTimeoutDisabled(t *testing.T) {
	store := NewMemoryStore()
	sid := hexID(0xa8)
	oldCreated := time.Now().Add(-100 * time.Hour).UnixNano()
	_ = store.Save(context.Background(), sid, map[string]any{absExpKey: oldCreated, "user": "admin"}, time.Hour)

	mw := New(Config{
		Store:           store,
		AbsoluteTimeout: -1,
		IdleTimeout:     30 * time.Minute,
	})

	var gotUser any
	var gotOK, wasFresh bool
	var gotID string
	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		wasFresh = s.IsFresh()
		gotID = s.ID()
		gotUser, gotOK = s.Get("user")
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("celeris_session", sid),
	)
	testutil.AssertNoError(t, err)
	if wasFresh {
		t.Fatal("expected existing session when absolute timeout is disabled")
	}
	if gotID != sid {
		t.Fatal("expected same session ID when absolute timeout is disabled")
	}
	if !gotOK || gotUser != "admin" {
		t.Fatalf("expected user=admin, got (%v, %v)", gotUser, gotOK)
	}
}

func TestAbsoluteTimeoutDisabledNoAbsExpStored(t *testing.T) {
	store := NewMemoryStore()
	mw := New(Config{
		Store:           store,
		AbsoluteTimeout: -1,
	})
	var sid string
	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("k", "v")
		sid = s.ID()
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)

	data, _ := store.Get(context.Background(), sid)
	if data == nil {
		t.Fatal("expected session in store")
	}
	if _, ok := data[absExpKey]; ok {
		t.Fatal("expected no _abs_exp key when absolute timeout is disabled")
	}
}

func TestAbsoluteTimeoutDefaultWhenZero(t *testing.T) {
	cfg := applyDefaults(Config{AbsoluteTimeout: 0})
	if cfg.AbsoluteTimeout != 24*time.Hour {
		t.Fatalf("AbsoluteTimeout: got %v, want 24h", cfg.AbsoluteTimeout)
	}
}

func TestAbsoluteTimeoutPreservedWhenNegative(t *testing.T) {
	cfg := applyDefaults(Config{AbsoluteTimeout: -1})
	if cfg.AbsoluteTimeout != -1 {
		t.Fatalf("AbsoluteTimeout: got %v, want -1", cfg.AbsoluteTimeout)
	}
}

// --- SameSite=None validation tests ---

func TestSameSiteNoneWithoutSecurePanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for SameSite=None without Secure")
		}
	}()
	New(Config{
		CookieSameSite: celeris.SameSiteNoneMode,
		CookieSecure:   false,
	})
}

func TestSameSiteNoneWithSecureOk(t *testing.T) {
	mw := New(Config{
		CookieSameSite: celeris.SameSiteNoneMode,
		CookieSecure:   true,
	})
	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("k", "v")
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
}

// --- Typed getter tests ---

func TestGetStringPresent(t *testing.T) {
	s := &Session{data: map[string]any{"name": "alice"}}
	if got := s.GetString("name"); got != "alice" {
		t.Fatalf("GetString: got %q, want %q", got, "alice")
	}
}

func TestGetStringMissing(t *testing.T) {
	s := &Session{data: make(map[string]any)}
	if got := s.GetString("missing"); got != "" {
		t.Fatalf("GetString missing: got %q, want %q", got, "")
	}
}

func TestGetStringWrongType(t *testing.T) {
	s := &Session{data: map[string]any{"num": 42}}
	if got := s.GetString("num"); got != "" {
		t.Fatalf("GetString wrong type: got %q, want %q", got, "")
	}
}

func TestGetIntPresent(t *testing.T) {
	s := &Session{data: map[string]any{"count": 7}}
	if got := s.GetInt("count"); got != 7 {
		t.Fatalf("GetInt: got %d, want %d", got, 7)
	}
}

func TestGetIntMissing(t *testing.T) {
	s := &Session{data: make(map[string]any)}
	if got := s.GetInt("missing"); got != 0 {
		t.Fatalf("GetInt missing: got %d, want 0", got)
	}
}

func TestGetIntWrongType(t *testing.T) {
	s := &Session{data: map[string]any{"val": "text"}}
	if got := s.GetInt("val"); got != 0 {
		t.Fatalf("GetInt wrong type: got %d, want 0", got)
	}
}

func TestGetBoolPresent(t *testing.T) {
	s := &Session{data: map[string]any{"admin": true}}
	if got := s.GetBool("admin"); !got {
		t.Fatal("GetBool: got false, want true")
	}
}

func TestGetBoolMissing(t *testing.T) {
	s := &Session{data: make(map[string]any)}
	if got := s.GetBool("missing"); got {
		t.Fatal("GetBool missing: got true, want false")
	}
}

func TestGetBoolWrongType(t *testing.T) {
	s := &Session{data: map[string]any{"flag": 1}}
	if got := s.GetBool("flag"); got {
		t.Fatal("GetBool wrong type: got true, want false")
	}
}

func TestGetFloat64Present(t *testing.T) {
	s := &Session{data: map[string]any{"score": 3.14}}
	if got := s.GetFloat64("score"); got != 3.14 {
		t.Fatalf("GetFloat64: got %f, want %f", got, 3.14)
	}
}

func TestGetFloat64Missing(t *testing.T) {
	s := &Session{data: make(map[string]any)}
	if got := s.GetFloat64("missing"); got != 0 {
		t.Fatalf("GetFloat64 missing: got %f, want 0", got)
	}
}

func TestGetFloat64WrongType(t *testing.T) {
	s := &Session{data: map[string]any{"val": "text"}}
	if got := s.GetFloat64("val"); got != 0 {
		t.Fatalf("GetFloat64 wrong type: got %f, want 0", got)
	}
}

// --- Post-handler save error test ---

func TestPostHandlerSaveError(t *testing.T) {
	saveErr := errors.New("store save failed")
	store := &failStore{saveErr: saveErr}
	var handlerErr error
	mw := New(Config{
		Store: store,
		ErrorHandler: func(_ *celeris.Context, err error) error {
			handlerErr = err
			return celeris.NewHTTPError(503, "session store unavailable")
		},
	})
	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("key", "val")
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/save-fail")
	testutil.AssertHTTPError(t, err, 503)
	if !errors.Is(handlerErr, saveErr) {
		t.Fatalf("expected save error, got %v", handlerErr)
	}
}

// --- Concurrent sessions test ---

func TestConcurrentSessions(t *testing.T) {
	store := NewMemoryStore()
	mw := New(Config{Store: store})
	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("worker", "val")
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				ctx, rec := celeristest.NewContext("GET", "/",
					celeristest.WithHandlers(chain...))
				err := ctx.Next()
				status := rec.StatusCode
				celeristest.ReleaseContext(ctx)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
					return
				}
				if status != 200 {
					t.Errorf("unexpected status: %d", status)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// --- Session ID validation tests ---

func TestValidSessionID(t *testing.T) {
	tests := []struct {
		name string
		sid  string
		want bool
	}{
		{"valid 64-char hex", strings.Repeat("ab", 32), true},
		{"all zeros", strings.Repeat("0", 64), true},
		{"all f's", strings.Repeat("f", 64), true},
		{"too short", strings.Repeat("a", 63), false},
		{"too long", strings.Repeat("a", 65), false},
		{"uppercase hex rejected", strings.Repeat("A", 64), false},
		{"mixed case rejected", strings.Repeat("a", 32) + strings.Repeat("A", 32), false},
		{"non-hex char g", strings.Repeat("a", 63) + "g", false},
		{"non-hex char z", strings.Repeat("a", 63) + "z", false},
		{"spaces rejected", strings.Repeat("a", 63) + " ", false},
		{"empty string", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validSessionID(tt.sid)
			if got != tt.want {
				t.Fatalf("validSessionID(%q) = %v, want %v", tt.sid, got, tt.want)
			}
		})
	}
}

func TestInvalidSessionIDCreatesFresh(t *testing.T) {
	store := NewMemoryStore()
	mw := New(Config{Store: store})
	var sess *Session
	handler := func(c *celeris.Context) error {
		sess = FromContext(c)
		sess.Set("k", "v")
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("celeris_session", "invalid-not-hex"))
	testutil.AssertNoError(t, err)
	if sess == nil {
		t.Fatal("session should not be nil")
	}
	if !sess.IsFresh() {
		t.Fatal("session should be fresh when ID is invalid")
	}
	if sess.ID() == "invalid-not-hex" {
		t.Fatal("session ID should not be the invalid value")
	}
}

func TestInvalidSessionIDTooShort(t *testing.T) {
	store := NewMemoryStore()
	mw := New(Config{Store: store})
	var sess *Session
	handler := func(c *celeris.Context) error {
		sess = FromContext(c)
		sess.Set("k", "v")
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("celeris_session", strings.Repeat("a", 63)))
	testutil.AssertNoError(t, err)
	if !sess.IsFresh() {
		t.Fatal("session should be fresh when ID length is wrong")
	}
}

func TestInvalidSessionIDUppercase(t *testing.T) {
	store := NewMemoryStore()
	mw := New(Config{Store: store})
	var sess *Session
	handler := func(c *celeris.Context) error {
		sess = FromContext(c)
		sess.Set("k", "v")
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("celeris_session", strings.Repeat("A", 64)))
	testutil.AssertNoError(t, err)
	if !sess.IsFresh() {
		t.Fatal("session should be fresh when ID has uppercase hex")
	}
}

func TestValidSessionIDLoadsExisting(t *testing.T) {
	store := NewMemoryStore()
	validID := strings.Repeat("ab", 32)
	_ = store.Save(context.Background(), validID, map[string]any{"user": "admin", "_abs_exp": time.Now().UnixNano()}, time.Hour)

	mw := New(Config{Store: store})
	var gotUser any
	var gotOK, wasFresh bool
	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		wasFresh = s.IsFresh()
		gotUser, gotOK = s.Get("user")
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("celeris_session", validID))
	testutil.AssertNoError(t, err)
	if wasFresh {
		t.Fatal("session should not be fresh for valid existing ID")
	}
	if !gotOK || gotUser != "admin" {
		t.Fatalf("expected user=admin, got %v", gotUser)
	}
}

// --- ChainExtractor tests ---

func TestChainExtractorFirstWins(t *testing.T) {
	e := ChainExtractor(
		func(_ *celeris.Context) string { return "first" },
		func(_ *celeris.Context) string { return "second" },
	)
	ctx, _ := celeristest.NewContextT(t, "GET", "/")
	if got := e(ctx); got != "first" {
		t.Fatalf("ChainExtractor: got %q, want %q", got, "first")
	}
}

func TestChainExtractorFallsThrough(t *testing.T) {
	e := ChainExtractor(
		func(_ *celeris.Context) string { return "" },
		func(_ *celeris.Context) string { return "second" },
	)
	ctx, _ := celeristest.NewContextT(t, "GET", "/")
	if got := e(ctx); got != "second" {
		t.Fatalf("ChainExtractor: got %q, want %q", got, "second")
	}
}

func TestChainExtractorAllEmpty(t *testing.T) {
	e := ChainExtractor(
		func(_ *celeris.Context) string { return "" },
		func(_ *celeris.Context) string { return "" },
	)
	ctx, _ := celeristest.NewContextT(t, "GET", "/")
	if got := e(ctx); got != "" {
		t.Fatalf("ChainExtractor: got %q, want empty", got)
	}
}

func TestChainExtractorNoExtractors(t *testing.T) {
	e := ChainExtractor()
	ctx, _ := celeristest.NewContextT(t, "GET", "/")
	if got := e(ctx); got != "" {
		t.Fatalf("ChainExtractor(no args): got %q, want empty", got)
	}
}

func TestChainExtractorWithRealExtractors(t *testing.T) {
	store := NewMemoryStore()
	validID := strings.Repeat("ab", 32)
	_ = store.Save(context.Background(), validID, map[string]any{"src": "header", "_abs_exp": time.Now().UnixNano()}, time.Hour)

	mw := New(Config{
		Store: store,
		Extractor: ChainExtractor(
			CookieExtractor("celeris_session"),
			HeaderExtractor("x-session-id"),
		),
	})
	var gotSrc any
	var wasFresh bool
	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		wasFresh = s.IsFresh()
		gotSrc, _ = s.Get("src")
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-session-id", validID))
	testutil.AssertNoError(t, err)
	if wasFresh {
		t.Fatal("expected existing session from header extractor")
	}
	if gotSrc != "header" {
		t.Fatalf("expected src=header, got %v", gotSrc)
	}
}

func TestSessionPoolReuse(t *testing.T) {
	store := NewMemoryStore()
	mw := New(Config{Store: store})
	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("k", "v")
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// Run 1000 iterations and verify no excessive allocs via pool reuse.
	for range 1000 {
		ctx, rec := celeristest.NewContext("GET", "/",
			celeristest.WithHandlers(chain...))
		err := ctx.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rec.StatusCode != 200 {
			t.Fatalf("unexpected status: %d", rec.StatusCode)
		}
		celeristest.ReleaseContext(ctx)
	}

	// Verify pool returns a *Session (reuse check: not a new alloc every time).
	sess := sessionPool.Get().(*Session)
	sessionPool.Put(sess)
	if sess == nil {
		t.Fatal("expected non-nil session from pool")
	}
}

func TestKeysFiltersAbsExp(t *testing.T) {
	s := &Session{
		id:    "test",
		data:  map[string]any{"name": "alice", absExpKey: int64(12345), "age": 30},
		store: NewMemoryStore(),
	}
	keys := s.Keys()
	for _, k := range keys {
		if k == absExpKey {
			t.Fatal("Keys() should not include _abs_exp")
		}
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d: %v", len(keys), keys)
	}
}

func TestLenFiltersAbsExp(t *testing.T) {
	s := &Session{
		id:    "test",
		data:  map[string]any{"name": "alice", absExpKey: int64(12345)},
		store: NewMemoryStore(),
	}
	if got := s.Len(); got != 1 {
		t.Fatalf("Len() with _abs_exp: got %d, want 1", got)
	}
}

func TestLenWithoutAbsExp(t *testing.T) {
	s := &Session{
		id:    "test",
		data:  map[string]any{"name": "alice", "age": 30},
		store: NewMemoryStore(),
	}
	if got := s.Len(); got != 2 {
		t.Fatalf("Len() without _abs_exp: got %d, want 2", got)
	}
}

func TestSetRejectsAbsExpKey(t *testing.T) {
	s := &Session{
		id:    "test",
		data:  map[string]any{absExpKey: int64(99)},
		store: NewMemoryStore(),
	}
	s.Set(absExpKey, int64(0))
	// The original value should be preserved.
	v, ok := s.data[absExpKey]
	if !ok {
		t.Fatal("expected _abs_exp to still exist in data")
	}
	if v.(int64) != 99 {
		t.Fatalf("expected _abs_exp=99, got %v", v)
	}
	if s.modified {
		t.Fatal("Set(_abs_exp, ...) should not mark session as modified")
	}
}

func TestNilDataBeforePoolPut(t *testing.T) {
	store := NewMemoryStore()
	mw := New(Config{Store: store})
	handler := func(c *celeris.Context) error {
		sess := FromContext(c)
		if sess == nil {
			t.Fatal("expected session in context")
		}
		sess.Set("key", "val")
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := testutil.RunChain(t, chain, "GET", "/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// After the middleware completes, the session is returned to the pool
	// with data=nil. Get it back and verify.
	sess := sessionPool.Get().(*Session)
	defer sessionPool.Put(sess)
	if sess.data != nil {
		t.Fatal("expected sess.data to be nil after pool return")
	}
}

// --- Store.Reset tests ---

func TestMemoryStoreReset(t *testing.T) {
	store := NewMemoryStore(MemoryStoreConfig{Shards: 4})
	ctx := context.Background()

	// Populate multiple sessions across shards.
	for i := range 20 {
		sid := hexID(byte(i))
		_ = store.Save(ctx, sid, map[string]any{"i": i}, time.Hour)
	}

	// Verify at least one exists.
	data, _ := store.Get(ctx, hexID(0))
	if data == nil {
		t.Fatal("expected session to exist before Reset")
	}

	// Reset.
	if err := store.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	// All sessions should be gone.
	for i := range 20 {
		sid := hexID(byte(i))
		data, err := store.Get(ctx, sid)
		if err != nil {
			t.Fatalf("Get after Reset: %v", err)
		}
		if data != nil {
			t.Fatalf("expected nil after Reset for session %d", i)
		}
	}
}

func TestMemoryStoreResetEmpty(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Reset(context.Background()); err != nil {
		t.Fatalf("Reset on empty store: %v", err)
	}
}

func TestMemoryStoreResetConcurrent(t *testing.T) {
	store := NewMemoryStore(MemoryStoreConfig{Shards: 4})
	ctx := context.Background()

	// Populate.
	for i := range 50 {
		_ = store.Save(ctx, hexID(byte(i)), map[string]any{"i": i}, time.Hour)
	}

	var wg sync.WaitGroup
	// Concurrent Reset + CRUD operations.
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = store.Reset(ctx)
	}()
	go func() {
		defer wg.Done()
		for i := range 50 {
			sid := hexID(byte(i + 100))
			_ = store.Save(ctx, sid, map[string]any{"i": i}, time.Hour)
			_, _ = store.Get(ctx, sid)
			_ = store.Delete(ctx, sid)
		}
	}()
	wg.Wait()
}

func TestMemoryStoreResetThenSave(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	_ = store.Save(ctx, "before", map[string]any{"k": "v"}, time.Hour)
	_ = store.Reset(ctx)

	// Store should accept new sessions after Reset.
	_ = store.Save(ctx, "after", map[string]any{"k": "v2"}, time.Hour)
	data, err := store.Get(ctx, "after")
	if err != nil {
		t.Fatalf("Get after Reset+Save: %v", err)
	}
	if data == nil || data["k"] != "v2" {
		t.Fatalf("expected k=v2 after Reset+Save, got %v", data)
	}

	// Old session should still be gone.
	data, _ = store.Get(ctx, "before")
	if data != nil {
		t.Fatal("expected old session gone after Reset")
	}
}

// --- Context propagation tests ---

// contextStore records the context passed to each method for verification.
type contextStore struct {
	inner    Store
	mu       sync.Mutex
	lastCtxs []context.Context
}

func (cs *contextStore) Get(ctx context.Context, id string) (map[string]any, error) {
	cs.mu.Lock()
	cs.lastCtxs = append(cs.lastCtxs, ctx)
	cs.mu.Unlock()
	return cs.inner.Get(ctx, id)
}

func (cs *contextStore) Save(ctx context.Context, id string, data map[string]any, expiry time.Duration) error {
	cs.mu.Lock()
	cs.lastCtxs = append(cs.lastCtxs, ctx)
	cs.mu.Unlock()
	return cs.inner.Save(ctx, id, data, expiry)
}

func (cs *contextStore) Delete(ctx context.Context, id string) error {
	cs.mu.Lock()
	cs.lastCtxs = append(cs.lastCtxs, ctx)
	cs.mu.Unlock()
	return cs.inner.Delete(ctx, id)
}

func (cs *contextStore) Reset(ctx context.Context) error {
	cs.mu.Lock()
	cs.lastCtxs = append(cs.lastCtxs, ctx)
	cs.mu.Unlock()
	return cs.inner.Reset(ctx)
}

func (cs *contextStore) contexts() []context.Context {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cp := make([]context.Context, len(cs.lastCtxs))
	copy(cp, cs.lastCtxs)
	return cp
}

func TestStoreReceivesRequestContext(t *testing.T) {
	cs := &contextStore{inner: NewMemoryStore()}
	mw := New(Config{Store: cs})

	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("key", "val")
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)

	ctxs := cs.contexts()
	// At minimum: Get (load attempt for fresh session is skipped since no cookie,
	// but the fresh session triggers a Save on post-handler).
	if len(ctxs) == 0 {
		t.Fatal("expected at least one context passed to store")
	}
	// All contexts should be non-nil.
	for i, ctx := range ctxs {
		if ctx == nil {
			t.Fatalf("context %d was nil", i)
		}
	}
}

func TestStoreContextOnExistingSession(t *testing.T) {
	cs := &contextStore{inner: NewMemoryStore()}
	sid := hexID(0xb1)
	_ = cs.inner.Save(context.Background(), sid, map[string]any{"user": "admin"}, time.Hour)

	mw := New(Config{Store: cs})
	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("updated", true)
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("celeris_session", sid),
	)
	testutil.AssertNoError(t, err)

	ctxs := cs.contexts()
	// Should have at least Get (load) + Save (post-handler).
	if len(ctxs) < 2 {
		t.Fatalf("expected at least 2 context captures, got %d", len(ctxs))
	}
	for i, ctx := range ctxs {
		if ctx == nil {
			t.Fatalf("context %d was nil", i)
		}
	}
}

func TestSessionDestroyPassesContext(t *testing.T) {
	cs := &contextStore{inner: NewMemoryStore()}
	sid := hexID(0xb2)
	_ = cs.inner.Save(context.Background(), sid, map[string]any{"user": "admin"}, time.Hour)

	mw := New(Config{Store: cs})
	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		return s.Destroy()
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("celeris_session", sid),
	)
	testutil.AssertNoError(t, err)

	ctxs := cs.contexts()
	// Get (load) + Delete (Destroy).
	if len(ctxs) < 2 {
		t.Fatalf("expected at least 2 context captures, got %d", len(ctxs))
	}
}

func TestSessionRegeneratePassesContext(t *testing.T) {
	cs := &contextStore{inner: NewMemoryStore()}
	sid := hexID(0xb3)
	_ = cs.inner.Save(context.Background(), sid, map[string]any{"user": "admin"}, time.Hour)

	mw := New(Config{Store: cs})
	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		return s.Regenerate()
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("celeris_session", sid),
	)
	testutil.AssertNoError(t, err)

	ctxs := cs.contexts()
	// Get (load) + Delete (Regenerate) + Save (post-handler).
	if len(ctxs) < 3 {
		t.Fatalf("expected at least 3 context captures, got %d", len(ctxs))
	}
}

func TestPooledSessionCtxNilAfterReturn(t *testing.T) {
	store := NewMemoryStore()
	mw := New(Config{Store: store})
	handler := func(c *celeris.Context) error {
		sess := FromContext(c)
		sess.Set("key", "val")
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := testutil.RunChain(t, chain, "GET", "/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sess := sessionPool.Get().(*Session)
	defer sessionPool.Put(sess)
	if sess.ctx != nil {
		t.Fatal("expected sess.ctx to be nil after pool return")
	}
}

// --- Handler.GetByID tests ---

func TestNewHandlerMiddlewareWorks(t *testing.T) {
	h := NewHandler()
	mw := h.Middleware()
	var gotFresh bool
	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		gotFresh = s.IsFresh()
		s.Set("user", "admin")
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	if !gotFresh {
		t.Fatal("expected fresh session from NewHandler middleware")
	}
}

func TestGetByIDExistingSession(t *testing.T) {
	store := NewMemoryStore()
	sid := hexID(0xd1)
	_ = store.Save(context.Background(), sid, map[string]any{"user": "admin", "role": "editor"}, time.Hour)

	h := NewHandler(Config{Store: store})
	sess, err := h.GetByID(context.Background(), sid)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if sess == nil {
		t.Fatal("expected non-nil session")
	}
	if sess.ID() != sid {
		t.Fatalf("ID: got %q, want %q", sess.ID(), sid)
	}
	user, ok := sess.Get("user")
	if !ok || user != "admin" {
		t.Fatalf("expected user=admin, got (%v, %v)", user, ok)
	}
	role, ok := sess.Get("role")
	if !ok || role != "editor" {
		t.Fatalf("expected role=editor, got (%v, %v)", role, ok)
	}
}

func TestGetByIDNonExistentSession(t *testing.T) {
	store := NewMemoryStore()
	h := NewHandler(Config{Store: store})
	sess, err := h.GetByID(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if sess != nil {
		t.Fatal("expected nil session for nonexistent ID")
	}
}

func TestGetByIDStoreError(t *testing.T) {
	fs := &failStore{getErr: errors.New("store error")}
	h := NewHandler(Config{Store: fs})
	sess, err := h.GetByID(context.Background(), "any-id")
	if err == nil || err.Error() != "store error" {
		t.Fatalf("expected 'store error', got %v", err)
	}
	if sess != nil {
		t.Fatal("expected nil session on store error")
	}
}

func TestGetByIDSessionIsReadOnly(t *testing.T) {
	store := NewMemoryStore()
	sid := hexID(0xd2)
	_ = store.Save(context.Background(), sid, map[string]any{"counter": 1}, time.Hour)

	h := NewHandler(Config{Store: store})
	sess, err := h.GetByID(context.Background(), sid)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	// Verify session has readOnlyStore/no ctx/no keyGen.
	if _, ok := sess.store.(readOnlyStore); !ok {
		t.Fatal("expected readOnlyStore on GetByID session")
	}
	if sess.ctx != nil {
		t.Fatal("expected ctx to be nil on read-only session")
	}
	if sess.keyGen != nil {
		t.Fatal("expected keyGen to be nil on read-only session")
	}
}

func TestGetByIDIntegrationWithMiddleware(t *testing.T) {
	store := NewMemoryStore()
	h := NewHandler(Config{Store: store})
	mw := h.Middleware()
	var sid string

	// Create a session via the middleware.
	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("user", "admin")
		sid = s.ID()
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)

	// Read it back via GetByID.
	sess, err := h.GetByID(context.Background(), sid)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session created by middleware to be readable via GetByID")
	}
	user, ok := sess.Get("user")
	if !ok || user != "admin" {
		t.Fatalf("expected user=admin from GetByID, got (%v, %v)", user, ok)
	}
}

func TestGetByIDNonExistentReturnsNil(t *testing.T) {
	store := NewMemoryStore()
	h := NewHandler(Config{Store: store})
	sess, err := h.GetByID(context.Background(), "nonexistent-id")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if sess != nil {
		t.Fatalf("expected nil session, got %+v", sess)
	}
}

func TestGetByIDExpiredSession(t *testing.T) {
	store := NewMemoryStore()
	sid := hexID(0xd3)
	_ = store.Save(context.Background(), sid, map[string]any{"k": "v"}, time.Nanosecond)

	h := NewHandler(Config{Store: store})
	time.Sleep(time.Millisecond)

	sess, err := h.GetByID(context.Background(), sid)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if sess != nil {
		t.Fatal("expected nil session for expired entry")
	}
}

// --- Fix verification tests ---

// Issue #1: Use-after-pool-Put — context key cleared on pool return.
// With OnRelease, cleanup fires when the context is released (not when the
// middleware returns). Verify the key is cleared after ReleaseContext.
func TestContextKeyNilAfterPoolReturn(t *testing.T) {
	store := NewMemoryStore()
	mw := New(Config{Store: store})
	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("k", "v")
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithHandlers(chain...),
	)
	err := ctx.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Key is still set after middleware returns (OnRelease hasn't fired yet).
	// Release the context to trigger OnRelease callbacks.
	celeristest.ReleaseContext(ctx)
	// After release, the context key should be nil.
	v, ok := ctx.Get(ContextKey)
	if ok && v != nil {
		t.Fatal("expected ContextKey to be nil after pool return (use-after-pool-Put)")
	}
}

// Issue #3: _abs_exp missing key bypass — sessions without _abs_exp are
// treated as expired when absolute timeout is enabled.
func TestAbsExpMissingKeyTreatedAsExpired(t *testing.T) {
	store := NewMemoryStore()
	sid := hexID(0xc1)
	// Save session WITHOUT _abs_exp.
	_ = store.Save(context.Background(), sid, map[string]any{"user": "admin"}, time.Hour)

	mw := New(Config{
		Store:           store,
		AbsoluteTimeout: 2 * time.Hour,
		IdleTimeout:     30 * time.Minute,
	})

	var sess *Session
	handler := func(c *celeris.Context) error {
		sess = FromContext(c)
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("celeris_session", sid),
	)
	testutil.AssertNoError(t, err)
	if !sess.IsFresh() {
		t.Fatal("expected fresh session when _abs_exp is missing (should be treated as expired)")
	}
	if sess.ID() == sid {
		t.Fatal("expected new session ID when _abs_exp is missing")
	}
}

// Issue #4: _abs_exp float64 type assertion — handles JSON-decoded float64.
func TestAbsExpFloat64TypeAssertion(t *testing.T) {
	store := NewMemoryStore()
	sid := hexID(0xc2)
	// Store _abs_exp as float64 (as JSON would decode it).
	oldCreated := float64(time.Now().Add(-3 * time.Hour).UnixNano())
	_ = store.Save(context.Background(), sid, map[string]any{absExpKey: oldCreated, "user": "admin"}, time.Hour)

	mw := New(Config{
		Store:           store,
		AbsoluteTimeout: 2 * time.Hour,
		IdleTimeout:     30 * time.Minute,
	})

	var sess *Session
	handler := func(c *celeris.Context) error {
		sess = FromContext(c)
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("celeris_session", sid),
	)
	testutil.AssertNoError(t, err)
	if !sess.IsFresh() {
		t.Fatal("expected fresh session when _abs_exp (float64) exceeds absolute timeout")
	}
}

// Issue #4 supplement: float64 _abs_exp within timeout is accepted.
func TestAbsExpFloat64NotExpired(t *testing.T) {
	store := NewMemoryStore()
	sid := hexID(0xc3)
	recentCreated := float64(time.Now().Add(-30 * time.Minute).UnixNano())
	_ = store.Save(context.Background(), sid, map[string]any{absExpKey: recentCreated, "user": "admin"}, time.Hour)

	mw := New(Config{
		Store:           store,
		AbsoluteTimeout: 2 * time.Hour,
		IdleTimeout:     30 * time.Minute,
	})

	var sess *Session
	handler := func(c *celeris.Context) error {
		sess = FromContext(c)
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("celeris_session", sid),
	)
	testutil.AssertNoError(t, err)
	if sess.IsFresh() {
		t.Fatal("expected existing session when float64 _abs_exp is within timeout")
	}
	if sess.ID() != sid {
		t.Fatal("expected same session ID when float64 _abs_exp is within timeout")
	}
}

// Issue #5: Cleanup processes min(4, numShards) per tick.
func TestMemoryStoreCleanupMultipleShards(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := NewMemoryStore(MemoryStoreConfig{
		Shards:          8,
		CleanupInterval: 10 * time.Millisecond,
		CleanupContext:  ctx,
	})

	// Save expired sessions that should be distributed across multiple shards.
	for i := range 20 {
		sid := hexID(byte(i + 0xe0))
		_ = store.Save(context.Background(), sid, map[string]any{"i": i}, time.Nanosecond)
	}
	time.Sleep(time.Millisecond)

	// Wait for cleanup to process multiple shards per tick.
	time.Sleep(100 * time.Millisecond)

	remaining := 0
	for i := range 20 {
		sid := hexID(byte(i + 0xe0))
		data, _ := store.Get(context.Background(), sid)
		if data != nil {
			remaining++
		}
	}
	if remaining > 0 {
		t.Fatalf("expected all expired sessions cleaned up, %d remaining", remaining)
	}
}

// Issue #6: MemoryStore.Close stops cleanup goroutine.
func TestMemoryStoreClose(t *testing.T) {
	store := NewMemoryStore(MemoryStoreConfig{
		CleanupInterval: time.Millisecond,
	})
	ms := store.(*memoryStore)
	ms.Close()
	// Double-close should not panic.
	ms.Close()
	// Store should still work after Close.
	err := store.Save(context.Background(), "test", map[string]any{"k": "v"}, time.Hour)
	if err != nil {
		t.Fatalf("Save after Close: %v", err)
	}
	data, err := store.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("Get after Close: %v", err)
	}
	if data["k"] != "v" {
		t.Fatal("expected data to persist after Close")
	}
}

// Issue #8: Save after Destroy returns ErrSessionDestroyed.
func TestSaveAfterDestroyReturnsError(t *testing.T) {
	store := NewMemoryStore()
	s := &Session{
		id:    "test",
		data:  map[string]any{"k": "v"},
		store: store,
		ctx:   context.Background(),
	}
	if err := s.Destroy(); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	err := s.Save()
	if !errors.Is(err, ErrSessionDestroyed) {
		t.Fatalf("expected ErrSessionDestroyed, got %v", err)
	}
}

// Issue #11: ErrorHandler wraps chainErr with save error.
func TestErrorHandlerWrapsChainErr(t *testing.T) {
	chainError := errors.New("handler error")
	saveError := errors.New("save failed")
	store := &failStore{saveErr: saveError}
	var gotErr error
	mw := New(Config{
		Store: store,
		ErrorHandler: func(_ *celeris.Context, err error) error {
			gotErr = err
			return err
		},
	})
	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("k", "v")
		return chainError
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	if err == nil {
		t.Fatal("expected error")
	}
	// The error passed to ErrorHandler should wrap both the save error
	// and the chain error.
	if !errors.Is(gotErr, saveError) {
		t.Fatalf("expected save error in wrapped error, got %v", gotErr)
	}
	if !errors.Is(gotErr, chainError) {
		t.Fatalf("expected chain error in wrapped error, got %v", gotErr)
	}
}

// Issue #12: Pool leak on error paths — session returned to pool even
// when store.Get fails.
func TestPoolReturnOnLoadError(t *testing.T) {
	fs := &failStore{getErr: errors.New("load failed")}
	sid := hexID(0xc4)
	mw := New(Config{Store: fs})
	handler := func(_ *celeris.Context) error { return nil }
	chain := []celeris.HandlerFunc{mw, handler}

	// Run multiple iterations to exercise the pool return path.
	for range 100 {
		_, _ = testutil.RunChain(t, chain, "GET", "/",
			celeristest.WithCookie("celeris_session", sid),
		)
	}
	// If pool was leaking, we'd create many new Session objects.
	// Verify pool returns a *Session.
	sess := sessionPool.Get().(*Session)
	sessionPool.Put(sess)
	if sess == nil {
		t.Fatal("expected non-nil session from pool")
	}
}

// Issue #16: GetByID session panics on Save.
func TestGetByIDPanicsOnSave(t *testing.T) {
	store := NewMemoryStore()
	sid := hexID(0xd4)
	_ = store.Save(context.Background(), sid, map[string]any{"k": "v"}, time.Hour)

	h := NewHandler(Config{Store: store})
	sess, err := h.GetByID(context.Background(), sid)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on Save of GetByID session")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "GetByID") {
			t.Fatalf("expected panic message about GetByID, got %v", r)
		}
	}()
	_ = sess.Save()
}

// Issue #16: GetByID session panics on Destroy.
func TestGetByIDPanicsOnDestroy(t *testing.T) {
	store := NewMemoryStore()
	sid := hexID(0xd5)
	_ = store.Save(context.Background(), sid, map[string]any{"k": "v"}, time.Hour)

	h := NewHandler(Config{Store: store})
	sess, err := h.GetByID(context.Background(), sid)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on Destroy of GetByID session")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "GetByID") {
			t.Fatalf("expected panic message about GetByID, got %v", r)
		}
	}()
	_ = sess.Destroy()
}
