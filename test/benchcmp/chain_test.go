package benchcmp

// Chain benchmarks test realistic middleware stacks across frameworks.
// Each chain represents a common production configuration.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/goceleris/celeris/celeristest"
	"github.com/gofiber/fiber/v3"
	fiberextractors "github.com/gofiber/fiber/v3/extractors"
	fiberbasicauth "github.com/gofiber/fiber/v3/middleware/basicauth"
	fibercors "github.com/gofiber/fiber/v3/middleware/cors"
	fibercsrf "github.com/gofiber/fiber/v3/middleware/csrf"
	fiberhelmet "github.com/gofiber/fiber/v3/middleware/helmet"
	fiberkeyauth "github.com/gofiber/fiber/v3/middleware/keyauth"
	fiberlimiter "github.com/gofiber/fiber/v3/middleware/limiter"
	fiberlogger "github.com/gofiber/fiber/v3/middleware/logger"
	fiberrecover "github.com/gofiber/fiber/v3/middleware/recover"
	fiberrequestid "github.com/gofiber/fiber/v3/middleware/requestid"
	fibertimeout "github.com/gofiber/fiber/v3/middleware/timeout"
	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	rscors "github.com/rs/cors"

	"github.com/goceleris/celeris/middleware/basicauth"
	"github.com/goceleris/celeris/middleware/cors"
	"github.com/goceleris/celeris/middleware/csrf"
	"github.com/goceleris/celeris/middleware/keyauth"
	"github.com/goceleris/celeris/middleware/logger"
	"github.com/goceleris/celeris/middleware/ratelimit"
	"github.com/goceleris/celeris/middleware/recovery"
	"github.com/goceleris/celeris/middleware/requestid"
	"github.com/goceleris/celeris/middleware/secure"
	"github.com/goceleris/celeris/middleware/timeout"
)

// ---------------------------------------------------------------------------
// Chain A: Public API — Recovery + RequestID + CORS + RateLimit + handler
// ---------------------------------------------------------------------------

func BenchmarkChainAPI_Celeris(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := recovery.New(recovery.Config{DisableLogStack: true})
	rid := requestid.New()
	co := cors.New()
	rl := ratelimit.New(ratelimit.Config{
		RPS: 1e9, Burst: 1e9, CleanupContext: ctx,
	})

	opts := []celeristest.Option{
		celeristest.WithHeader("origin", "http://example.com"),
		celeristest.WithHeader("x-forwarded-for", "1.2.3.4"),
		celeristest.WithHandlers(rec, rid, co, rl, noop),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c, _ := celeristest.NewContext("GET", "/api/users", opts...)
		c.Next()
		celeristest.ReleaseContext(c)
	}
}

func BenchmarkChainAPI_Fiber(b *testing.B) {
	h := fiberApp(func(app *fiber.App) {
		app.Use(fiberrecover.New())
		app.Use(fiberrequestid.New())
		app.Use(fibercors.New(fibercors.Config{AllowOrigins: []string{"*"}}))
		app.Use(fiberlimiter.New(fiberlimiter.Config{
			Max: int(1e9), Expiration: time.Hour,
		}))
		app.Get("/api/users", func(c fiber.Ctx) error { return nil })
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		fctx := acquireFctx(fiber.MethodGet, "/api/users")
		fctx.Request.Header.Set("Origin", "http://example.com")
		fctx.Request.Header.Set("X-Forwarded-For", "1.2.3.4")
		h(fctx)
		releaseFctx(fctx)
	}
}

func BenchmarkChainAPI_Echo(b *testing.B) {
	e := echo.New()
	e.Use(echomw.Recover())
	e.Use(echomw.RequestID())
	e.Use(echomw.CORSWithConfig(echomw.CORSConfig{AllowOrigins: []string{"*"}}))
	e.Use(echomw.RateLimiterWithConfig(echomw.RateLimiterConfig{
		Store: echomw.NewRateLimiterMemoryStore(1e9),
	}))
	e.GET("/api/users", func(c echo.Context) error { return nil })

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
		req.Header.Set("Origin", "http://example.com")
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
	}
}

func BenchmarkChainAPI_Chi(b *testing.B) {
	r := chi.NewRouter()
	r.Use(chimw.Recoverer)
	r.Use(chimw.RequestID)
	r.Use(rscors.New(rscors.Options{AllowedOrigins: []string{"*"}}).Handler)
	r.Get("/api/users", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
		req.Header.Set("Origin", "http://example.com")
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
	}
}

