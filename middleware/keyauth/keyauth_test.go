package keyauth

import (
	"crypto/subtle"
	"errors"
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

func validatorFor(expected string) func(*celeris.Context, string) (bool, error) {
	want := []byte(expected)
	return func(_ *celeris.Context, key string) (bool, error) {
		return subtle.ConstantTimeCompare([]byte(key), want) == 1, nil
	}
}

func TestHeaderExtractionDefault(t *testing.T) {
	mw := New(Config{Validator: validatorFor("test-key")})
	var storedKey string
	handler := func(c *celeris.Context) error {
		storedKey = KeyFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-api-key", "test-key"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
	if storedKey != "test-key" {
		t.Fatalf("key in context: got %q, want %q", storedKey, "test-key")
	}
}

func TestQueryExtraction(t *testing.T) {
	mw := New(Config{
		KeyLookup: "query:api_key",
		Validator: validatorFor("qkey"),
	})
	var storedKey string
	handler := func(c *celeris.Context) error {
		storedKey = KeyFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithQuery("api_key", "qkey"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if storedKey != "qkey" {
		t.Fatalf("key in context: got %q, want %q", storedKey, "qkey")
	}
}

func TestCookieExtraction(t *testing.T) {
	mw := New(Config{
		KeyLookup: "cookie:auth_token",
		Validator: validatorFor("ckey"),
	})
	var storedKey string
	handler := func(c *celeris.Context) error {
		storedKey = KeyFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithCookie("auth_token", "ckey"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if storedKey != "ckey" {
		t.Fatalf("key in context: got %q, want %q", storedKey, "ckey")
	}
}

func TestMissingKeyReturnsErrMissingKey(t *testing.T) {
	mw := New(Config{Validator: validatorFor("test-key")})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertHTTPError(t, err, 401)
	if !errors.Is(err, ErrMissingKey) {
		t.Fatalf("expected ErrMissingKey, got %v", err)
	}
}

func TestInvalidKeyReturnsErrUnauthorized(t *testing.T) {
	mw := New(Config{Validator: validatorFor("correct-key")})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-api-key", "wrong-key"),
	)
	testutil.AssertHTTPError(t, err, 401)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestStaticKeysSingleValid(t *testing.T) {
	mw := New(Config{Validator: StaticKeys("key-alpha")})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-api-key", "key-alpha"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestStaticKeysMultipleValid(t *testing.T) {
	mw := New(Config{Validator: StaticKeys("key-1", "key-2", "key-3")})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	for _, key := range []string{"key-1", "key-2", "key-3"} {
		rec, err := testutil.RunChain(t, chain, "GET", "/",
			celeristest.WithHeader("x-api-key", key),
		)
		testutil.AssertNoError(t, err)
		testutil.AssertStatus(t, rec, 200)
	}
}

func TestStaticKeysInvalid(t *testing.T) {
	mw := New(Config{Validator: StaticKeys("key-1", "key-2")})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-api-key", "key-wrong"),
	)
	testutil.AssertHTTPError(t, err, 401)
}

func TestStaticKeysDifferentLengths(t *testing.T) {
	// Keys of different lengths must all be compared timing-safely.
	mw := New(Config{Validator: StaticKeys("short", "medium-key", "a-very-long-api-key")})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	for _, key := range []string{"short", "medium-key", "a-very-long-api-key"} {
		rec, err := testutil.RunChain(t, chain, "GET", "/",
			celeristest.WithHeader("x-api-key", key),
		)
		testutil.AssertNoError(t, err)
		testutil.AssertStatus(t, rec, 200)
	}

	// Wrong key (same length as one valid key) should fail.
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-api-key", "xxxxx"),
	)
	testutil.AssertHTTPError(t, err, 401)

	// Wrong key (different length from all) should fail.
	_, err = testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-api-key", "ab"),
	)
	testutil.AssertHTTPError(t, err, 401)
}

func TestBearerPrefixStripping(t *testing.T) {
	mw := New(Config{
		KeyLookup: "header:Authorization:Bearer ",
		Validator: validatorFor("my-token"),
	})
	var storedKey string
	handler := func(c *celeris.Context) error {
		storedKey = KeyFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer my-token"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if storedKey != "my-token" {
		t.Fatalf("key in context: got %q, want %q", storedKey, "my-token")
	}
}

func TestBearerPrefixMissing(t *testing.T) {
	mw := New(Config{
		KeyLookup: "header:Authorization:Bearer ",
		Validator: validatorFor("my-token"),
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// Without the Bearer prefix, key extraction returns empty.
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "my-token"),
	)
	testutil.AssertHTTPError(t, err, 401)
	if !errors.Is(err, ErrMissingKey) {
		t.Fatalf("expected ErrMissingKey, got %v", err)
	}
}

func TestMultiSourceExtraction(t *testing.T) {
	mw := New(Config{
		KeyLookup: "header:Authorization:Bearer ,query:api_key",
		Validator: validatorFor("my-token"),
	})
	var storedKey string
	handler := func(c *celeris.Context) error {
		storedKey = KeyFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// First source (header) should work.
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer my-token"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if storedKey != "my-token" {
		t.Fatalf("key from header: got %q, want %q", storedKey, "my-token")
	}

	// Second source (query) fallback should work.
	storedKey = ""
	rec, err = testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithQuery("api_key", "my-token"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if storedKey != "my-token" {
		t.Fatalf("key from query: got %q, want %q", storedKey, "my-token")
	}

	// No source provides a key.
	_, err = testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertHTTPError(t, err, 401)
}

func TestCustomErrorHandler(t *testing.T) {
	mw := New(Config{
		Validator: validatorFor("correct"),
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return celeris.NewHTTPError(403, "Forbidden")
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-api-key", "wrong"),
	)
	testutil.AssertHTTPError(t, err, 403)
}

func TestCustomErrorHandlerOnMissingKey(t *testing.T) {
	mw := New(Config{
		Validator: validatorFor("correct"),
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return celeris.NewHTTPError(400, "Key required")
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertHTTPError(t, err, 400)
}

func TestSkipBypassesAuth(t *testing.T) {
	mw := New(Config{
		Validator: validatorFor("test-key"),
		Skip:      func(_ *celeris.Context) bool { return true },
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "skipped")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "skipped")
}

func TestSkipPathsBypassesAuth(t *testing.T) {
	mw := New(Config{
		Validator: validatorFor("test-key"),
		SkipPaths: []string{"/health", "/ready"},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := testutil.RunChain(t, chain, "GET", "/health")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	rec, err = testutil.RunChain(t, chain, "GET", "/ready")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	_, err = testutil.RunChain(t, chain, "GET", "/protected")
	testutil.AssertHTTPError(t, err, 401)
}

func TestKeyFromContextNoAuth(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "GET", "/")
	if got := KeyFromContext(ctx); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestKeyFromContextWithAuth(t *testing.T) {
	mw := New(Config{Validator: validatorFor("my-key")})
	var key string
	handler := func(c *celeris.Context) error {
		key = KeyFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, _ = testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-api-key", "my-key"),
	)
	if key != "my-key" {
		t.Fatalf("KeyFromContext: got %q, want %q", key, "my-key")
	}
}

func TestNilValidatorPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil Validator")
		}
	}()
	New(Config{Validator: nil})
}

func TestInvalidKeyLookupFormatPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for invalid KeyLookup format")
		}
	}()
	New(Config{
		KeyLookup: "bad",
		Validator: validatorFor("x"),
	})
}

func TestUnsupportedKeyLookupSourcePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for unsupported KeyLookup source")
		}
	}()
	New(Config{
		KeyLookup: "redis:token",
		Validator: validatorFor("x"),
	})
}

