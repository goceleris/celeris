package middleware_test

import (
	"context"
	"encoding/hex"
	"hash/crc32"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/circuitbreaker"
	"github.com/goceleris/celeris/middleware/cors"
	"github.com/goceleris/celeris/middleware/etag"
	"github.com/goceleris/celeris/middleware/methodoverride"
	"github.com/goceleris/celeris/middleware/proxy"
	"github.com/goceleris/celeris/middleware/ratelimit"
	"github.com/goceleris/celeris/middleware/recovery"
	"github.com/goceleris/celeris/middleware/redirect"
	"github.com/goceleris/celeris/middleware/requestid"
	"github.com/goceleris/celeris/middleware/secure"
	"github.com/goceleris/celeris/middleware/singleflight"
	"github.com/goceleris/celeris/middleware/timeout"
)

// weakCRC32ETag computes the weak CRC-32 ETag for a body, matching the
// format produced by the etag middleware.
func weakCRC32ETag(body []byte) string {
	cs := crc32.ChecksumIEEE(body)
	var buf [12]byte
	buf[0] = 'W'
	buf[1] = '/'
	buf[2] = '"'
	hex.Encode(buf[3:11], []byte{byte(cs >> 24), byte(cs >> 16), byte(cs >> 8), byte(cs)})
	buf[11] = '"'
	return string(buf[:])
}

// Pre-computed bodies and ETags used across benchmarks.
var (
	textBody        = []byte(strings.Repeat("The quick brown fox jumps over the lazy dog. ", 20))
	textHandlerETag = weakCRC32ETag(textBody)

	html4KB     = []byte("<html><head><title>Example</title></head><body>" + strings.Repeat("<p>Lorem ipsum dolor sit amet, consectetur adipiscing elit.</p>", 60) + "</body></html>")
	html4KBETag = weakCRC32ETag(html4KB)

	json1KB     = []byte(`{"users":[` + strings.Repeat(`{"id":1,"name":"Alice","email":"alice@example.com","role":"admin"},`, 12) + `{"id":13,"name":"Bob","email":"bob@example.com","role":"user"}]}`)
	json1KBETag = weakCRC32ETag(json1KB)
)

// jsonHandler simulates a typical JSON API response.
func jsonHandler(c *celeris.Context) error {
	return c.JSON(200, map[string]string{"status": "ok", "message": "hello world"})
}

// textHandler simulates a text response for ETag testing.
func textHandler(c *celeris.Context) error {
	return c.Blob(200, "text/plain", textBody)
}

// html4KBHandler returns a ~4KB HTML page, typical for static file serving.
func html4KBHandler(c *celeris.Context) error {
	return c.Blob(200, "text/html", html4KB)
}

// json1KBHandler returns a ~1KB JSON response, typical for an API endpoint.
func json1KBHandler(c *celeris.Context) error {
	return c.Blob(200, "application/json", json1KB)
}

// webhookHandler simulates a POST webhook receiver response.
func webhookHandler(c *celeris.Context) error {
	return c.JSON(200, map[string]string{"received": "true"})
}

