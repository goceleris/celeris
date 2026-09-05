package session

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/store"
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

func BenchmarkSessionAnonymous(b *testing.B) {
	mw := New()
	noop := func(c *celeris.Context) error {
		s := FromContext(c)
		_ = s.ID()
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

const benchSessionID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func BenchmarkSessionExisting(b *testing.B) {
	kv := NewMemoryStore()
	mw := New(Config{
		Store:        kv,
		KeyGenerator: func() string { return benchSessionID },
	})
	// Pre-populate store with an encoded session blob containing a valid _abs_exp.
	buf, _ := store.EncodeJSON(map[string]any{
		"user":    "admin",
		absExpKey: time.Now().UnixNano(),
	})
	_ = kv.Set(context.Background(), benchSessionID, buf, 0)

	noop := func(c *celeris.Context) error {
		s := FromContext(c)
		_ = s.ID()
		return nil
	}
	opts := []celeristest.Option{
		celeristest.WithCookie("celeris_session", benchSessionID),
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
	kv := NewMemoryStore()
	ctx := context.Background()
	buf, _ := store.EncodeJSON(map[string]any{"k": "v"})
	_ = kv.Set(ctx, "bench-id", buf, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = kv.Get(ctx, "bench-id")
	}
}

func BenchmarkMemoryStoreSave(b *testing.B) {
	kv := NewMemoryStore()
	ctx := context.Background()
	buf, _ := store.EncodeJSON(map[string]any{"k": "v"})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = kv.Set(ctx, "bench-id", buf, 0)
	}
}