func BenchmarkChainAPI_Stdlib(b *testing.B) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})
	chain := chainStdlibRecovery(chainStdlibRequestID(
		rscors.New(rscors.Options{AllowedOrigins: []string{"*"}}).Handler(handler),
	))

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
		req.Header.Set("Origin", "http://example.com")
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)
	}
}

// ---------------------------------------------------------------------------
// Chain B: Authenticated API — Recovery + Logger + RequestID + BasicAuth
// ---------------------------------------------------------------------------

func BenchmarkChainAuth_Celeris(b *testing.B) {
	rec := recovery.New(recovery.Config{DisableLogStack: true})
	lg := logger.New(logger.Config{
		Output: slog.New(logger.NewFastHandler(io.Discard, nil)),
	})
	rid := requestid.New()
	auth := basicauth.New(basicauth.Config{
		Validator: func(user, pass string) bool {
			return subtle.ConstantTimeCompare([]byte(user), []byte("admin")) == 1 &&
				subtle.ConstantTimeCompare([]byte(pass), []byte("secret")) == 1
		},
	})

	opts := []celeristest.Option{
		celeristest.WithBasicAuth("admin", "secret"),
		celeristest.WithHandlers(rec, lg, rid, auth, noop),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c, _ := celeristest.NewContext("GET", "/admin/dashboard", opts...)
		c.Next()
		celeristest.ReleaseContext(c)
	}
}

func BenchmarkChainAuth_Fiber(b *testing.B) {
	h := fiberApp(func(app *fiber.App) {
		app.Use(fiberrecover.New())
		app.Use(fiberlogger.New(fiberlogger.Config{Stream: io.Discard}))
		app.Use(fiberrequestid.New())
		app.Use(fiberbasicauth.New(fiberbasicauth.Config{
			Users: map[string]string{"admin": "secret"},
			Authorizer: func(user, pass string, _ fiber.Ctx) bool {
				return subtle.ConstantTimeCompare([]byte(user), []byte("admin")) == 1 &&
					subtle.ConstantTimeCompare([]byte(pass), []byte("secret")) == 1
			},
		}))
		app.Get("/admin/dashboard", func(c fiber.Ctx) error { return nil })
	})
	creds := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:secret"))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		fctx := acquireFctx(fiber.MethodGet, "/admin/dashboard")
		fctx.Request.Header.Set("Authorization", creds)
		h(fctx)
		releaseFctx(fctx)
	}
}

func BenchmarkChainAuth_Echo(b *testing.B) {
	e := echo.New()
	e.Use(echomw.Recover())
	e.Use(echomw.LoggerWithConfig(echomw.LoggerConfig{Output: io.Discard}))
	e.Use(echomw.RequestID())
	e.Use(echomw.BasicAuth(func(user, pass string, _ echo.Context) (bool, error) {
		return subtle.ConstantTimeCompare([]byte(user), []byte("admin")) == 1 &&
			subtle.ConstantTimeCompare([]byte(pass), []byte("secret")) == 1, nil
	}))
	e.GET("/admin/dashboard", func(c echo.Context) error { return nil })

	creds := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:secret"))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodGet, "/admin/dashboard", nil)
		req.Header.Set("Authorization", creds)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
	}
}

func BenchmarkChainAuth_Chi(b *testing.B) {
	r := chi.NewRouter()
	r.Use(chimw.Recoverer)
	r.Use(chimw.RequestLogger(&chimw.DefaultLogFormatter{Logger: log.New(io.Discard, "", 0), NoColor: true}))
	r.Use(chimw.RequestID)
	r.Use(chiBasicAuthMiddleware("admin", "secret"))
	r.Get("/admin/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})

	creds := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:secret"))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodGet, "/admin/dashboard", nil)
		req.Header.Set("Authorization", creds)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
	}
}

func BenchmarkChainAuth_Stdlib(b *testing.B) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})
	chain := chainStdlibRecovery(chainStdlibLogger(chainStdlibRequestID(
		stdlibBasicAuthMiddleware("admin", "secret")(handler),
	)))

	creds := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:secret"))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodGet, "/admin/dashboard", nil)
		req.Header.Set("Authorization", creds)
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)
	}
}

// ---------------------------------------------------------------------------
// Chain C: Security — Recovery + Secure + CSRF + KeyAuth
// ---------------------------------------------------------------------------

