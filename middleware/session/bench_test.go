package session

import (
	"context"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkSessionNew(b *testing.B) {
	mw := New()
	noop := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("user", "admin")
		return nil
	}
	opts := []celeristest.Option{celeristest.WithHandlers(mw, noop)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkSessionExisting(b *testing.B) {
	store := NewMemoryStore()
	mw := New(Config{
		Store:        store,
		KeyGenerator: func() string { return "bench-session-id" },
	})
	// Pre-populate store.
	_ = store.Save(context.Background(), "bench-session-id", map[string]any{"user": "admin"}, 0)

	noop := func(c *celeris.Context) error {
		s := FromContext(c)
		_ = s.ID()
		return nil
	}
	opts := []celeristest.Option{
		celeristest.WithCookie("celeris_session", "bench-session-id"),
		celeristest.WithHandlers(mw, noop),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkMemoryStoreGet(b *testing.B) {
	store := NewMemoryStore()
	ctx := context.Background()
	_ = store.Save(ctx, "bench-id", map[string]any{"k": "v"}, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = store.Get(ctx, "bench-id")
	}
}

func BenchmarkMemoryStoreSave(b *testing.B) {
	store := NewMemoryStore()
	ctx := context.Background()
	data := map[string]any{"k": "v"}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = store.Save(ctx, "bench-id", data, 0)
	}
}
