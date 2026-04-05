package basicauth

import (
	"crypto/subtle"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkBasicAuthAllow(b *testing.B) {
	mw := New(Config{
		Validator: func(user, pass string) bool {
			return subtle.ConstantTimeCompare([]byte(user), []byte("admin")) == 1 &&
				subtle.ConstantTimeCompare([]byte(pass), []byte("secret")) == 1
		},
	})
	noop := func(_ *celeris.Context) error { return nil }
	opts := []celeristest.Option{
		celeristest.WithBasicAuth("admin", "secret"),
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

func BenchmarkBasicAuthDeny(b *testing.B) {
	mw := New(Config{
		Validator: func(_, _ string) bool { return false },
	})
	opts := []celeristest.Option{celeristest.WithBasicAuth("wrong", "creds")}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/", opts...)
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}
