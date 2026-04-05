package benchcmp

import (
	"crypto/subtle"
	"encoding/base64"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"
	rscors "github.com/rs/cors"
)

var chiNoop = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

func BenchmarkLogger_Chi(b *testing.B) {
	chimw.DefaultLogger = chimw.RequestLogger(&chimw.DefaultLogFormatter{
		Logger:  log.New(io.Discard, "", 0),
		NoColor: true,
	})
	handler := chimw.Logger(chiNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("GET", "/bench", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

func BenchmarkRecovery_Chi(b *testing.B) {
	handler := chimw.Recoverer(chiNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("GET", "/bench", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

func BenchmarkCORS_Chi_Preflight(b *testing.B) {
	c := rscors.New(rscors.Options{AllowedOrigins: []string{"*"}})
	handler := c.Handler(chiNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("OPTIONS", "/bench", nil)
		req.Header.Set("Origin", "http://example.com")
		req.Header.Set("Access-Control-Request-Method", "GET")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

func BenchmarkCORS_Chi_Simple(b *testing.B) {
	c := rscors.New(rscors.Options{AllowedOrigins: []string{"*"}})
	handler := c.Handler(chiNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("GET", "/bench", nil)
		req.Header.Set("Origin", "http://example.com")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

func BenchmarkRequestID_Chi(b *testing.B) {
	handler := chimw.RequestID(chiNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("GET", "/bench", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

func BenchmarkTimeout_Chi(b *testing.B) {
	handler := chimw.Timeout(5 * time.Second)(chiNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("GET", "/bench", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

func BenchmarkSecure_Chi(b *testing.B) {
	handler := stdlibSecureHeaders(chiNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("GET", "/bench", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

func BenchmarkKeyAuth_Chi(b *testing.B) {
	handler := chiKeyAuth("test-api-key")(chiNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("GET", "/bench", nil)
		req.Header.Set("X-Api-Key", "test-api-key")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

func BenchmarkCSRF_Chi(b *testing.B) {
	handler := stdlibCSRF(chiNoop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("GET", "/bench", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

func BenchmarkBasicAuth_Chi(b *testing.B) {
	handler := chiBasicAuthMiddleware("admin", "secret")(chiNoop)
	creds := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:secret"))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest("GET", "/bench", nil)
		req.Header.Set("Authorization", creds)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

func chiKeyAuth(key string) func(http.Handler) http.Handler {
	keyBytes := []byte(key)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiKey := r.Header.Get("X-Api-Key")
			if subtle.ConstantTimeCompare([]byte(apiKey), keyBytes) != 1 {
				w.WriteHeader(401)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func chiBasicAuthMiddleware(user, pass string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if len(auth) < 7 || auth[:6] != "Basic " {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			decoded, err := base64.StdEncoding.DecodeString(auth[6:])
			if err != nil {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			parts := string(decoded)
			var u, p string
			for i := range parts {
				if parts[i] == ':' {
					u, p = parts[:i], parts[i+1:]
					break
				}
			}
			if subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 ||
				subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