func BenchmarkChainSecurity_Celeris(b *testing.B) {
	rec := recovery.New(recovery.Config{DisableLogStack: true})
	sec := secure.New()
	cs := csrf.New()
	ka := keyauth.New(keyauth.Config{
		Validator: keyauth.StaticKeys("test-key"),
	})

	opts := []celeristest.Option{
		celeristest.WithHeader("x-api-key", "test-key"),
		celeristest.WithHandlers(rec, sec, cs, ka, noop),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c, _ := celeristest.NewContext("GET", "/secure/data", opts...)
		c.Next()
		celeristest.ReleaseContext(c)
	}
}

func BenchmarkChainSecurity_Fiber(b *testing.B) {
	h := fiberApp(func(app *fiber.App) {
		app.Use(fiberrecover.New())
		app.Use(fiberhelmet.New())
		app.Use(fibercsrf.New())
		app.Use(fiberkeyauth.New(fiberkeyauth.Config{
			Extractor: fiberextractors.FromHeader("X-Api-Key"),
			Validator: func(_ fiber.Ctx, key string) (bool, error) {
				return subtle.ConstantTimeCompare([]byte(key), []byte("test-key")) == 1, nil
			},
		}))
		app.Get("/secure/data", func(c fiber.Ctx) error { return nil })
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		fctx := acquireFctx(fiber.MethodGet, "/secure/data")
		fctx.Request.Header.Set("X-Api-Key", "test-key")
		h(fctx)
		releaseFctx(fctx)
	}
}

func BenchmarkChainSecurity_Echo(b *testing.B) {
	e := echo.New()
	e.Use(echomw.Recover())
	e.Use(echomw.Secure())
	e.Use(echomw.CSRF())
	e.Use(echomw.KeyAuthWithConfig(echomw.KeyAuthConfig{
		KeyLookup:  "header:X-Api-Key",
		AuthScheme: "",
		Validator: func(key string, _ echo.Context) (bool, error) {
			return subtle.ConstantTimeCompare([]byte(key), []byte("test-key")) == 1, nil
		},
	}))
	e.GET("/secure/data", func(c echo.Context) error { return nil })

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodGet, "/secure/data", nil)
		req.Header.Set("X-Api-Key", "test-key")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
	}
}

func BenchmarkChainSecurity_Chi(b *testing.B) {
	r := chi.NewRouter()
	r.Use(chimw.Recoverer)
	r.Use(stdlibSecureHeaders)
	r.Use(stdlibCSRF)
	r.Use(chiKeyAuth("test-key"))
	r.Get("/secure/data", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodGet, "/secure/data", nil)
		req.Header.Set("X-Api-Key", "test-key")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
	}
}

func BenchmarkChainSecurity_Stdlib(b *testing.B) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})
	keyBytes := []byte("test-key")
	stdlibKeyAuthHandler := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Api-Key")), keyBytes) != 1 {
				w.WriteHeader(401)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	chain := chainStdlibRecovery(stdlibSecureHeaders(stdlibCSRF(
		stdlibKeyAuthHandler(handler),
	)))

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodGet, "/secure/data", nil)
		req.Header.Set("X-Api-Key", "test-key")
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)
	}
}

// ---------------------------------------------------------------------------
// Chain D: Full Stack — Recovery + Logger + RequestID + CORS + RateLimit + Secure + Timeout
// ---------------------------------------------------------------------------

func BenchmarkChainFullStack_Celeris(b *testing.B) {
	bgCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := recovery.New(recovery.Config{DisableLogStack: true})
	lg := logger.New(logger.Config{
		Output: slog.New(logger.NewFastHandler(io.Discard, nil)),
	})
	rid := requestid.New()
	co := cors.New()
	rl := ratelimit.New(ratelimit.Config{
		RPS: 1e9, Burst: 1e9, CleanupContext: bgCtx,
	})
	sec := secure.New()
	to := timeout.New(timeout.Config{Timeout: 5 * time.Second})

	opts := []celeristest.Option{
		celeristest.WithHeader("origin", "http://example.com"),
		celeristest.WithHeader("x-forwarded-for", "1.2.3.4"),
		celeristest.WithHandlers(rec, lg, rid, co, rl, sec, to, noop),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c, _ := celeristest.NewContext("GET", "/full-stack", opts...)
		c.Next()
		celeristest.ReleaseContext(c)
	}
}

