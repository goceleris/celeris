package csrf

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/extract"
	"github.com/goceleris/celeris/middleware/internal/randutil"
	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// validToken is a 64-char hex string representing a 32-byte raw token.
const validToken = "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

// --- Safe method tests ---

func TestSafeMethodGeneratesToken(t *testing.T) {
	mw := New()
	var token string
	handler := func(c *celeris.Context) error {
		token = TokenFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if token == "" {
		t.Fatal("expected non-empty CSRF token in context")
	}
	if len(token) != 64 { // 32 bytes = 64 hex chars
		t.Fatalf("token length: got %d, want 64", len(token))
	}
}

func TestSafeMethodSetsCookie(t *testing.T) {
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)

	found := false
	for _, h := range rec.Headers {
		if h[0] == "set-cookie" {
			found = true
			if len(h[1]) == 0 {
				t.Fatal("set-cookie header is empty")
			}
			break
		}
	}
	if !found {
		t.Fatal("expected set-cookie header for CSRF token")
	}
}

func TestSafeMethodReusesExistingCookie(t *testing.T) {
	mw := New()
	var token string
	handler := func(c *celeris.Context) error {
		token = TokenFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("_csrf", validToken),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if token != validToken {
		t.Fatalf("token %q does not match original %q", token, validToken)
	}
	found := false
	for _, h := range rec.Headers {
		if h[0] == "set-cookie" && strings.Contains(h[1], "_csrf=") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected set-cookie header for CSRF token")
	}
}

func TestSafeMethodGeneratesNewTokenWhenNoCookie(t *testing.T) {
	mw := New()
	var token1, token2 string
	handler1 := func(c *celeris.Context) error {
		token1 = TokenFromContext(c)
		return nil
	}
	handler2 := func(c *celeris.Context) error {
		token2 = TokenFromContext(c)
		return nil
	}
	chain1 := []celeris.HandlerFunc{mw, handler1}
	chain2 := []celeris.HandlerFunc{mw, handler2}
	_, err := testutil.RunChain(t, chain1, "GET", "/")
	testutil.AssertNoError(t, err)
	_, err = testutil.RunChain(t, chain2, "GET", "/")
	testutil.AssertNoError(t, err)
	if token1 == "" || token2 == "" {
		t.Fatal("expected non-empty tokens")
	}
	if token1 == token2 {
		t.Fatal("expected different tokens for requests without cookies")
	}
}

func TestSafeMethodVariants(t *testing.T) {
	tests := []struct {
		method string
	}{
		{"HEAD"},
		{"OPTIONS"},
		{"TRACE"},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			mw := New()
			var token string
			handler := func(c *celeris.Context) error {
				token = TokenFromContext(c)
				return nil
			}
			chain := []celeris.HandlerFunc{mw, handler}
			_, err := testutil.RunChain(t, chain, tt.method, "/")
			testutil.AssertNoError(t, err)
			if token == "" {
				t.Fatalf("expected token for %s request", tt.method)
			}
		})
	}
}

// --- Unsafe method tests ---

func TestUnsafeMethodValidatesToken(t *testing.T) {
	mw := New()
	var stored string
	handler := func(c *celeris.Context) error {
		stored = TokenFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", validToken),
		celeristest.WithCookie("_csrf", validToken),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if stored != validToken {
		t.Fatalf("context token: got %q, want %q", stored, validToken)
	}
}

func TestUnsafeMethodRejections(t *testing.T) {
	tests := []struct {
		name   string
		method string
		opts   []celeristest.Option
	}{
		{"missing cookie", "POST", []celeristest.Option{
			celeristest.WithHeader("x-csrf-token", "sometoken"),
		}},
		{"missing header", "POST", []celeristest.Option{
			celeristest.WithCookie("_csrf", "sometoken"),
		}},
		{"token mismatch", "POST", []celeristest.Option{
			celeristest.WithHeader("x-csrf-token", "aaaa"),
			celeristest.WithCookie("_csrf", "bbbb"),
		}},
		{"PUT without token", "PUT", []celeristest.Option{
			celeristest.WithCookie("_csrf", "tok"),
		}},
		{"DELETE without token", "DELETE", nil},
		{"PATCH without token", "PATCH", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := New()
			handler := func(c *celeris.Context) error {
				return c.String(200, "ok")
			}
			chain := []celeris.HandlerFunc{mw, handler}
			opts := append([]celeristest.Option{}, tt.opts...)
			_, err := testutil.RunChain(t, chain, tt.method, "/resource", opts...)
			testutil.AssertHTTPError(t, err, 403)
		})
	}
}

// --- TokenFromContext tests ---

func TestTokenFromContext(t *testing.T) {
	tests := []struct {
		name      string
		setup     func() (*celeris.Context, func())
		wantEmpty bool
	}{
		{
			"no token",
			func() (*celeris.Context, func()) {
				ctx, _ := celeristest.NewContextT(t, "GET", "/")
				return ctx, func() {}
			},
			true,
		},
		{
			"no handler in context",
			func() (*celeris.Context, func()) {
				ctx, _ := celeristest.NewContextT(t, "GET", "/")
				return ctx, func() {}
			},
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cleanup := tt.setup()
			defer cleanup()
			got := TokenFromContext(ctx)
			if tt.wantEmpty && got != "" {
				t.Fatalf("expected empty, got %q", got)
			}
		})
	}
}

