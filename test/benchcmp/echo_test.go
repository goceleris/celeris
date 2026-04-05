package benchcmp

import (
	"crypto/subtle"
	"encoding/base64"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
)

func newEcho() *echo.Echo {
	e := echo.New()
	e.Logger.SetOutput(io.Discard)
	return e
}

var echoNoop = func(c echo.Context) error { return nil }

func BenchmarkLogger_Echo(b *testing.B) {
	e := newEcho()
	mw := echomw.LoggerWithConfig(echomw.LoggerConfig{Output: io.Discard})
	handler := mw(echoNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("GET", "/bench", nil)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		_ = handler(c)
	}
}

func BenchmarkRecovery_Echo(b *testing.B) {
	e := newEcho()
	mw := echomw.RecoverWithConfig(echomw.RecoverConfig{
		DisablePrintStack: true,
		LogErrorFunc:      func(_ echo.Context, _ error, _ []byte) error { return nil },
	})
	handler := mw(echoNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("GET", "/bench", nil)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		_ = handler(c)
	}
}

func BenchmarkCORS_Echo_Preflight(b *testing.B) {
	e := newEcho()
	mw := echomw.CORSWithConfig(echomw.CORSConfig{AllowOrigins: []string{"*"}})
	handler := mw(echoNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("OPTIONS", "/bench", nil)
		req.Header.Set("Origin", "http://example.com")
		req.Header.Set("Access-Control-Request-Method", "GET")
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		_ = handler(c)
	}
}

func BenchmarkCORS_Echo_Simple(b *testing.B) {
	e := newEcho()
	mw := echomw.CORSWithConfig(echomw.CORSConfig{AllowOrigins: []string{"*"}})
	handler := mw(echoNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("GET", "/bench", nil)
		req.Header.Set("Origin", "http://example.com")
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		_ = handler(c)
	}
}

func BenchmarkRateLimit_Echo(b *testing.B) {
	e := newEcho()
	mw := echomw.RateLimiterWithConfig(echomw.RateLimiterConfig{
		Store: echomw.NewRateLimiterMemoryStoreWithConfig(echomw.RateLimiterMemoryStoreConfig{
			Rate:      1e9,
			Burst:     int(1e9),
			ExpiresIn: time.Hour,
		}),
		IdentifierExtractor: func(c echo.Context) (string, error) {
			return c.RealIP(), nil
		},
	})
	handler := mw(echoNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("GET", "/bench", nil)
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		_ = handler(c)
	}
}

func BenchmarkRequestID_Echo(b *testing.B) {
	e := newEcho()
	mw := echomw.RequestID()
	handler := mw(echoNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("GET", "/bench", nil)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		_ = handler(c)
	}
}

func BenchmarkTimeout_Echo(b *testing.B) {
	e := newEcho()
	mw := echomw.TimeoutWithConfig(echomw.TimeoutConfig{
		Timeout: 5 * time.Second,
	})
	handler := mw(echoNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("GET", "/bench", nil)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		_ = handler(c)
	}
}

func BenchmarkBodyLimit_Echo(b *testing.B) {
	e := newEcho()
	mw := echomw.BodyLimit("1M")
	handler := mw(echoNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("POST", "/bench", nil)
		req.Header.Set("Content-Length", "1024")
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		_ = handler(c)
	}
}

func BenchmarkSecure_Echo(b *testing.B) {
	e := newEcho()
	mw := echomw.Secure()
	handler := mw(echoNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("GET", "/bench", nil)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		_ = handler(c)
	}
}

func BenchmarkKeyAuth_Echo(b *testing.B) {
	e := newEcho()
	mw := echomw.KeyAuthWithConfig(echomw.KeyAuthConfig{
		KeyLookup:  "header:X-Api-Key",
		AuthScheme: "",
		Validator: func(key string, _ echo.Context) (bool, error) {
			return subtle.ConstantTimeCompare([]byte(key), []byte("test-api-key")) == 1, nil
		},
	})
	handler := mw(echoNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("GET", "/bench", nil)
		req.Header.Set("X-Api-Key", "test-api-key")
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		_ = handler(c)
	}
}

func BenchmarkCSRF_Echo(b *testing.B) {
	e := newEcho()
	e.Use(echomw.CSRF())
	e.GET("/bench", echoNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("GET", "/bench", nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
	}
}

func BenchmarkBasicAuth_Echo(b *testing.B) {
	e := newEcho()
	mw := echomw.BasicAuth(func(user, pass string, _ echo.Context) (bool, error) {
		return subtle.ConstantTimeCompare([]byte(user), []byte("admin")) == 1 &&
			subtle.ConstantTimeCompare([]byte(pass), []byte("secret")) == 1, nil
	})
	handler := mw(echoNoop)
	creds := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:secret"))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("GET", "/bench", nil)
		req.Header.Set("Authorization", creds)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		_ = handler(c)
	}
}
