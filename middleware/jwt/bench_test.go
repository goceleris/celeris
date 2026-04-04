package jwt

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/jwt/internal/jwtparse"
)

func BenchmarkJWTAllow(b *testing.B) {
	secret := []byte("test-secret-key-32-bytes-long!!!")
	tokenStr, _ := jwtparse.SignToken(jwtparse.SigningMethodHS256, jwtparse.MapClaims{"sub": "1234"}, secret)

	mw := New(Config{SigningKey: secret})
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
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

func BenchmarkJWTDeny(b *testing.B) {
	secret := []byte("test-secret-key-32-bytes-long!!!")
	mw := New(Config{SigningKey: secret})
	opts := []celeristest.Option{
		celeristest.WithHeader("authorization", "Bearer invalid.token.here"),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/", opts...)
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkJWTMissing(b *testing.B) {
	secret := []byte("test-secret-key-32-bytes-long!!!")
	mw := New(Config{SigningKey: secret})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/")
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkJWTSigningKeys(b *testing.B) {
	secret1 := []byte("secret-one-32-bytes-long!!!!!!!!")
	secret2 := []byte("secret-two-32-bytes-long!!!!!!!!")
	tokenStr := signTokenWithKid(jwtparse.MapClaims{"sub": "1234"}, "key-2", secret2)

	mw := New(Config{
		SigningKeys: map[string]any{
			"key-1": secret1,
			"key-2": secret2,
		},
	})
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
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

func BenchmarkJWTQueryLookup(b *testing.B) {
	secret := []byte("test-secret-key-32-bytes-long!!!")
	tokenStr, _ := jwtparse.SignToken(jwtparse.SigningMethodHS256, jwtparse.MapClaims{"sub": "1234"}, secret)

	mw := New(Config{
		SigningKey:  secret,
		TokenLookup: "query:token",
	})
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{
		celeristest.WithQuery("token", tokenStr),
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