func TestTokenFromContextWithMiddleware(t *testing.T) {
	mw := New()
	var token string
	handler := func(c *celeris.Context) error {
		token = TokenFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, _ = testutil.RunChain(t, chain, "GET", "/")
	if token == "" {
		t.Fatal("expected non-empty token from TokenFromContext")
	}
}

// --- Token lookup tests ---

func TestFormTokenLookup(t *testing.T) {
	mw := New(Config{TokenLookup: "form:_csrf"})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithCookie("_csrf", validToken),
		celeristest.WithContentType("application/x-www-form-urlencoded"),
		celeristest.WithBody([]byte("_csrf="+validToken)),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestQueryTokenLookup(t *testing.T) {
	mw := New(Config{TokenLookup: "query:csrf_token"})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithCookie("_csrf", validToken),
		celeristest.WithQuery("csrf_token", validToken),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestQueryTokenLookupMissing(t *testing.T) {
	mw := New(Config{TokenLookup: "query:csrf_token"})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithCookie("_csrf", "sometoken"),
	)
	testutil.AssertHTTPError(t, err, 403)
}

// --- Skip tests ---

func TestSkipFunction(t *testing.T) {
	mw := New(Config{
		Skip: func(_ *celeris.Context) bool { return true },
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "skipped")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/submit")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "skipped")
}

func TestSkipPaths(t *testing.T) {
	mw := New(Config{SkipPaths: []string{"/health", "/ready"}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := testutil.RunChain(t, chain, "POST", "/health")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	rec, err = testutil.RunChain(t, chain, "POST", "/ready")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	_, err = testutil.RunChain(t, chain, "POST", "/other")
	testutil.AssertHTTPError(t, err, 403)
}

// --- Error handler tests ---

func TestCustomErrorHandler(t *testing.T) {
	var receivedErr error
	mw := New(Config{
		ErrorHandler: func(_ *celeris.Context, err error) error {
			receivedErr = err
			return celeris.NewHTTPError(418, "custom error")
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/submit")
	testutil.AssertHTTPError(t, err, 418)
	if receivedErr == nil {
		t.Fatal("expected ErrorHandler to receive an error")
	}
}

func TestCustomErrorHandlerOnMismatch(t *testing.T) {
	var receivedErr error
	mw := New(Config{
		ErrorHandler: func(_ *celeris.Context, err error) error {
			receivedErr = err
			return celeris.NewHTTPError(418, "mismatch")
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", "aaa"),
		celeristest.WithCookie("_csrf", "bbb"),
	)
	testutil.AssertHTTPError(t, err, 418)
	if receivedErr != ErrForbidden {
		t.Fatalf("expected ErrForbidden, got %v", receivedErr)
	}
}

// --- Cookie option tests ---

func TestCookieOptionsAreSet(t *testing.T) {
	mw := New(Config{
		CookieName:     "my_csrf",
		CookiePath:     "/app",
		CookieDomain:   "example.com",
		CookieMaxAge:   3600,
		CookieSecure:   true,
		CookieHTTPOnly: true,
		CookieSameSite: celeris.SameSiteStrictMode,
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	found := false
	for _, h := range rec.Headers {
		if h[0] == "set-cookie" {
			found = true
			v := h[1]
			if !strings.Contains(v, "my_csrf=") {
				t.Fatalf("cookie name not found in set-cookie: %s", v)
			}
			if !strings.Contains(v, "Path=/app") {
				t.Fatalf("cookie path not found in set-cookie: %s", v)
			}
			if !strings.Contains(v, "Domain=example.com") {
				t.Fatalf("cookie domain not found in set-cookie: %s", v)
			}
			if !strings.Contains(v, "Max-Age=3600") {
				t.Fatalf("cookie max-age not found in set-cookie: %s", v)
			}
			if !strings.Contains(v, "Secure") {
				t.Fatalf("cookie Secure not found in set-cookie: %s", v)
			}
			if !strings.Contains(v, "HttpOnly") {
				t.Fatalf("cookie HttpOnly not found in set-cookie: %s", v)
			}
			if !strings.Contains(v, "SameSite=Strict") {
				t.Fatalf("cookie SameSite=Strict not found in set-cookie: %s", v)
			}
			break
		}
	}
	if !found {
		t.Fatal("expected set-cookie header")
	}
}

func TestSessionCookieMaxAgeZero(t *testing.T) {
	mw := New(Config{CookieMaxAge: 0})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	for _, h := range rec.Headers {
		if h[0] == "set-cookie" {
			if strings.Contains(h[1], "Max-Age") {
				t.Fatalf("expected no Max-Age in session cookie, got: %s", h[1])
			}
			return
		}
	}
	t.Fatal("expected set-cookie header")
}

// --- defaultConfig tests ---

func TestDefaultConfigValues(t *testing.T) {
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"TokenLength", defaultConfig.TokenLength, 32},
		{"TokenLookup", defaultConfig.TokenLookup, "header:X-CSRF-Token"},
		{"CookieName", defaultConfig.CookieName, "_csrf"},
		{"CookiePath", defaultConfig.CookiePath, "/"},
		{"CookieMaxAge", defaultConfig.CookieMaxAge, 86400},
		{"CookieHTTPOnly", defaultConfig.CookieHTTPOnly, true},
		{"CookieSameSite", defaultConfig.CookieSameSite, celeris.SameSiteLaxMode},
		{"Expiration", defaultConfig.Expiration, time.Hour},
		{"ContextKey", defaultConfig.ContextKey, "csrf_token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("got %v, want %v", tt.got, tt.want)
			}
		})
	}
}

func TestContextKeyConstant(t *testing.T) {
	if ContextKey != "csrf_token" {
		t.Fatalf("ContextKey: got %q, want %q", ContextKey, "csrf_token")
	}
}

// --- validate() / panic tests ---

func TestValidationPanics(t *testing.T) {
	tests := []struct {
		name   string
		action func()
	}{
		{"invalid TokenLookup format", func() { New(Config{TokenLookup: "invalid"}) }},
		{"unsupported TokenLookup source", func() { New(Config{TokenLookup: "socket:token"}) }},
		{"TokenLength exceeds 32", func() { New(Config{TokenLength: 33}) }},
		{"SameSite=None without Secure", func() {
			New(Config{CookieSameSite: celeris.SameSiteNoneMode, CookieSecure: false})
		}},
		{"SingleUseToken without Storage", func() { New(Config{SingleUseToken: true}) }},
		{"wildcard HTTP origin", func() {
			New(Config{TrustedOrigins: []string{"http://*.example.com"}})
		}},
		{"wildcard no-scheme origin", func() {
			New(Config{TrustedOrigins: []string{"*.example.com"}})
		}},
		{"mixed origins HTTP wildcard", func() {
			New(Config{TrustedOrigins: []string{"https://trusted.example.com", "http://*.evil.com"}})
		}},
		{"invalid segment in multi-extractor", func() {
			New(Config{TokenLookup: "header:X-CSRF-Token,bad"})
		}},
		{"invalid source in comma lookup", func() {
			New(Config{TokenLookup: "header:X-CSRF-Token,socket:val"})
		}},
		{"extract parse no colon", func() { extract.Parse("headeronly") }},
		{"extract parse unsupported source", func() { extract.Parse("socket:val") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic for %s", tt.name)
				}
			}()
			tt.action()
		})
	}
}

func TestValidationNoPanic(t *testing.T) {
	tests := []struct {
		name   string
		action func()
	}{
		{"query TokenLookup", func() { New(Config{TokenLookup: "query:token"}) }},
		{"SameSite=None with Secure", func() {
			New(Config{CookieSameSite: celeris.SameSiteNoneMode, CookieSecure: true})
		}},
		{"wildcard HTTPS origin", func() {
			New(Config{TrustedOrigins: []string{"https://*.example.com"}})
		}},
		{"exact HTTP origin", func() {
			New(Config{TrustedOrigins: []string{"http://trusted.example.com"}})
		}},
		{"exact HTTPS origin", func() {
			New(Config{TrustedOrigins: []string{"https://trusted.example.com"}})
		}},
		{"mixed valid origins", func() {
			New(Config{TrustedOrigins: []string{"http://trusted.example.com", "https://*.example.com"}})
		}},
		{"comma-separated lookup", func() {
			New(Config{TokenLookup: "header:X-CSRF-Token,form:_csrf"})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("unexpected panic: %v", r)
				}
			}()
			tt.action()
		})
	}
}

// --- applyDefaults tests ---

func TestApplyDefaults(t *testing.T) {
	tests := []struct {
		name  string
		input Config
		check func(t *testing.T, cfg Config)
	}{
		{"zero TokenLength", Config{TokenLength: 0}, func(t *testing.T, cfg Config) {
			if cfg.TokenLength != 32 {
				t.Fatalf("expected 32, got %d", cfg.TokenLength)
			}
		}},
		{"negative TokenLength", Config{TokenLength: -5}, func(t *testing.T, cfg Config) {
			if cfg.TokenLength != 32 {
				t.Fatalf("expected 32, got %d", cfg.TokenLength)
			}
		}},
		{"custom values preserved", Config{
			TokenLength: 16, TokenLookup: "form:csrf_tok",
			CookieName: "my_tok", CookiePath: "/api",
		}, func(t *testing.T, cfg Config) {
			if cfg.TokenLength != 16 {
				t.Fatalf("TokenLength: got %d", cfg.TokenLength)
			}
			if cfg.TokenLookup != "form:csrf_tok" {
				t.Fatalf("TokenLookup: got %q", cfg.TokenLookup)
			}
			if cfg.CookieName != "my_tok" {
				t.Fatalf("CookieName: got %q", cfg.CookieName)
			}
			if cfg.CookiePath != "/api" {
				t.Fatalf("CookiePath: got %q", cfg.CookiePath)
			}
		}},
		{"independent fields", Config{CookieSameSite: celeris.SameSiteStrictMode}, func(t *testing.T, cfg Config) {
			if len(cfg.SafeMethods) == 0 {
				t.Fatal("SafeMethods should be defaulted independently")
			}
			if cfg.CookieSameSite != celeris.SameSiteStrictMode {
				t.Fatalf("CookieSameSite: got %v", cfg.CookieSameSite)
			}
		}},
		{"default Expiration", Config{}, func(t *testing.T, cfg Config) {
			if cfg.Expiration != time.Hour {
				t.Fatalf("expected 1h, got %v", cfg.Expiration)
			}
		}},
		{"custom Expiration", Config{Expiration: 2 * time.Hour}, func(t *testing.T, cfg Config) {
			if cfg.Expiration != 2*time.Hour {
				t.Fatalf("expected 2h, got %v", cfg.Expiration)
			}
		}},
		{"default ContextKey", Config{}, func(t *testing.T, cfg Config) {
			if cfg.ContextKey != "csrf_token" {
				t.Fatalf("expected csrf_token, got %q", cfg.ContextKey)
			}
		}},
		{"custom ContextKey", Config{ContextKey: "custom"}, func(t *testing.T, cfg Config) {
			if cfg.ContextKey != "custom" {
				t.Fatalf("expected custom, got %q", cfg.ContextKey)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := applyDefaults(tt.input)
			tt.check(t, cfg)
		})
	}
}

// --- token generation tests ---

func TestGenerateTokenLength(t *testing.T) {
	tok := randutil.HexToken(16)
	if len(tok) != 32 {
		t.Fatalf("expected 32 hex chars for 16 bytes, got %d", len(tok))
	}
	tok = randutil.HexToken(32)
	if len(tok) != 64 {
		t.Fatalf("expected 64 hex chars for 32 bytes, got %d", len(tok))
	}
}

func TestGenerateTokenUniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for range 100 {
		tok := randutil.HexToken(32)
		if _, ok := seen[tok]; ok {
			t.Fatal("generated duplicate token")
		}
		seen[tok] = struct{}{}
	}
}

// --- Custom SafeMethods ---

func TestCustomSafeMethods(t *testing.T) {
	mw := New(Config{SafeMethods: []string{"GET"}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	// HEAD is no longer safe (not in custom list).
	_, err = testutil.RunChain(t, chain, "HEAD", "/")
	testutil.AssertHTTPError(t, err, 403)
}

// --- Vary header tests ---

func TestVaryCookieHeaderOnSafeMethod(t *testing.T) {
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "vary", "Cookie")
}

func TestVaryCookieHeaderOnUnsafeMethod(t *testing.T) {
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", validToken),
		celeristest.WithCookie("_csrf", validToken),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "vary", "Cookie")
}

func TestVaryHeaderDoesNotClobber(t *testing.T) {
	mw := New()
	preMiddleware := func(c *celeris.Context) error {
		c.AddHeader("vary", "Accept-Encoding")
		return c.Next()
	}
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{preMiddleware, mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	foundEncoding := false
	foundCookie := false
	for _, h := range rec.Headers {
		if h[0] == "vary" && h[1] == "Accept-Encoding" {
			foundEncoding = true
		}
		if h[0] == "vary" && h[1] == "Cookie" {
			foundCookie = true
		}
	}
	if !foundEncoding {
		t.Fatal("expected Vary: Accept-Encoding to be preserved")
	}
	if !foundCookie {
		t.Fatal("expected Vary: Cookie to be added")
	}
}

// --- Sec-Fetch-Site tests ---

func TestSecFetchSite(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		wantStatus int
	}{
		{"cross-site rejected", "cross-site", 403},
		{"same-origin allowed", "same-origin", 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := New()
			handler := func(c *celeris.Context) error {
				return c.String(200, "ok")
			}
			chain := []celeris.HandlerFunc{mw, handler}
			rec, err := testutil.RunChain(t, chain, "POST", "/submit",
				celeristest.WithHeader("x-csrf-token", validToken),
				celeristest.WithCookie("_csrf", validToken),
				celeristest.WithHeader("sec-fetch-site", tt.value),
			)
			if tt.wantStatus == 403 {
				testutil.AssertHTTPError(t, err, 403)
			} else {
				testutil.AssertNoError(t, err)
				testutil.AssertStatus(t, rec, 200)
			}
		})
	}
}

// --- Origin validation tests ---

func TestOriginValidation(t *testing.T) {
	tests := []struct {
		name       string
		origin     string
		wantStatus int
	}{
		{"mismatch rejected", "https://evil.com", 403},
		{"match allowed", "https://localhost", 200},
		{"no header allowed", "", 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := New()
			handler := func(c *celeris.Context) error {
				return c.String(200, "ok")
			}
			chain := []celeris.HandlerFunc{mw, handler}
			opts := []celeristest.Option{
				celeristest.WithHeader("x-csrf-token", validToken),
				celeristest.WithCookie("_csrf", validToken),
			}
			if tt.origin != "" {
				opts = append(opts, celeristest.WithHeader("origin", tt.origin))
			}
			rec, err := testutil.RunChain(t, chain, "POST", "/submit", opts...)
			if tt.wantStatus == 403 {
				testutil.AssertHTTPError(t, err, 403)
			} else {
				testutil.AssertNoError(t, err)
				testutil.AssertStatus(t, rec, 200)
			}
		})
	}
}

// --- Trusted origins tests ---

func TestTrustedOriginsAllowed(t *testing.T) {
	mw := New(Config{
		TrustedOrigins: []string{"https://trusted.example.com"},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", validToken),
		celeristest.WithCookie("_csrf", validToken),
		celeristest.WithHeader("origin", "https://trusted.example.com"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestTrustedOriginsUnlistedRejected(t *testing.T) {
	mw := New(Config{
		TrustedOrigins: []string{"https://trusted.example.com"},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", validToken),
		celeristest.WithCookie("_csrf", validToken),
		celeristest.WithHeader("origin", "https://untrusted.example.com"),
	)
	testutil.AssertHTTPError(t, err, 403)
}

// --- Error handler + sentinel tests ---

func TestErrorHandlerReceivesSentinels(t *testing.T) {
	tests := []struct {
		name    string
		opts    []celeristest.Option
		wantErr error
	}{
		{"ErrSecFetchSite", []celeristest.Option{
			celeristest.WithHeader("sec-fetch-site", "cross-site"),
		}, ErrSecFetchSite},
		{"ErrOriginMismatch", []celeristest.Option{
			celeristest.WithHeader("origin", "https://evil.com"),
		}, ErrOriginMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var receivedErr error
			mw := New(Config{
				ErrorHandler: func(_ *celeris.Context, err error) error {
					receivedErr = err
					return celeris.NewHTTPError(418, "custom")
				},
			})
			handler := func(c *celeris.Context) error {
				return c.String(200, "ok")
			}
			chain := []celeris.HandlerFunc{mw, handler}
			_, err := testutil.RunChain(t, chain, "POST", "/submit", tt.opts...)
			testutil.AssertHTTPError(t, err, 418)
			if !errors.Is(receivedErr, tt.wantErr) {
				t.Fatalf("expected %v, got %v", tt.wantErr, receivedErr)
			}
		})
	}
}

// --- Granular sentinel error tests ---

func TestGranularErrorSentinels(t *testing.T) {
	tests := []struct {
		name    string
		opts    []celeristest.Option
		wantErr error
	}{
		{"ErrSecFetchSite on cross-site", []celeristest.Option{
			celeristest.WithHeader("x-csrf-token", validToken),
			celeristest.WithCookie("_csrf", validToken),
			celeristest.WithHeader("sec-fetch-site", "cross-site"),
		}, ErrSecFetchSite},
		{"ErrOriginMismatch on bad origin", []celeristest.Option{
			celeristest.WithHeader("x-csrf-token", validToken),
			celeristest.WithCookie("_csrf", validToken),
			celeristest.WithHeader("origin", "https://evil.com"),
		}, ErrOriginMismatch},
		{"ErrRefererMissing on HTTPS no referer", []celeristest.Option{
			celeristest.WithHeader("x-csrf-token", validToken),
			celeristest.WithCookie("_csrf", validToken),
			celeristest.WithHeader("x-forwarded-proto", "https"),
		}, ErrRefererMissing},
		{"ErrRefererMismatch on HTTPS bad referer", []celeristest.Option{
			celeristest.WithHeader("x-csrf-token", validToken),
			celeristest.WithCookie("_csrf", validToken),
			celeristest.WithHeader("x-forwarded-proto", "https"),
			celeristest.WithHeader("referer", "https://evil.com/page"),
		}, ErrRefererMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := New()
			handler := func(c *celeris.Context) error {
				return c.String(200, "ok")
			}
			chain := []celeris.HandlerFunc{mw, handler}
			_, err := testutil.RunChain(t, chain, "POST", "/submit", tt.opts...)
			testutil.AssertHTTPError(t, err, 403)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected %v, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestGranularErrorSentinelsAreDistinct(t *testing.T) {
	sentinels := []error{
		ErrForbidden,
		ErrMissingToken,
		ErrTokenNotFound,
		ErrOriginMismatch,
		ErrRefererMissing,
		ErrRefererMismatch,
		ErrSecFetchSite,
	}
	seen := make(map[string]bool, len(sentinels))
	for _, e := range sentinels {
		msg := e.Error()
		if seen[msg] {
			t.Fatalf("duplicate sentinel message: %q", msg)
		}
		seen[msg] = true
	}
}

func TestIsOriginAllowed(t *testing.T) {
	trusted := map[string]struct{}{
		"https://app.example.com": {},
	}
	tests := []struct {
		name    string
		origin  string
		host    string
		allowed bool
	}{
		{"same host", "https://localhost", "localhost", true},
		{"same host with port", "https://localhost:8080", "localhost:8080", true},
		{"trusted origin", "https://app.example.com", "localhost", true},
		{"untrusted origin", "https://evil.com", "localhost", false},
		{"empty origin host", "data:text/html", "localhost", false},
		{"invalid origin", "://bad", "localhost", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isOriginAllowed(tt.origin, tt.host, trusted, nil)
			if got != tt.allowed {
				t.Fatalf("isOriginAllowed(%q, %q): got %v, want %v", tt.origin, tt.host, got, tt.allowed)
			}
		})
	}
}

// --- Referer tests ---

func TestRefererFallbackHTTPS(t *testing.T) {
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// HTTPS request with mismatched Referer and no Origin should be rejected.
	_, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", validToken),
		celeristest.WithCookie("_csrf", validToken),
		celeristest.WithHeader("x-forwarded-proto", "https"),
		celeristest.WithHeader("referer", "https://evil.com/page"),
	)
	testutil.AssertHTTPError(t, err, 403)
}

func TestRefererFallbackHTTPSMatchingHost(t *testing.T) {
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", validToken),
		celeristest.WithCookie("_csrf", validToken),
		celeristest.WithHeader("x-forwarded-proto", "https"),
		celeristest.WithHeader("referer", "https://localhost/page"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestRefererFallbackSkippedOnHTTP(t *testing.T) {
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", validToken),
		celeristest.WithCookie("_csrf", validToken),
		celeristest.WithHeader("referer", "https://evil.com/page"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestRefererFullURLExtractsOriginForComparison(t *testing.T) {
	mw := New(Config{
		TrustedOrigins: []string{"https://trusted.example.com"},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", validToken),
		celeristest.WithCookie("_csrf", validToken),
		celeristest.WithHeader("x-forwarded-proto", "https"),
		celeristest.WithHeader("referer", "https://trusted.example.com/some/path?q=1"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestRefererWithPathMatchesHost(t *testing.T) {
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", validToken),
		celeristest.WithCookie("_csrf", validToken),
		celeristest.WithHeader("x-forwarded-proto", "https"),
		celeristest.WithHeader("referer", "https://localhost/deep/path?key=value"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestRefererWildcardMatchesOriginOnly(t *testing.T) {
	mw := New(Config{
		TrustedOrigins: []string{"https://*.example.com"},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", validToken),
		celeristest.WithCookie("_csrf", validToken),
		celeristest.WithHeader("x-forwarded-proto", "https"),
		celeristest.WithHeader("referer", "https://app.example.com/api/v1/action"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

// --- Storage (server-side token) tests ---

func TestStorageSafeMethodStoresToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewMemoryStorage(MemoryStorageConfig{Shards: 1, CleanupContext: ctx})
	mw := New(Config{Storage: store})
	var token string
	handler := func(c *celeris.Context) error {
		token = TokenFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if token == "" {
		t.Fatal("expected token")
	}
	stored, ok := store.Get(storageKey(token))
	if !ok {
		t.Fatal("token not found in storage")
	}
	if stored != token {
		t.Fatalf("stored token %q != context token %q", stored, token)
	}
}

func TestStorageUnsafeMethodValidates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewMemoryStorage(MemoryStorageConfig{Shards: 1, CleanupContext: ctx})
	store.Set(storageKey(validToken), validToken, time.Hour)
	mw := New(Config{Storage: store})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", validToken),
		celeristest.WithCookie("_csrf", validToken),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestStorageUnsafeMethodRejectsExpiredToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewMemoryStorage(MemoryStorageConfig{Shards: 1, CleanupContext: ctx})
	store.Set(storageKey(validToken), validToken, time.Nanosecond)
	time.Sleep(time.Millisecond)
	mw := New(Config{Storage: store})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", validToken),
		celeristest.WithCookie("_csrf", validToken),
	)
	testutil.AssertHTTPError(t, err, 403)
}

func TestStorageUnsafeMethodRejectsMissingStorageToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewMemoryStorage(MemoryStorageConfig{Shards: 1, CleanupContext: ctx})
	// Token in cookie but NOT in storage.
	mw := New(Config{Storage: store})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", validToken),
		celeristest.WithCookie("_csrf", validToken),
	)
	testutil.AssertHTTPError(t, err, 403)
}

func TestStorageUnsafeMethodRejectsMismatchedRequestToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewMemoryStorage(MemoryStorageConfig{Shards: 1, CleanupContext: ctx})
	store.Set(storageKey(validToken), validToken, time.Hour)
	mw := New(Config{Storage: store})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", "wrong"),
		celeristest.WithCookie("_csrf", validToken),
	)
	testutil.AssertHTTPError(t, err, 403)
}

func TestStorageSafeMethodRegeneratesWhenNotInStorage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewMemoryStorage(MemoryStorageConfig{Shards: 1, CleanupContext: ctx})
	mw := New(Config{Storage: store})
	var token string
	handler := func(c *celeris.Context) error {
		token = TokenFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	oldCookie := "stale_cookie_value"
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("_csrf", oldCookie),
	)
	testutil.AssertNoError(t, err)
	if token == oldCookie {
		t.Fatal("expected new token, got stale cookie value")
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}
}

// --- SingleUseToken tests ---

func TestSingleUseTokenDeletesAfterValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewMemoryStorage(MemoryStorageConfig{Shards: 1, CleanupContext: ctx})
	store.Set(storageKey(validToken), validToken, time.Hour)
	mw := New(Config{Storage: store, SingleUseToken: true})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// First request: succeeds.
	rec, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", validToken),
		celeristest.WithCookie("_csrf", validToken),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	// Token should be deleted from storage.
	_, ok := store.Get(storageKey(validToken))
	if ok {
		t.Fatal("expected token to be deleted from storage after single use")
	}

	// Second request with same token: fails.
	_, err = testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", validToken),
		celeristest.WithCookie("_csrf", validToken),
	)
	testutil.AssertHTTPError(t, err, 403)
}

func TestSingleUseTokenAtomicGetAndDelete(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewMemoryStorage(MemoryStorageConfig{Shards: 1, CleanupContext: ctx})
	store.Set(storageKey(validToken), validToken, time.Hour)
	mw := New(Config{Storage: store, SingleUseToken: true})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", validToken),
		celeristest.WithCookie("_csrf", validToken),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	_, ok := store.Get(storageKey(validToken))
	if ok {
		t.Fatal("expected token deleted after single use")
	}
}

// --- Wildcard TrustedOrigins tests ---

func TestWildcardTrustedOrigins(t *testing.T) {
	tests := []struct {
		name       string
		origin     string
		wantStatus int
	}{
		{"subdomain allowed", "https://app.example.com", 200},
		{"deep subdomain rejected", "https://deep.sub.example.com", 403},
		{"no match rejected", "https://evil.com", 403},
		{"leading dot rejected", "https://.evil.example.com", 403},
		{"double dot rejected", "https://sub..evil.example.com", 403},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := New(Config{
				TrustedOrigins: []string{"https://*.example.com"},
			})
			handler := func(c *celeris.Context) error {
				return c.String(200, "ok")
			}
			chain := []celeris.HandlerFunc{mw, handler}
			rec, err := testutil.RunChain(t, chain, "POST", "/submit",
				celeristest.WithHeader("x-csrf-token", validToken),
				celeristest.WithCookie("_csrf", validToken),
				celeristest.WithHeader("origin", tt.origin),
			)
			if tt.wantStatus == 403 {
				testutil.AssertHTTPError(t, err, 403)
			} else {
				testutil.AssertNoError(t, err)
				testutil.AssertStatus(t, rec, 200)
			}
		})
	}
}

func TestWildcardTrustedOriginMixedWithExact(t *testing.T) {
	mw := New(Config{
		TrustedOrigins: []string{
			"https://exact.example.com",
			"https://*.wildcard.com",
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// Exact match.
	rec, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", validToken),
		celeristest.WithCookie("_csrf", validToken),
		celeristest.WithHeader("origin", "https://exact.example.com"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	// Wildcard match.
	rec, err = testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", validToken),
		celeristest.WithCookie("_csrf", validToken),
		celeristest.WithHeader("origin", "https://sub.wildcard.com"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestWildcardOriginMatch(t *testing.T) {
	tests := []struct {
		name    string
		pattern wildcardOrigin
		origin  string
		want    bool
	}{
		{"match", wildcardOrigin{"https://", ".example.com", 1}, "https://app.example.com", true},
		{"no match", wildcardOrigin{"https://", ".example.com", 1}, "https://evil.com", false},
		{"too short", wildcardOrigin{"https://", ".example.com", 1}, "https://.example.com", false},
		{"deep sub rejected", wildcardOrigin{"https://", ".example.com", 1}, "https://a.b.example.com", false},
		{"deep sub unlimited", wildcardOrigin{"https://", ".example.com", 0}, "https://a.b.example.com", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.pattern.match(tt.origin)
			if got != tt.want {
				t.Fatalf("match(%q): got %v, want %v", tt.origin, got, tt.want)
			}
		})
	}
}

// --- Multiple extractors tests ---

func TestMultipleExtractorsHeaderFirst(t *testing.T) {
	mw := New(Config{TokenLookup: "header:X-CSRF-Token,form:_csrf"})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithHeader("x-csrf-token", validToken),
		celeristest.WithCookie("_csrf", validToken),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestMultipleExtractorsFormFallback(t *testing.T) {
	mw := New(Config{TokenLookup: "header:X-CSRF-Token,form:_csrf"})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithCookie("_csrf", validToken),
		celeristest.WithContentType("application/x-www-form-urlencoded"),
		celeristest.WithBody([]byte("_csrf="+validToken)),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestMultipleExtractorsAllMissing(t *testing.T) {
	mw := New(Config{TokenLookup: "header:X-CSRF-Token,form:_csrf,query:token"})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithCookie("_csrf", "sometoken"),
	)
	testutil.AssertHTTPError(t, err, 403)
}

func TestMultipleExtractorsThreeSourcesQueryLast(t *testing.T) {
	mw := New(Config{TokenLookup: "header:X-CSRF-Token,form:_csrf,query:token"})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/submit",
		celeristest.WithCookie("_csrf", validToken),
		celeristest.WithQuery("token", validToken),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

// --- Custom KeyGenerator tests ---

func TestCustomKeyGenerator(t *testing.T) {
	var counter atomic.Int32
	mw := New(Config{
		KeyGenerator: func() string {
			counter.Add(1)
			return "custom-token-value"
		},
	})
	var token string
	handler := func(c *celeris.Context) error {
		token = TokenFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if token != "custom-token-value" {
		t.Fatalf("expected custom-token-value, got %q", token)
	}
	if counter.Load() != 1 {
		t.Fatalf("expected KeyGenerator called once, got %d", counter.Load())
	}
}

func TestCustomKeyGeneratorNotCalledWhenCookieExists(t *testing.T) {
	var counter atomic.Int32
	mw := New(Config{
		KeyGenerator: func() string {
			counter.Add(1)
			return "custom-token"
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("_csrf", "existing-token"),
	)
	testutil.AssertNoError(t, err)
	if counter.Load() != 0 {
		t.Fatalf("KeyGenerator should not be called when cookie exists, got %d calls", counter.Load())
	}
}

// --- Configurable ContextKey tests ---

func TestCustomContextKey(t *testing.T) {
	mw := New(Config{ContextKey: "my_token"})
	var token string
	handler := func(c *celeris.Context) error {
		v, ok := c.Get("my_token")
		if ok {
			token, _ = v.(string)
		}
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if token == "" {
		t.Fatal("expected token stored under custom context key")
	}
}

func TestDefaultContextKey(t *testing.T) {
	mw := New()
	var token string
	handler := func(c *celeris.Context) error {
		v, ok := c.Get("csrf_token")
		if ok {
			token, _ = v.(string)
		}
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	if token == "" {
		t.Fatal("expected token stored under default context key")
	}
}

func TestTokenFromContextCustomKey(t *testing.T) {
	mw := New(Config{ContextKey: "my_csrf_token"})
	var token string
	handler := func(c *celeris.Context) error {
		token = TokenFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if token == "" {
		t.Fatal("expected non-empty CSRF token from custom context key")
	}
}

func TestTokenFromContextDefaultKeyStillWorks(t *testing.T) {
	mw := New()
	var token string
	handler := func(c *celeris.Context) error {
		token = TokenFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if token == "" {
		t.Fatal("expected non-empty CSRF token from default context key")
	}
}

func TestHandlerStoresContextKey(t *testing.T) {
	customKey := "custom_key"
	mw := New(Config{ContextKey: customKey})
	var handlerKey string
	handler := func(c *celeris.Context) error {
		h := HandlerFromContext(c)
		if h != nil {
			handlerKey = h.contextKey
		}
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	if handlerKey != customKey {
		t.Fatalf("handler contextKey: got %q, want %q", handlerKey, customKey)
	}
}

// --- DeleteToken tests ---

func TestDeleteTokenWithStorage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewMemoryStorage(MemoryStorageConfig{Shards: 1, CleanupContext: ctx})
	store.Set(storageKey(validToken), validToken, time.Hour)
	mw := New(Config{Storage: store})
	handler := func(c *celeris.Context) error {
		err := DeleteToken(c)
		if err != nil {
			return err
		}
		return c.String(200, "deleted")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("_csrf", validToken),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	_, ok := store.Get(storageKey(validToken))
	if ok {
		t.Fatal("expected token to be deleted from storage")
	}

	for _, h := range rec.Headers {
		if h[0] == "set-cookie" && strings.Contains(h[1], "_csrf=;") {
			return
		}
	}
	t.Fatal("expected expiry set-cookie with empty value")
}

func TestDeleteTokenWithoutStorage(t *testing.T) {
	mw := New()
	handler := func(c *celeris.Context) error {
		err := DeleteToken(c)
		if err != nil {
			return err
		}
		return c.String(200, "deleted")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("_csrf", validToken),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	for _, h := range rec.Headers {
		if h[0] == "set-cookie" && strings.Contains(h[1], "_csrf=;") {
			return
		}
	}
	t.Fatal("expected expiry set-cookie with empty value")
}

func TestDeleteTokenNoCookieReturnsError(t *testing.T) {
	mw := New()
	handler := func(c *celeris.Context) error {
		return DeleteToken(c)
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertHTTPError(t, err, 404)
}

func TestDeleteTokenNoMiddlewareReturnsError(t *testing.T) {
	handler := func(c *celeris.Context) error {
		return DeleteToken(c)
	}
	chain := []celeris.HandlerFunc{handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertHTTPError(t, err, 404)
}

// --- HandlerFromContext tests ---

func TestHandlerFromContextReturnsHandler(t *testing.T) {
	mw := New()
	var h *Handler
	handler := func(c *celeris.Context) error {
		h = HandlerFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	if h == nil {
		t.Fatal("expected Handler from context")
	}
}

func TestHandlerFromContextNilWithoutMiddleware(t *testing.T) {
	handler := func(c *celeris.Context) error {
		h := HandlerFromContext(c)
		if h != nil {
			t.Fatal("expected nil Handler without middleware")
		}
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
}

func TestHandlerDeleteToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewMemoryStorage(MemoryStorageConfig{Shards: 1, CleanupContext: ctx})
	store.Set(storageKey(validToken), validToken, time.Hour)
	mw := New(Config{Storage: store})
	handler := func(c *celeris.Context) error {
		h := HandlerFromContext(c)
		if h == nil {
			t.Fatal("expected handler")
		}
		return h.DeleteToken(c)
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("_csrf", validToken),
	)
	testutil.AssertNoError(t, err)

	_, ok := store.Get(storageKey(validToken))
	if ok {
		t.Fatal("expected token deleted")
	}
}

// --- MemoryStorage tests ---

func TestMemoryStorageGetSetDelete(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewMemoryStorage(MemoryStorageConfig{Shards: 2, CleanupContext: ctx})

	_, ok := store.Get("key1")
	if ok {
		t.Fatal("expected not found")
	}

	store.Set("key1", "token1", time.Hour)
	val, ok := store.Get("key1")
	if !ok || val != "token1" {
		t.Fatalf("expected token1, got %q (ok=%v)", val, ok)
	}

	store.Delete("key1")
	_, ok = store.Get("key1")
	if ok {
		t.Fatal("expected not found after delete")
	}
}

func TestMemoryStorageExpiry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewMemoryStorage(MemoryStorageConfig{Shards: 1, CleanupContext: ctx})
	store.Set("key1", "token1", time.Nanosecond)
	time.Sleep(time.Millisecond)
	_, ok := store.Get("key1")
	if ok {
		t.Fatal("expected expired token not found")
	}
}

func TestMemoryStorageConcurrency(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewMemoryStorage(MemoryStorageConfig{Shards: 4, CleanupContext: ctx})
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := "key" + string(rune('a'+n%26))
			store.Set(key, "tok", time.Hour)
			store.Get(key)
			store.Delete(key)
		}(i)
	}
	wg.Wait()
}

func TestMemoryStorageDefaultShards(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewMemoryStorage(MemoryStorageConfig{CleanupContext: ctx})
	store.Set("k", "v", time.Hour)
	val, ok := store.Get("k")
	if !ok || val != "v" {
		t.Fatal("expected value with default shard count")
	}
}

func TestMemoryStorageGetAndDelete(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewMemoryStorage(MemoryStorageConfig{Shards: 2, CleanupContext: ctx})

	_, ok, err := store.GetAndDelete("missing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected not found")
	}

	store.Set("key1", "token1", time.Hour)
	val, ok, err := store.GetAndDelete("key1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || val != "token1" {
		t.Fatalf("expected token1, got %q (ok=%v)", val, ok)
	}

	_, ok = store.Get("key1")
	if ok {
		t.Fatal("expected key to be deleted after GetAndDelete")
	}
}

func TestMemoryStorageGetAndDeleteExpired(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewMemoryStorage(MemoryStorageConfig{Shards: 1, CleanupContext: ctx})
	store.Set("key1", "token1", time.Nanosecond)
	time.Sleep(time.Millisecond)
	_, ok, err := store.GetAndDelete("key1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected expired token not found")
	}
}

// --- extractOrigin tests ---

func TestExtractOrigin(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		want   string
	}{
		{"full URL with path", "https://example.com/path/to/page", "https://example.com"},
		{"full URL with port", "https://example.com:8080/path", "https://example.com:8080"},
		{"origin only", "https://example.com", "https://example.com"},
		{"http scheme", "http://example.com/page", "http://example.com"},
		{"invalid URL", "://bad", ""},
		{"no host", "data:text/html,hello", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractOrigin(tt.rawURL)
			if got != tt.want {
				t.Fatalf("extractOrigin(%q) = %q, want %q", tt.rawURL, got, tt.want)
			}
		})
	}
}

// --- storageKey tests ---

func TestStorageKeyIsHashed(t *testing.T) {
	token := "test-token"
	key := storageKey(token)
	if len(key) != 64 {
		t.Fatalf("expected 64-char hex digest, got %d chars", len(key))
	}
	if key == token {
		t.Fatal("storage key should be hashed, not plaintext")
	}
	if storageKey(token) != key {
		t.Fatal("storageKey is not deterministic")
	}
}

// --- isValidTokenHex tests ---

func TestIsValidTokenHex(t *testing.T) {
	tests := []struct {
		name  string
		token string
		n     int
		want  bool
	}{
		{"raw length valid", validToken, 32, true},
		{"wrong length", "abcdef", 32, false},
		{"non-hex chars", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", 32, false},
		{"empty", "", 32, false},
		{"uppercase hex", "ABCDEF1234567890ABCDEF1234567890ABCDEF1234567890ABCDEF1234567890", 32, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isValidTokenHex(tt.token, tt.n)
			if got != tt.want {
				t.Fatalf("isValidTokenHex(%q, %d) = %v, want %v", tt.token, tt.n, got, tt.want)
			}
		})
	}
}

// --- Cookie-injection defense tests ---

func TestCookieInjectionDefense(t *testing.T) {
	tests := []struct {
		name       string
		cookie     string
		header     string
		wantStatus int
	}{
		{"non-hex rejected", "not-hex-at-all!!", "not-hex-at-all!!", 403},
		{"wrong length rejected", "abcdef", "abcdef", 403},
		{"valid hex raw accepted", validToken, validToken, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := New()
			handler := func(c *celeris.Context) error {
				return c.String(200, "ok")
			}
			chain := []celeris.HandlerFunc{mw, handler}
			rec, err := testutil.RunChain(t, chain, "POST", "/submit",
				celeristest.WithHeader("x-csrf-token", tt.header),
				celeristest.WithCookie("_csrf", tt.cookie),
			)
			if tt.wantStatus == 403 {
				testutil.AssertHTTPError(t, err, 403)
			} else {
				testutil.AssertNoError(t, err)
				testutil.AssertStatus(t, rec, 200)
			}
		})
	}
}

// --- Extractor self-sabotage validation tests ---

func TestValidateExtractorCookieSelfSabotagePanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when TokenLookup extracts from the CSRF cookie itself")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "defeats the double-submit pattern") {
			t.Fatalf("expected descriptive panic message, got: %v", r)
		}
	}()
	cfg := Config{TokenLookup: "cookie:_csrf"}
	cfg = applyDefaults(cfg)
	cfg.validate()
}

func TestValidateExtractorCookieCustomNameSelfSabotagePanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when TokenLookup extracts from custom CSRF cookie")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "defeats the double-submit pattern") {
			t.Fatalf("expected descriptive panic message, got: %v", r)
		}
	}()
	cfg := Config{
		TokenLookup: "cookie:my_token",
		CookieName:  "my_token",
	}
	cfg = applyDefaults(cfg)
	cfg.validate()
}

func TestValidateExtractorCookieDifferentNameDoesNotPanic(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for unsupported source 'cookie'")
		}
		msg, ok := r.(string)
		if ok && strings.Contains(msg, "defeats the double-submit pattern") {
			t.Fatal("self-sabotage check should not fire for different cookie name")
		}
	}()
	cfg := Config{TokenLookup: "cookie:different_name"}
	cfg = applyDefaults(cfg)
	cfg.validate()
}

// --- Safe-method does not refresh expiry for existing tokens ---

func TestSafeMethodDoesNotRefreshExpiryForExistingToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewMemoryStorage(MemoryStorageConfig{Shards: 1, CleanupContext: ctx})

	store.Set(storageKey(validToken), validToken, 100*time.Millisecond)

	mw := New(Config{Storage: store, Expiration: time.Hour})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("_csrf", validToken),
	)
	testutil.AssertNoError(t, err)

	time.Sleep(150 * time.Millisecond)

	_, ok := store.Get(storageKey(validToken))
	if ok {
		t.Fatal("expected token to expire (safe method should not refresh expiry)")
	}
}