// BenchmarkChainBaseline benchmarks the handler alone (no middleware).
func BenchmarkChainBaseline(b *testing.B) {
	opts := []celeristest.Option{celeristest.WithHandlers(jsonHandler)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/users", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkChainMinimalAPI benchmarks: requestid -> recovery -> handler.
func BenchmarkChainMinimalAPI(b *testing.B) {
	chain := []celeris.HandlerFunc{
		requestid.New(),
		recovery.New(recovery.Config{DisableLogStack: true}),
		jsonHandler,
	}
	opts := []celeristest.Option{celeristest.WithHandlers(chain...)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/users", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkChainStandardAPI benchmarks: requestid -> recovery -> cors -> secure -> handler.
func BenchmarkChainStandardAPI(b *testing.B) {
	chain := []celeris.HandlerFunc{
		requestid.New(),
		recovery.New(recovery.Config{DisableLogStack: true}),
		cors.New(),
		secure.New(),
		jsonHandler,
	}
	opts := []celeristest.Option{
		celeristest.WithHandlers(chain...),
		celeristest.WithHeader("origin", "https://example.com"),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/users", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkChainStandardNoOrigin benchmarks the standard chain without an Origin header
// (CORS fast path: no origin = not a CORS request).
func BenchmarkChainStandardNoOrigin(b *testing.B) {
	chain := []celeris.HandlerFunc{
		requestid.New(),
		recovery.New(recovery.Config{DisableLogStack: true}),
		cors.New(),
		secure.New(),
		jsonHandler,
	}
	opts := []celeristest.Option{celeristest.WithHandlers(chain...)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/users", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkChainFullAPI benchmarks: requestid -> recovery -> cors -> secure -> etag -> handler.
func BenchmarkChainFullAPI(b *testing.B) {
	chain := []celeris.HandlerFunc{
		requestid.New(),
		recovery.New(recovery.Config{DisableLogStack: true}),
		cors.New(),
		secure.New(),
		etag.New(),
		textHandler,
	}
	opts := []celeristest.Option{
		celeristest.WithHandlers(chain...),
		celeristest.WithHeader("origin", "https://example.com"),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/data", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkChainFullAPI304 benchmarks the full chain with ETag 304 (conditional response).
func BenchmarkChainFullAPI304(b *testing.B) {
	chain := []celeris.HandlerFunc{
		requestid.New(),
		recovery.New(recovery.Config{DisableLogStack: true}),
		cors.New(),
		secure.New(),
		etag.New(),
		textHandler,
	}
	opts := []celeristest.Option{
		celeristest.WithHandlers(chain...),
		celeristest.WithHeader("origin", "https://example.com"),
		celeristest.WithHeader("if-none-match", textHandlerETag),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/data", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkChainPreRouting benchmarks: proxy -> redirect.HTTPSRedirect -> methodoverride -> handler.
func BenchmarkChainPreRouting(b *testing.B) {
	chain := []celeris.HandlerFunc{
		proxy.New(proxy.Config{
			TrustedProxies: []string{"10.0.0.0/8"},
		}),
		redirect.HTTPSRedirect(),
		methodoverride.New(methodoverride.Config{
			Getter: methodoverride.HeaderGetter("X-HTTP-Method-Override"),
		}),
		jsonHandler,
	}
	opts := []celeristest.Option{
		celeristest.WithHandlers(chain...),
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-for", "203.0.113.50"),
		celeristest.WithScheme("https"),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("POST", "/api/users", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// deepSecureConfig suppresses most secure headers to keep total response
// headers under the 16-slot inline buffer. This isolates the chain-depth
// overhead from the header-count bug (respHdrBuf overflow on reset).
var deepSecureConfig = secure.Config{
	XContentTypeOptions:       "nosniff",
	XFrameOptions:             "SAMEORIGIN",
	XSSProtection:             secure.Suppress,
	ReferrerPolicy:            secure.Suppress,
	CrossOriginOpenerPolicy:   secure.Suppress,
	CrossOriginResourcePolicy: secure.Suppress,
	CrossOriginEmbedderPolicy: secure.Suppress,
	XDNSPrefetchControl:       secure.Suppress,
	XPermittedCrossDomain:     secure.Suppress,
	OriginAgentCluster:        secure.Suppress,
	XDownloadOptions:          secure.Suppress,
	DisableHSTS:               true,
}

// BenchmarkChainDeep benchmarks an 8-middleware deep chain:
// requestid -> recovery -> cors -> secure -> ratelimit -> timeout -> etag -> handler.
func BenchmarkChainDeep(b *testing.B) {
	cleanupCtx, cancel := context.WithCancel(context.Background())
	b.Cleanup(cancel)

	chain := []celeris.HandlerFunc{
		requestid.New(),
		recovery.New(recovery.Config{DisableLogStack: true}),
		cors.New(),
		secure.New(deepSecureConfig),
		ratelimit.New(ratelimit.Config{
			RPS:            1e9,
			Burst:          1 << 30,
			CleanupContext: cleanupCtx,
			DisableHeaders: true,
		}),
		timeout.New(timeout.Config{Timeout: 30 * time.Second}),
		etag.New(),
		textHandler,
	}
	opts := []celeristest.Option{
		celeristest.WithHandlers(chain...),
		celeristest.WithHeader("origin", "https://example.com"),
		celeristest.WithRemoteAddr("127.0.0.1:9999"),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/data", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkChainDeepParallel runs the deep chain under GOMAXPROCS goroutines.
func BenchmarkChainDeepParallel(b *testing.B) {
	cleanupCtx, cancel := context.WithCancel(context.Background())
	b.Cleanup(cancel)

	chain := []celeris.HandlerFunc{
		requestid.New(),
		recovery.New(recovery.Config{DisableLogStack: true}),
		cors.New(),
		secure.New(deepSecureConfig),
		ratelimit.New(ratelimit.Config{
			RPS:            1e9,
			Burst:          1 << 30,
			CleanupContext: cleanupCtx,
			DisableHeaders: true,
		}),
		timeout.New(timeout.Config{Timeout: 30 * time.Second}),
		etag.New(),
		textHandler,
	}
	opts := []celeristest.Option{
		celeristest.WithHandlers(chain...),
		celeristest.WithHeader("origin", "https://example.com"),
		celeristest.WithRemoteAddr("127.0.0.1:9999"),
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ctx, _ := celeristest.NewContext("GET", "/api/data", opts...)
			_ = ctx.Next()
			celeristest.ReleaseContext(ctx)
		}
	})
}

// --- v1.3.1 chain benchmarks ---

// BenchmarkChainProductionAPI benchmarks a realistic production API behind a reverse proxy:
// proxy -> redirect.HTTPS -> methodoverride -> requestid -> recovery -> cors -> secure -> ratelimit -> etag -> handler.
func BenchmarkChainProductionAPI(b *testing.B) {
	cleanupCtx, cancel := context.WithCancel(context.Background())
	b.Cleanup(cancel)

	chain := []celeris.HandlerFunc{
		proxy.New(proxy.Config{
			TrustedProxies: []string{"10.0.0.0/8"},
		}),
		redirect.HTTPSRedirect(),
		methodoverride.New(methodoverride.Config{
			Getter: methodoverride.HeaderGetter("X-HTTP-Method-Override"),
		}),
		requestid.New(),
		recovery.New(recovery.Config{DisableLogStack: true}),
		cors.New(),
		secure.New(deepSecureConfig),
		ratelimit.New(ratelimit.Config{
			RPS:            1e9,
			Burst:          1 << 30,
			CleanupContext: cleanupCtx,
			DisableHeaders: true,
		}),
		etag.New(),
		json1KBHandler,
	}
	opts := []celeristest.Option{
		celeristest.WithHandlers(chain...),
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-for", "203.0.113.50"),
		celeristest.WithScheme("https"),
		celeristest.WithHeader("origin", "https://app.example.com"),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/v2/users", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkChainStaticFile benchmarks a static file server chain:
// requestid -> recovery -> secure -> etag -> ~4KB HTML handler.
func BenchmarkChainStaticFile(b *testing.B) {
	chain := []celeris.HandlerFunc{
		requestid.New(),
		recovery.New(recovery.Config{DisableLogStack: true}),
		secure.New(deepSecureConfig),
		etag.New(),
		html4KBHandler,
	}
	opts := []celeristest.Option{
		celeristest.WithHandlers(chain...),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/index.html", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkChainStaticFile304 benchmarks the same static file chain with
// a matching If-None-Match header, exercising the 304 fast path.
func BenchmarkChainStaticFile304(b *testing.B) {
	chain := []celeris.HandlerFunc{
		requestid.New(),
		recovery.New(recovery.Config{DisableLogStack: true}),
		secure.New(deepSecureConfig),
		etag.New(),
		html4KBHandler,
	}
	opts := []celeristest.Option{
		celeristest.WithHandlers(chain...),
		celeristest.WithHeader("if-none-match", html4KBETag),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/index.html", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkChainWebhookReceiver benchmarks a POST webhook receiver:
// proxy -> methodoverride -> requestid -> recovery -> handler.
func BenchmarkChainWebhookReceiver(b *testing.B) {
	chain := []celeris.HandlerFunc{
		proxy.New(proxy.Config{
			TrustedProxies: []string{"10.0.0.0/8"},
		}),
		methodoverride.New(methodoverride.Config{
			Getter: methodoverride.HeaderGetter("X-HTTP-Method-Override"),
		}),
		requestid.New(),
		recovery.New(recovery.Config{DisableLogStack: true}),
		webhookHandler,
	}
	opts := []celeristest.Option{
		celeristest.WithHandlers(chain...),
		celeristest.WithRemoteAddr("10.0.0.1:4321"),
		celeristest.WithHeader("x-forwarded-for", "198.51.100.10"),
		celeristest.WithScheme("https"),
		celeristest.WithHeader("content-type", "application/json"),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("POST", "/webhooks/stripe", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkChainPreRoutingOnly benchmarks only the pre-routing middleware
// on their passthrough paths (HTTPS already set, no method override header):
// proxy -> redirect.HTTPSRedirect -> methodoverride -> handler.
func BenchmarkChainPreRoutingOnly(b *testing.B) {
	chain := []celeris.HandlerFunc{
		proxy.New(proxy.Config{
			TrustedProxies: []string{"10.0.0.0/8"},
		}),
		redirect.HTTPSRedirect(),
		methodoverride.New(methodoverride.Config{
			Getter: methodoverride.HeaderGetter("X-HTTP-Method-Override"),
		}),
		jsonHandler,
	}
	opts := []celeristest.Option{
		celeristest.WithHandlers(chain...),
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-for", "203.0.113.50"),
		celeristest.WithScheme("https"),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/users", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkChainETagMiss benchmarks isolated ETag with a ~1KB JSON body (200 response).
func BenchmarkChainETagMiss(b *testing.B) {
	chain := []celeris.HandlerFunc{
		etag.New(),
		json1KBHandler,
	}
	opts := []celeristest.Option{
		celeristest.WithHandlers(chain...),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/users", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkChainETagHit benchmarks isolated ETag with a matching If-None-Match (304 response).
func BenchmarkChainETagHit(b *testing.B) {
	chain := []celeris.HandlerFunc{
		etag.New(),
		json1KBHandler,
	}
	opts := []celeristest.Option{
		celeristest.WithHandlers(chain...),
		celeristest.WithHeader("if-none-match", json1KBETag),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/users", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkChainWithCircuitBreaker benchmarks: requestid -> recovery -> circuitbreaker -> timeout -> handler.
func BenchmarkChainWithCircuitBreaker(b *testing.B) {
	chain := []celeris.HandlerFunc{
		requestid.New(),
		recovery.New(recovery.Config{DisableLogStack: true}),
		circuitbreaker.New(),
		timeout.New(timeout.Config{Timeout: 30 * time.Second}),
		jsonHandler,
	}
	opts := []celeristest.Option{celeristest.WithHandlers(chain...)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/users", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

// BenchmarkChainWithSingleflight benchmarks: requestid -> recovery -> singleflight -> etag -> handler.
func BenchmarkChainWithSingleflight(b *testing.B) {
	chain := []celeris.HandlerFunc{
		requestid.New(),
		recovery.New(recovery.Config{DisableLogStack: true}),
		singleflight.New(),
		etag.New(),
		textHandler,
	}
	opts := []celeristest.Option{celeristest.WithHandlers(chain...)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/api/data", opts...)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}