func TestApplyDefaultsFixesEmptyKeyLookup(t *testing.T) {
	cfg := applyDefaults(Config{KeyLookup: ""})
	if cfg.KeyLookup != "header:X-API-Key" {
		t.Fatalf("expected header:X-API-Key, got %q", cfg.KeyLookup)
	}
}

func TestValidatorErrorPropagated(t *testing.T) {
	dbErr := errors.New("db connection failed")
	mw := New(Config{
		Validator: func(_ *celeris.Context, _ string) (bool, error) {
			return false, dbErr
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-api-key", "some-key"),
	)
	if !errors.Is(err, dbErr) {
		t.Fatalf("expected db error, got %v", err)
	}
}

func TestSkipDoesNotStoreKey(t *testing.T) {
	mw := New(Config{
		Validator: validatorFor("test-key"),
		Skip:      func(_ *celeris.Context) bool { return true },
	})
	var key string
	handler := func(c *celeris.Context) error {
		key = KeyFromContext(c)
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, _ = testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-api-key", "test-key"),
	)
	if key != "" {
		t.Fatalf("expected empty key on skip, got %q", key)
	}
}

func TestStaticKeysEmpty(t *testing.T) {
	mw := New(Config{Validator: StaticKeys()})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-api-key", "anything"),
	)
	testutil.AssertHTTPError(t, err, 401)
}

// --- Form extractor tests ---

func TestFormExtraction(t *testing.T) {
	mw := New(Config{
		KeyLookup: "form:api_key",
		Validator: validatorFor("form-key"),
	})
	var storedKey string
	handler := func(c *celeris.Context) error {
		storedKey = KeyFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithContentType("application/x-www-form-urlencoded"),
		celeristest.WithBody([]byte("api_key=form-key")),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if storedKey != "form-key" {
		t.Fatalf("key from form: got %q, want %q", storedKey, "form-key")
	}
}

func TestFormExtractionMissing(t *testing.T) {
	mw := New(Config{
		KeyLookup: "form:api_key",
		Validator: validatorFor("form-key"),
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithContentType("application/x-www-form-urlencoded"),
		celeristest.WithBody([]byte("other=value")),
	)
	testutil.AssertHTTPError(t, err, 401)
}

// --- Param extractor tests ---

func TestParamExtraction(t *testing.T) {
	mw := New(Config{
		KeyLookup: "param:token",
		Validator: validatorFor("param-key"),
	})
	var storedKey string
	handler := func(c *celeris.Context) error {
		storedKey = KeyFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/api/param-key",
		celeristest.WithParam("token", "param-key"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if storedKey != "param-key" {
		t.Fatalf("key from param: got %q, want %q", storedKey, "param-key")
	}
}

func TestParamExtractionMissing(t *testing.T) {
	mw := New(Config{
		KeyLookup: "param:token",
		Validator: validatorFor("x"),
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/api/")
	testutil.AssertHTTPError(t, err, 401)
}

// --- SuccessHandler tests ---

func TestSuccessHandler(t *testing.T) {
	var called bool
	mw := New(Config{
		Validator: validatorFor("test-key"),
		SuccessHandler: func(c *celeris.Context) {
			called = true
			c.Set("tenant", "acme")
		},
	})
	var tenant string
	handler := func(c *celeris.Context) error {
		v, _ := c.Get("tenant")
		if v != nil {
			tenant = v.(string)
		}
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-api-key", "test-key"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if !called {
		t.Fatal("expected SuccessHandler to be called")
	}
	if tenant != "acme" {
		t.Fatalf("tenant: got %q, want %q", tenant, "acme")
	}
}

func TestSuccessHandlerNotCalledOnFailure(t *testing.T) {
	var called bool
	mw := New(Config{
		Validator: validatorFor("correct"),
		SuccessHandler: func(_ *celeris.Context) {
			called = true
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-api-key", "wrong"),
	)
	testutil.AssertHTTPError(t, err, 401)
	if called {
		t.Fatal("expected SuccessHandler NOT to be called on auth failure")
	}
}

func TestSuccessHandlerNilIsNoOp(t *testing.T) {
	mw := New(Config{
		Validator:      validatorFor("test-key"),
		SuccessHandler: nil,
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-api-key", "test-key"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

// --- WWW-Authenticate header tests ---

func TestWWWAuthenticateDefaultScheme(t *testing.T) {
	mw := New(Config{Validator: validatorFor("correct")})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithHandlers(chain...),
		celeristest.WithHeader("x-api-key", "wrong"),
	)
	err := ctx.Next()
	testutil.AssertHTTPError(t, err, 401)

	found := false
	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "www-authenticate" {
			if h[1] != `ApiKey realm="Restricted"` {
				t.Fatalf("www-authenticate: got %q, want %q", h[1], `ApiKey realm="Restricted"`)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected www-authenticate response header")
	}
}

func TestWWWAuthenticateCustomScheme(t *testing.T) {
	mw := New(Config{
		Validator:  validatorFor("correct"),
		AuthScheme: "Bearer",
		Realm:      "MyApp",
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithHandlers(chain...),
	)
	err := ctx.Next()
	testutil.AssertHTTPError(t, err, 401)

	found := false
	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "www-authenticate" {
			expected := `Bearer realm="MyApp"`
			if h[1] != expected {
				t.Fatalf("www-authenticate: got %q, want %q", h[1], expected)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected www-authenticate response header")
	}
}

func TestWWWAuthenticateOnMissingKey(t *testing.T) {
	mw := New(Config{Validator: validatorFor("correct")})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithHandlers(chain...),
	)
	err := ctx.Next()
	testutil.AssertHTTPError(t, err, 401)

	found := false
	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "www-authenticate" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected www-authenticate header on missing key")
	}
}

func TestWWWAuthenticateNotSetOnSuccess(t *testing.T) {
	mw := New(Config{Validator: validatorFor("test-key")})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithHandlers(chain...),
		celeristest.WithHeader("x-api-key", "test-key"),
	)
	err := ctx.Next()
	testutil.AssertNoError(t, err)

	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "www-authenticate" {
			t.Fatal("expected no www-authenticate header on successful auth")
		}
	}
}

func TestDefaultRealm(t *testing.T) {
	cfg := applyDefaults(Config{Validator: validatorFor("x")})
	if cfg.Realm != "Restricted" {
		t.Fatalf("Realm: got %q, want %q", cfg.Realm, "Restricted")
	}
}

func TestCustomRealm(t *testing.T) {
	cfg := applyDefaults(Config{Validator: validatorFor("x"), Realm: "Admin"})
	if cfg.Realm != "Admin" {
		t.Fatalf("Realm: got %q, want %q", cfg.Realm, "Admin")
	}
}

func TestWWWAuthenticateValue(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"default scheme", Config{Realm: "Restricted"}, `ApiKey realm="Restricted"`},
		{"bearer", Config{AuthScheme: "Bearer", Realm: "MyApp"}, `Bearer realm="MyApp"`},
		{"token", Config{AuthScheme: "Token", Realm: "API"}, `Token realm="API"`},
	}
	for _, tt := range tests {
		got := wwwAuthenticateValue(tt.cfg)
		if got != tt.want {
			t.Errorf("wwwAuthenticateValue(%s) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestMultiSourceWithFormFallback(t *testing.T) {
	mw := New(Config{
		KeyLookup: "header:X-API-Key,form:api_key",
		Validator: validatorFor("fallback-key"),
	})
	var storedKey string
	handler := func(c *celeris.Context) error {
		storedKey = KeyFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// Form fallback.
	rec, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithContentType("application/x-www-form-urlencoded"),
		celeristest.WithBody([]byte("api_key=fallback-key")),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if storedKey != "fallback-key" {
		t.Fatalf("key from form fallback: got %q, want %q", storedKey, "fallback-key")
	}
}

// --- RFC 6750 WWW-Authenticate error field tests ---

func TestWWWAuthenticateRFC6750AllFields(t *testing.T) {
	mw := New(Config{
		Validator:  validatorFor("correct"),
		AuthScheme: "Bearer",
		Realm:      "MyAPI",
		ChallengeParams: map[string]string{
			"error":             "invalid_token",
			"error_description": "The access token expired",
			"error_uri":         "https://example.com/error",
			"scope":             "read write",
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithHandlers(chain...),
	)
	err := ctx.Next()
	testutil.AssertHTTPError(t, err, 401)

	expected := `Bearer realm="MyAPI", error="invalid_token", error_description="The access token expired", error_uri="https://example.com/error", scope="read write"`
	found := false
	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "www-authenticate" {
			if h[1] != expected {
				t.Fatalf("www-authenticate:\n  got  %q\n  want %q", h[1], expected)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected www-authenticate header")
	}
}

func TestWWWAuthenticateRFC6750ErrorOnly(t *testing.T) {
	mw := New(Config{
		Validator: validatorFor("correct"),
		ChallengeParams: map[string]string{
			"error": "insufficient_scope",
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithHandlers(chain...),
	)
	err := ctx.Next()
	testutil.AssertHTTPError(t, err, 401)

	expected := `ApiKey realm="Restricted", error="insufficient_scope"`
	found := false
	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "www-authenticate" {
			if h[1] != expected {
				t.Fatalf("www-authenticate:\n  got  %q\n  want %q", h[1], expected)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected www-authenticate header")
	}
}

func TestWWWAuthenticateRFC6750ScopeOnly(t *testing.T) {
	mw := New(Config{
		Validator:  validatorFor("correct"),
		AuthScheme: "Bearer",
		ChallengeParams: map[string]string{
			"scope": "admin",
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithHandlers(chain...),
	)
	err := ctx.Next()
	testutil.AssertHTTPError(t, err, 401)

	expected := `Bearer realm="Restricted", scope="admin"`
	found := false
	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "www-authenticate" {
			if h[1] != expected {
				t.Fatalf("www-authenticate:\n  got  %q\n  want %q", h[1], expected)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected www-authenticate header")
	}
}

func TestWWWAuthenticateValueRFC6750(t *testing.T) {
	got := wwwAuthenticateValue(Config{
		AuthScheme: "Bearer",
		Realm:      "API",
		ChallengeParams: map[string]string{
			"error":             "invalid_request",
			"error_description": "missing token",
			"error_uri":         "https://docs.example.com/auth",
			"scope":             "read",
		},
	})
	expected := `Bearer realm="API", error="invalid_request", error_description="missing token", error_uri="https://docs.example.com/auth", scope="read"`
	if got != expected {
		t.Fatalf("wwwAuthenticateValue:\n  got  %q\n  want %q", got, expected)
	}
}

func TestChallengeParamsRealmKeyPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for ChallengeParams with 'realm' key")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "realm") {
			t.Fatalf("unexpected panic message: %v", r)
		}
	}()
	New(Config{
		Validator:       validatorFor("x"),
		ChallengeParams: map[string]string{"realm": "evil"},
	})
}

func TestChallengeParamsAuthSchemeKeyPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for ChallengeParams with 'auth_scheme' key")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "auth_scheme") {
			t.Fatalf("unexpected panic message: %v", r)
		}
	}()
	New(Config{
		Validator:       validatorFor("x"),
		ChallengeParams: map[string]string{"auth_scheme": "Bearer"},
	})
}

func TestChallengeParamsErrorInvalidValuePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for invalid ChallengeParams error")
		}
	}()
	New(Config{
		Validator:       validatorFor("x"),
		ChallengeParams: map[string]string{"error": "bad_value"},
	})
}

func TestChallengeParamsErrorURIInvalidPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for invalid ChallengeParams error_uri")
		}
	}()
	New(Config{
		Validator:       validatorFor("x"),
		ChallengeParams: map[string]string{"error_uri": "not-a-uri"},
	})
}

func TestChallengeParamsErrorURIRelativePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for relative ChallengeParams error_uri")
		}
	}()
	New(Config{
		Validator:       validatorFor("x"),
		ChallengeParams: map[string]string{"error_uri": "/relative/path"},
	})
}

func TestChallengeParamsErrorValidValues(_ *testing.T) {
	for _, v := range []string{"invalid_request", "invalid_token", "insufficient_scope"} {
		// Should not panic.
		New(Config{
			Validator:       validatorFor("x"),
			ChallengeParams: map[string]string{"error": v},
		})
	}
}

// --- ContinueOnIgnoredError tests ---

func TestContinueOnIgnoredErrorMissingKey(t *testing.T) {
	mw := New(Config{
		Validator: validatorFor("test-key"),
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return nil // ignore the error
		},
		ContinueOnIgnoredError: true,
	})
	var reached bool
	handler := func(c *celeris.Context) error {
		reached = true
		return c.String(200, "public")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "public")
	if !reached {
		t.Fatal("expected handler to be reached with ContinueOnIgnoredError")
	}
}

func TestContinueOnIgnoredErrorInvalidKey(t *testing.T) {
	mw := New(Config{
		Validator: validatorFor("correct"),
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return nil
		},
		ContinueOnIgnoredError: true,
	})
	var reached bool
	handler := func(c *celeris.Context) error {
		reached = true
		return c.String(200, "public")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-api-key", "wrong"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if !reached {
		t.Fatal("expected handler to be reached")
	}
}

func TestContinueOnIgnoredErrorNotSetAborts(t *testing.T) {
	mw := New(Config{
		Validator: validatorFor("test-key"),
		ErrorHandler: func(c *celeris.Context, _ error) error {
			c.Abort()
			return nil
		},
		ContinueOnIgnoredError: false,
	})
	var reached bool
	handler := func(c *celeris.Context) error {
		reached = true
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	if reached {
		t.Fatal("expected handler NOT to be reached when ErrorHandler aborts")
	}
}

func TestContinueOnIgnoredErrorHandlerReturnsError(t *testing.T) {
	mw := New(Config{
		Validator: validatorFor("correct"),
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return celeris.NewHTTPError(403, "Forbidden")
		},
		ContinueOnIgnoredError: true,
	})
	var reached bool
	handler := func(c *celeris.Context) error {
		reached = true
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertHTTPError(t, err, 403)
	if reached {
		t.Fatal("expected handler NOT to be reached when ErrorHandler returns non-nil")
	}
}

func TestContinueOnIgnoredErrorKeyNotStored(t *testing.T) {
	mw := New(Config{
		Validator: validatorFor("correct"),
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return nil
		},
		ContinueOnIgnoredError: true,
	})
	var key string
	handler := func(c *celeris.Context) error {
		key = KeyFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-api-key", "wrong"),
	)
	testutil.AssertNoError(t, err)
	if key != "" {
		t.Fatalf("expected empty key when auth failed but continued, got %q", key)
	}
}

func TestContinueOnIgnoredErrorValidKeyStillWorks(t *testing.T) {
	mw := New(Config{
		Validator: validatorFor("correct"),
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return nil
		},
		ContinueOnIgnoredError: true,
	})
	var key string
	handler := func(c *celeris.Context) error {
		key = KeyFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-api-key", "correct"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if key != "correct" {
		t.Fatalf("expected key %q, got %q", "correct", key)
	}
}

// --- Issue #6: Realm/Scope/Description escaping ---

func TestWWWAuthenticateScopeValidTokens(t *testing.T) {
	got := wwwAuthenticateValue(Config{
		Realm:      "API",
		AuthScheme: "Bearer",
		ChallengeParams: map[string]string{
			"scope": "read write",
		},
	})
	expected := `Bearer realm="API", scope="read write"`
	if got != expected {
		t.Fatalf("wwwAuthenticateValue:\n  got  %q\n  want %q", got, expected)
	}
}

func TestWWWAuthenticateEscapesDescription(t *testing.T) {
	got := wwwAuthenticateValue(Config{
		Realm: "API",
		ChallengeParams: map[string]string{
			"error":             "invalid_token",
			"error_description": `token contains "bad" chars`,
		},
	})
	expected := `ApiKey realm="API", error="invalid_token", error_description="token contains \"bad\" chars"`
	if got != expected {
		t.Fatalf("wwwAuthenticateValue:\n  got  %q\n  want %q", got, expected)
	}
}

// --- Issue #7: AuthScheme validation ---

func TestAuthSchemeWithSpacePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for AuthScheme with space")
		}
	}()
	New(Config{
		Validator:  validatorFor("x"),
		AuthScheme: "Bearer Token",
	})
}

func TestAuthSchemeWithQuotePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for AuthScheme with quote")
		}
	}()
	New(Config{
		Validator:  validatorFor("x"),
		AuthScheme: `Be"arer`,
	})
}

func TestAuthSchemeWithControlCharPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for AuthScheme with control char")
		}
	}()
	New(Config{
		Validator:  validatorFor("x"),
		AuthScheme: "Bear\x00er",
	})
}

// --- Issue #9: WWW-Authenticate not set on ContinueOnIgnoredError ---

func TestContinueOnIgnoredErrorNoWWWAuthenticate(t *testing.T) {
	mw := New(Config{
		Validator: validatorFor("correct"),
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return nil
		},
		ContinueOnIgnoredError: true,
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "public")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithHandlers(chain...),
	)
	err := ctx.Next()
	testutil.AssertNoError(t, err)

	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "www-authenticate" {
			t.Fatal("expected no www-authenticate header when ContinueOnIgnoredError and ErrorHandler returns nil")
		}
	}
}

func TestErrorHandlerReturnsNonNilSetsWWWAuthenticate(t *testing.T) {
	mw := New(Config{
		Validator: validatorFor("correct"),
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return celeris.NewHTTPError(401, "Nope")
		},
		ContinueOnIgnoredError: true,
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithHandlers(chain...),
	)
	err := ctx.Next()
	testutil.AssertHTTPError(t, err, 401)

	found := false
	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "www-authenticate" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected www-authenticate header when ErrorHandler returns non-nil error")
	}
}