func BenchmarkChainFullStack_Fiber(b *testing.B) {
	h := fiberApp(func(app *fiber.App) {
		app.Use(fiberrecover.New())
		app.Use(fiberlogger.New(fiberlogger.Config{Stream: io.Discard}))
		app.Use(fiberrequestid.New())
		app.Use(fibercors.New(fibercors.Config{AllowOrigins: []string{"*"}}))
		app.Use(fiberlimiter.New(fiberlimiter.Config{
			Max: int(1e9), Expiration: time.Hour,
		}))
		app.Use(fiberhelmet.New())
		app.Get("/full-stack", fibertimeout.New(
			func(c fiber.Ctx) error { return nil },
			fibertimeout.Config{Timeout: 5 * time.Second},
		))
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		fctx := acquireFctx(fiber.MethodGet, "/full-stack")
		fctx.Request.Header.Set("Origin", "http://example.com")
		fctx.Request.Header.Set("X-Forwarded-For", "1.2.3.4")
		h(fctx)
		releaseFctx(fctx)
	}
}

func BenchmarkChainFullStack_Echo(b *testing.B) {
	e := echo.New()
	e.Use(echomw.Recover())
	e.Use(echomw.LoggerWithConfig(echomw.LoggerConfig{Output: io.Discard}))
	e.Use(echomw.RequestID())
	e.Use(echomw.CORSWithConfig(echomw.CORSConfig{AllowOrigins: []string{"*"}}))
	e.Use(echomw.RateLimiterWithConfig(echomw.RateLimiterConfig{
		Store: echomw.NewRateLimiterMemoryStore(1e9),
	}))
	e.Use(echomw.Secure())
	e.Use(echomw.TimeoutWithConfig(echomw.TimeoutConfig{Timeout: 5 * time.Second}))
	e.GET("/full-stack", func(c echo.Context) error { return nil })

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodGet, "/full-stack", nil)
		req.Header.Set("Origin", "http://example.com")
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
	}
}

func BenchmarkChainFullStack_Chi(b *testing.B) {
	r := chi.NewRouter()
	r.Use(chimw.Recoverer)
	r.Use(chimw.RequestLogger(&chimw.DefaultLogFormatter{Logger: log.New(io.Discard, "", 0), NoColor: true}))
	r.Use(chimw.RequestID)
	r.Use(rscors.New(rscors.Options{AllowedOrigins: []string{"*"}}).Handler)
	r.Use(stdlibSecureHeaders)
	r.Use(chimw.Timeout(5 * time.Second))
	r.Get("/full-stack", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodGet, "/full-stack", nil)
		req.Header.Set("Origin", "http://example.com")
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
	}
}

func BenchmarkChainFullStack_Stdlib(b *testing.B) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})
	chain := chainStdlibRecovery(chainStdlibLogger(chainStdlibRequestID(
		rscors.New(rscors.Options{AllowedOrigins: []string{"*"}}).Handler(
			stdlibSecureHeaders(
				http.TimeoutHandler(handler, 5*time.Second, "timeout"),
			),
		),
	)))

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodGet, "/full-stack", nil)
		req.Header.Set("Origin", "http://example.com")
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)
	}
}

// ---------------------------------------------------------------------------
// Stdlib middleware helpers for chain benchmarks (prefixed to avoid collision)
// ---------------------------------------------------------------------------

func chainStdlibRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rv := recover(); rv != nil {
				w.WriteHeader(500)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func chainStdlibRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf [16]byte
		_, _ = rand.Read(buf[:])
		id := fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r)
	})
}

func chainStdlibLogger(next http.Handler) http.Handler {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Duration("latency", time.Since(start)),
		)
	})
}

func stdlibBasicAuth(user, pass string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		const prefix = "Basic "
		if !strings.HasPrefix(auth, prefix) {
			w.Header().Set("WWW-Authenticate", `Basic realm="restricted"`)
			w.WriteHeader(401)
			return
		}
		decoded, err := base64.StdEncoding.DecodeString(auth[len(prefix):])
		if err != nil {
			w.WriteHeader(401)
			return
		}
		parts := strings.SplitN(string(decoded), ":", 2)
		if len(parts) != 2 {
			w.WriteHeader(401)
			return
		}
		if subtle.ConstantTimeCompare([]byte(parts[0]), []byte(user)) != 1 ||
			subtle.ConstantTimeCompare([]byte(parts[1]), []byte(pass)) != 1 {
			w.WriteHeader(401)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func stdlibCSRFChain(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" || r.Method == "HEAD" || r.Method == "OPTIONS" {
			var buf [32]byte
			_, _ = rand.Read(buf[:])
			token := hex.EncodeToString(buf[:])
			http.SetCookie(w, &http.Cookie{Name: "_csrf", Value: token, Path: "/", HttpOnly: true})
		}
		next.ServeHTTP(w, r)
	})
}
