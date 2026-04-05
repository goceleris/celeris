package benchcmp

import (
	"crypto/subtle"
	"encoding/base64"
	"io"
	"sync"
	"testing"
	"time"

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
	"github.com/valyala/fasthttp"
)

func fiberApp(setup func(app *fiber.App)) fasthttp.RequestHandler {
	app := fiber.New()
	setup(app)
	return app.Handler()
}

var fctxPool = sync.Pool{New: func() any { return &fasthttp.RequestCtx{} }}

func acquireFctx(method, path string) *fasthttp.RequestCtx {
	fctx := fctxPool.Get().(*fasthttp.RequestCtx)
	fctx.Request.Header.SetMethod(method)
	fctx.Request.SetRequestURI(path)
	return fctx
}

func releaseFctx(fctx *fasthttp.RequestCtx) {
	fctx.Request.Reset()
	fctx.Response.Reset()
	fctxPool.Put(fctx)
}

func BenchmarkLogger_Fiber(b *testing.B) {
	h := fiberApp(func(app *fiber.App) {
		app.Use(fiberlogger.New(fiberlogger.Config{Stream: io.Discard}))
		app.Get("/bench", func(c fiber.Ctx) error { return nil })
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		fctx := acquireFctx(fiber.MethodGet, "/bench")
		h(fctx)
		releaseFctx(fctx)
	}
}

func BenchmarkRecovery_Fiber(b *testing.B) {
	h := fiberApp(func(app *fiber.App) {
		app.Use(fiberrecover.New())
		app.Get("/bench", func(c fiber.Ctx) error { return nil })
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		fctx := acquireFctx(fiber.MethodGet, "/bench")
		h(fctx)
		releaseFctx(fctx)
	}
}

func BenchmarkCORS_Fiber_Preflight(b *testing.B) {
	h := fiberApp(func(app *fiber.App) {
		app.Use(fibercors.New(fibercors.Config{AllowOrigins: []string{"*"}}))
		app.Get("/bench", func(c fiber.Ctx) error { return nil })
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		fctx := acquireFctx(fiber.MethodOptions, "/bench")
		fctx.Request.Header.Set("Origin", "http://example.com")
		fctx.Request.Header.Set("Access-Control-Request-Method", "GET")
		h(fctx)
		releaseFctx(fctx)
	}
}

func BenchmarkCORS_Fiber_Simple(b *testing.B) {
	h := fiberApp(func(app *fiber.App) {
		app.Use(fibercors.New(fibercors.Config{AllowOrigins: []string{"*"}}))
		app.Get("/bench", func(c fiber.Ctx) error { return nil })
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		fctx := acquireFctx(fiber.MethodGet, "/bench")
		fctx.Request.Header.Set("Origin", "http://example.com")
		h(fctx)
		releaseFctx(fctx)
	}
}

func BenchmarkRateLimit_Fiber(b *testing.B) {
	h := fiberApp(func(app *fiber.App) {
		app.Use(fiberlimiter.New(fiberlimiter.Config{
			Max:        int(1e9),
			Expiration: time.Hour,
		}))
		app.Get("/bench", func(c fiber.Ctx) error { return nil })
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		fctx := acquireFctx(fiber.MethodGet, "/bench")
		fctx.Request.Header.Set("X-Forwarded-For", "1.2.3.4")
		h(fctx)
		releaseFctx(fctx)
	}
}

func BenchmarkRequestID_Fiber(b *testing.B) {
	h := fiberApp(func(app *fiber.App) {
		app.Use(fiberrequestid.New())
		app.Get("/bench", func(c fiber.Ctx) error { return nil })
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		fctx := acquireFctx(fiber.MethodGet, "/bench")
		h(fctx)
		releaseFctx(fctx)
	}
}

func BenchmarkTimeout_Fiber(b *testing.B) {
	h := fiberApp(func(app *fiber.App) {
		app.Get("/bench", fibertimeout.New(func(c fiber.Ctx) error { return nil }, fibertimeout.Config{Timeout: 5 * time.Second}))
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		fctx := acquireFctx(fiber.MethodGet, "/bench")
		h(fctx)
		releaseFctx(fctx)
	}
}

func BenchmarkSecure_Fiber(b *testing.B) {
	h := fiberApp(func(app *fiber.App) {
		app.Use(fiberhelmet.New())
		app.Get("/bench", func(c fiber.Ctx) error { return nil })
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		fctx := acquireFctx(fiber.MethodGet, "/bench")
		h(fctx)
		releaseFctx(fctx)
	}
}

func BenchmarkKeyAuth_Fiber(b *testing.B) {
	h := fiberApp(func(app *fiber.App) {
		app.Use(fiberkeyauth.New(fiberkeyauth.Config{
			Extractor: fiberextractors.FromHeader("X-Api-Key"),
			Validator: func(_ fiber.Ctx, key string) (bool, error) {
				return subtle.ConstantTimeCompare([]byte(key), []byte("test-api-key")) == 1, nil
			},
		}))
		app.Get("/bench", func(c fiber.Ctx) error { return nil })
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		fctx := acquireFctx(fiber.MethodGet, "/bench")
		fctx.Request.Header.Set("X-Api-Key", "test-api-key")
		h(fctx)
		releaseFctx(fctx)
	}
}

func BenchmarkCSRF_Fiber(b *testing.B) {
	h := fiberApp(func(app *fiber.App) {
		app.Use(fibercsrf.New())
		app.Get("/bench", func(c fiber.Ctx) error { return nil })
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		fctx := acquireFctx(fiber.MethodGet, "/bench")
		h(fctx)
		releaseFctx(fctx)
	}
}

func BenchmarkBasicAuth_Fiber(b *testing.B) {
	h := fiberApp(func(app *fiber.App) {
		app.Use(fiberbasicauth.New(fiberbasicauth.Config{
			Users: map[string]string{"admin": "secret"},
			Authorizer: func(user, pass string, _ fiber.Ctx) bool {
				return subtle.ConstantTimeCompare([]byte(user), []byte("admin")) == 1 &&
					subtle.ConstantTimeCompare([]byte(pass), []byte("secret")) == 1
			},
		}))
		app.Get("/bench", func(c fiber.Ctx) error { return nil })
	})
	creds := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:secret"))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		fctx := acquireFctx(fiber.MethodGet, "/bench")
		fctx.Request.Header.Set("Authorization", creds)
		h(fctx)
		releaseFctx(fctx)
	}
}