// --- Issue #13: Realm with quote/control-char validation ---

func TestRealmWithQuotePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for Realm with quote")
		}
	}()
	New(Config{
		Validator: validatorFor("x"),
		Realm:     `My "Realm"`,
	})
}

func TestRealmWithBackslashPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for Realm with backslash")
		}
	}()
	New(Config{
		Validator: validatorFor("x"),
		Realm:     `My\Realm`,
	})
}

func TestRealmWithControlCharPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for Realm with control char")
		}
	}()
	New(Config{
		Validator: validatorFor("x"),
		Realm:     "My\x00Realm",
	})
}

// --- ChallengeParams scope NQCHAR validation tests ---

func TestChallengeParamsScopeValidNQCHAR(t *testing.T) {
	// Should not panic: valid NQCHAR tokens.
	New(Config{
		Validator:       validatorFor("x"),
		ChallengeParams: map[string]string{"scope": "read write admin"},
	})
}

func TestChallengeParamsScopeWithQuotePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for scope with double-quote")
		}
	}()
	New(Config{
		Validator:       validatorFor("x"),
		ChallengeParams: map[string]string{"scope": `read "write"`},
	})
}

func TestChallengeParamsScopeWithBackslashPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for scope with backslash")
		}
	}()
	New(Config{
		Validator:       validatorFor("x"),
		ChallengeParams: map[string]string{"scope": `read\write`},
	})
}

func TestChallengeParamsScopeWithControlCharPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for scope with control char")
		}
	}()
	New(Config{
		Validator:       validatorFor("x"),
		ChallengeParams: map[string]string{"scope": "read\x00write"},
	})
}

func TestChallengeParamsScopeWithDoubleSpacePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for scope with double space (empty token)")
		}
	}()
	New(Config{
		Validator:       validatorFor("x"),
		ChallengeParams: map[string]string{"scope": "read  write"},
	})
}

func FuzzKeyAuthHeader(f *testing.F) {
	f.Add("valid-key")
	f.Add("")
	f.Add("a")
	f.Add("very-long-key-" + string(make([]byte, 1024)))
	f.Fuzz(func(t *testing.T, key string) {
		mw := New(Config{
			Validator: func(_ *celeris.Context, k string) (bool, error) {
				return k == "valid-key", nil
			},
		})
		handler := func(c *celeris.Context) error {
			return c.String(200, "ok")
		}
		chain := []celeris.HandlerFunc{mw, handler}
		opts := []celeristest.Option{}
		if key != "" {
			opts = append(opts, celeristest.WithHeader("x-api-key", key))
		}
		_, _ = testutil.RunChain(t, chain, "GET", "/fuzz", opts...)
	})
}
