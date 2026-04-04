package keyauth

import (
	"crypto/subtle"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkKeyAuthAllow(b *testing.B) {
	mw := New(Config{
		Validator: func(_ *celeris.Context, key string) (bool, error) {
			return subtle.ConstantTimeCompare([]byte(key), []byte("test-key")) == 1, nil
		},
	})
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{
		celeristest.WithHeader("x-api-key", "test-key"),
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

func BenchmarkKeyAuthDeny(b *testing.B) {
	mw := New(Config{
		Validator: func(_ *celeris.Context, _ string) (bool, error) {
			return false, nil
		},
	})
	opts := []celeristest.Option{celeristest.WithHeader("x-api-key", "wrong")}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/", opts...)
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkKeyAuthStaticKeys(b *testing.B) {
	mw := New(Config{
		Validator: StaticKeys("key-1", "key-2", "key-3"),
	})
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{
		celeristest.WithHeader("x-api-key", "key-2"),
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

func BenchmarkKeyAuthMissing(b *testing.B) {
	mw := New(Config{
		Validator: func(_ *celeris.Context, _ string) (bool, error) {
			return true, nil
		},
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/")
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}
