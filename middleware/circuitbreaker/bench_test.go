package circuitbreaker

import (
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkCircuitBreakerClosed(b *testing.B) {
	mw := New(Config{
		Threshold:   0.5,
		MinRequests: 1_000_000,
		WindowSize:  time.Hour,
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c, _ := celeristest.NewContext("GET", "/")
		_ = mw(c)
		celeristest.ReleaseContext(c)
	}
}

func BenchmarkCircuitBreakerOpen(b *testing.B) {
	mw, _ := NewWithBreaker(Config{
		Threshold:      0.5,
		MinRequests:    2,
		WindowSize:     time.Hour,
		CooldownPeriod: time.Hour,
	})

	// Trip the breaker.
	for range 2 {
		handler := func(c *celeris.Context) error {
			return celeris.NewHTTPError(500, "fail")
		}
		c, _ := celeristest.NewContext("GET", "/",
			celeristest.WithHandlers(mw, handler))
		_ = c.Next()
		celeristest.ReleaseContext(c)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c, _ := celeristest.NewContext("GET", "/")
		_ = mw(c)
		celeristest.ReleaseContext(c)
	}
}

func BenchmarkCircuitBreakerParallel(b *testing.B) {
	mw := New(Config{
		Threshold:   0.5,
		MinRequests: 1_000_000,
		WindowSize:  time.Hour,
	})
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c, _ := celeristest.NewContext("GET", "/")
			_ = mw(c)
			celeristest.ReleaseContext(c)
		}
	})
}
