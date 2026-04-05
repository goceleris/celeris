package proxy

import (
	"testing"

	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkProxyTrusted(b *testing.B) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8", "192.168.0.0/16"},
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/",
			celeristest.WithRemoteAddr("10.0.0.1:1234"),
			celeristest.WithHeader("x-forwarded-for", "1.2.3.4, 192.168.1.1, 10.0.0.2"),
		)
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkProxyUntrusted(b *testing.B) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/",
			celeristest.WithRemoteAddr("203.0.113.1:1234"),
			celeristest.WithHeader("x-forwarded-for", "5.6.7.8"),
		)
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkProxyNoConfig(b *testing.B) {
	mw := New(Config{})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/",
			celeristest.WithRemoteAddr("10.0.0.1:1234"),
			celeristest.WithHeader("x-forwarded-for", "5.6.7.8"),
		)
		_ = mw(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkWalkXFF(b *testing.B) {
	nets := parseTrustedProxies([]string{"10.0.0.0/8", "192.168.0.0/16"})
	xff := "1.2.3.4, 192.168.1.1, 10.0.0.2"
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		walkXFF(xff, nets)
	}
}
