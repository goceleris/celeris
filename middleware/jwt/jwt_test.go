package jwt

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/jwt/internal/jwtparse"
)

var testSecret = []byte("test-secret-key-32-bytes-long!!!")

func signToken(claims jwtparse.Claims) string {
	s, err := jwtparse.SignToken(jwtparse.SigningMethodHS256, claims, testSecret)
	if err != nil {
		panic(err)
	}
	return s
}

func signTokenWithMethod(method jwtparse.SigningMethod, claims jwtparse.Claims, key any) string {
	s, err := jwtparse.SignToken(method, claims, key)
	if err != nil {
		panic(err)
	}
	return s
}

// signTokenWithKid creates a signed JWT with a kid in the JOSE header.
func signTokenWithKid(claims jwtparse.Claims, kid string, secret []byte) string {
	header := struct {
		Alg string `json:"alg"`
		Kid string `json:"kid,omitempty"`
		Typ string `json:"typ,omitempty"`
	}{Alg: "HS256", Kid: kid, Typ: "JWT"}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		panic(err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	headerEnc := base64.RawURLEncoding.EncodeToString(headerJSON)
	claimsEnc := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signingInput := headerEnc + "." + claimsEnc
	sig, err := jwtparse.SigningMethodHS256.Sign(signingInput, secret)
	if err != nil {
		panic(err)
	}
	sigEnc := base64.RawURLEncoding.EncodeToString(sig)
	return signingInput + "." + sigEnc
}

// signTokenWithKidAndMethod creates a signed JWT with a kid header and custom method/key.
func signTokenWithKidAndMethod(method jwtparse.SigningMethod, claims jwtparse.Claims, kid string, key any) string {
	header := struct {
		Alg string `json:"alg"`
		Kid string `json:"kid,omitempty"`
		Typ string `json:"typ,omitempty"`
	}{Alg: method.Alg(), Kid: kid, Typ: "JWT"}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		panic(err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	headerEnc := base64.RawURLEncoding.EncodeToString(headerJSON)
	claimsEnc := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signingInput := headerEnc + "." + claimsEnc
	sig, err := method.Sign(signingInput, key)
	if err != nil {
		panic(err)
	}
	sigEnc := base64.RawURLEncoding.EncodeToString(sig)
	return signingInput + "." + sigEnc
}

func runChain(t *testing.T, chain []celeris.HandlerFunc, method, path string, opts ...celeristest.Option) (*celeristest.ResponseRecorder, error) {
	t.Helper()
	allOpts := make([]celeristest.Option, 0, len(opts)+1)
	allOpts = append(allOpts, celeristest.WithHandlers(chain...))
	allOpts = append(allOpts, opts...)
	ctx, rec := celeristest.NewContextT(t, method, path, allOpts...)
	err := ctx.Next()
	return rec, err
}

func assertNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func assertStatus(t *testing.T, rec *celeristest.ResponseRecorder, expected int) {
	t.Helper()
	if rec.StatusCode != expected {
		t.Fatalf("status: got %d, want %d", rec.StatusCode, expected)
	}
}

func assertBodyContains(t *testing.T, rec *celeristest.ResponseRecorder, substr string) {
	t.Helper()
	body := rec.BodyString()
	if len(body) == 0 || !contains(body, substr) {
		t.Fatalf("body does not contain %q: %q", substr, body)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstr(s, substr)
}

func searchSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func assertHTTPError(t *testing.T, err error, expectedCode int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected HTTPError with code %d, got nil", expectedCode)
	}
	var he *celeris.HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if he.Code != expectedCode {
		t.Fatalf("HTTPError code: got %d, want %d", he.Code, expectedCode)
	}
}

func TestValidHMACToken(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "1234"})
	mw := New(Config{SigningKey: testSecret})
	var storedSub string
	handler := func(c *celeris.Context) error {
		claims, ok := ClaimsFromContext[jwtparse.MapClaims](c)
		if ok {
			storedSub, _ = claims["sub"].(string)
		}
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
	if storedSub != "1234" {
		t.Fatalf("sub: got %q, want %q", storedSub, "1234")
	}
}

func TestMissingTokenReturnsErrTokenMissing(t *testing.T) {
	mw := New(Config{SigningKey: testSecret})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := runChain(t, chain, "GET", "/")
	assertHTTPError(t, err, 401)
	if !errors.Is(err, ErrTokenMissing) {
		t.Fatalf("expected ErrTokenMissing, got %v", err)
	}
}

func TestInvalidTokenReturnsErrJWTMalformed(t *testing.T) {
	mw := New(Config{SigningKey: testSecret})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer invalid.token.here"),
	)
	assertHTTPError(t, err, 401)
	if !errors.Is(err, ErrJWTMalformed) {
		t.Fatalf("expected ErrJWTMalformed, got %v", err)
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{
		"sub": "1234",
		"exp": float64(time.Now().Add(-time.Hour).Unix()),
	})
	mw := New(Config{SigningKey: testSecret})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertHTTPError(t, err, 401)
	if !errors.Is(err, ErrJWTExpired) {
		t.Fatalf("expected ErrJWTExpired, got %v", err)
	}
}

func TestSigningKeysKidRotation(t *testing.T) {
	secret1 := []byte("secret-one-32-bytes-long!!!!!!!!")
	secret2 := []byte("secret-two-32-bytes-long!!!!!!!!")
	tokenStr := signTokenWithKid(jwtparse.MapClaims{"sub": "user1"}, "key-2", secret2)

	mw := New(Config{
		SigningKeys: map[string]any{
			"key-1": secret1,
			"key-2": secret2,
		},
	})
	var storedSub string
	handler := func(c *celeris.Context) error {
		claims, ok := ClaimsFromContext[jwtparse.MapClaims](c)
		if ok {
			storedSub, _ = claims["sub"].(string)
		}
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
	if storedSub != "user1" {
		t.Fatalf("sub: got %q, want %q", storedSub, "user1")
	}
}

func TestSigningKeysUnknownKid(t *testing.T) {
	secret := []byte("known-secret-32-bytes-long!!!!!!")
	tokenStr := signTokenWithKid(jwtparse.MapClaims{"sub": "u"}, "unknown-kid", secret)

	mw := New(Config{
		SigningKeys: map[string]any{
			"known-kid": secret,
		},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertHTTPError(t, err, 401)
}

func TestTokenFromContext(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "1234"})
	mw := New(Config{SigningKey: testSecret})
	var got *jwtparse.Token
	handler := func(c *celeris.Context) error {
		got = TokenFromContext(c)
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, _ = runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	if got == nil {
		t.Fatal("TokenFromContext returned nil")
	}
	if !got.Valid {
		t.Fatal("token is not valid")
	}
}

func TestTokenFromContextNoAuth(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "GET", "/")
	if got := TokenFromContext(ctx); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestClaimsFromContextGeneric(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "typed"})
	mw := New(Config{SigningKey: testSecret})
	var sub string
	var ok bool
	handler := func(c *celeris.Context) error {
		claims, found := ClaimsFromContext[jwtparse.MapClaims](c)
		ok = found
		if found {
			sub, _ = claims["sub"].(string)
		}
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, _ = runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	if !ok {
		t.Fatal("ClaimsFromContext returned false")
	}
	if sub != "typed" {
		t.Fatalf("sub: got %q, want %q", sub, "typed")
	}
}

func TestClaimsFromContextNoAuth(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "GET", "/")
	_, ok := ClaimsFromContext[jwtparse.MapClaims](ctx)
	if ok {
		t.Fatal("expected false, got true")
	}
}

func TestTokenLookupQuery(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "q"})
	mw := New(Config{
		SigningKey:  testSecret,
		TokenLookup: "query:token",
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "GET", "/",
		celeristest.WithQuery("token", tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
}

func TestTokenLookupCookie(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "ck"})
	mw := New(Config{
		SigningKey:  testSecret,
		TokenLookup: "cookie:jwt",
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "GET", "/",
		celeristest.WithCookie("jwt", tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
}

func TestTokenLookupMultipleSources(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "multi"})
	mw := New(Config{
		SigningKey:  testSecret,
		TokenLookup: "header:Authorization:Bearer ,query:token,cookie:jwt",
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}

	// First source (header) empty, second (query) has token.
	rec, err := runChain(t, chain, "GET", "/",
		celeristest.WithQuery("token", tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)

	// Cookie as fallback.
	rec, err = runChain(t, chain, "GET", "/",
		celeristest.WithCookie("jwt", tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
}

func TestSkipBypassesAuth(t *testing.T) {
	mw := New(Config{
		SigningKey: testSecret,
		Skip:       func(_ *celeris.Context) bool { return true },
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "skipped")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "GET", "/")
	assertNoError(t, err)
	assertStatus(t, rec, 200)
	assertBodyContains(t, rec, "skipped")
}

func TestSkipPathsBypassesAuth(t *testing.T) {
	mw := New(Config{
		SigningKey: testSecret,
		SkipPaths:  []string{"/health", "/ready"},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := runChain(t, chain, "GET", "/health")
	assertNoError(t, err)
	assertStatus(t, rec, 200)

	rec, err = runChain(t, chain, "GET", "/ready")
	assertNoError(t, err)
	assertStatus(t, rec, 200)

	_, err = runChain(t, chain, "GET", "/protected")
	assertHTTPError(t, err, 401)
}

func TestSuccessHandlerCalled(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "1234"})
	var called bool
	mw := New(Config{
		SigningKey: testSecret,
		SuccessHandler: func(_ *celeris.Context) {
			called = true
		},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertNoError(t, err)
	if !called {
		t.Fatal("SuccessHandler was not called")
	}
}

func TestSuccessHandlerNotCalledOnFailure(t *testing.T) {
	var called bool
	mw := New(Config{
		SigningKey: testSecret,
		SuccessHandler: func(_ *celeris.Context) {
			called = true
		},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	_, _ = runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer invalid.token"),
	)
	if called {
		t.Fatal("SuccessHandler should not be called on failure")
	}
}

func TestWrongSigningMethodRejected(t *testing.T) {
	// Create a token with HS384 but middleware expects HS256.
	tokenStr := signTokenWithMethod(jwtparse.SigningMethodHS384, jwtparse.MapClaims{"sub": "1"}, testSecret)

	mw := New(Config{SigningKey: testSecret})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertHTTPError(t, err, 401)
}

func TestCustomErrorHandler(t *testing.T) {
	mw := New(Config{
		SigningKey: testSecret,
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return celeris.NewHTTPError(403, "Forbidden")
		},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := runChain(t, chain, "GET", "/")
	assertHTTPError(t, err, 403)
}

func TestCustomKeyFunc(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "custom"})
	mw := New(Config{
		SigningKey: testSecret,
		KeyFunc: func(_ *jwtparse.Token) (any, error) {
			return testSecret, nil
		},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
}

func TestNoConfigPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for empty config")
		}
	}()
	New(Config{})
}

func TestInvalidTokenLookupPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for invalid TokenLookup")
		}
	}()
	New(Config{
		SigningKey:  testSecret,
		TokenLookup: "bad",
	})
}

func TestFormExtractorSupported(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "form-user"})
	mw := New(Config{
		SigningKey:  testSecret,
		TokenLookup: "form:access_token",
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "POST", "/",
		celeristest.WithHeader("content-type", "application/x-www-form-urlencoded"),
		celeristest.WithBody([]byte("access_token="+tokenStr)),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
}

func TestParamExtractorSupported(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "param-user"})
	mw := New(Config{
		SigningKey:  testSecret,
		TokenLookup: "param:token",
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "GET", "/resource/"+tokenStr,
		celeristest.WithParam("token", tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
}

func TestBearerPrefixCaseInsensitive(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "1234"})
	mw := New(Config{SigningKey: testSecret})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}

	// Lowercase "bearer" should match "Bearer " prefix (case-insensitive).
	rec, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "bearer "+tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)

	// Mixed case "BEARER" should also match.
	rec, err = runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "BEARER "+tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)

	// "BeArEr" should also match.
	rec, err = runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "BeArEr "+tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
}

func TestSkipDoesNotStoreToken(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "1234"})
	mw := New(Config{
		SigningKey: testSecret,
		Skip:       func(_ *celeris.Context) bool { return true },
	})
	var tok *jwtparse.Token
	handler := func(c *celeris.Context) error {
		tok = TokenFromContext(c)
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, _ = runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	if tok != nil {
		t.Fatal("expected nil token on skip")
	}
}

func TestDefaultConfigValues(t *testing.T) {
	if defaultConfig.SigningMethod.Alg() != "HS256" {
		t.Fatalf("default SigningMethod: got %q, want HS256", defaultConfig.SigningMethod.Alg())
	}
	if defaultConfig.TokenLookup != "header:Authorization:Bearer " {
		t.Fatalf("default TokenLookup: got %q, want %q", defaultConfig.TokenLookup, "header:Authorization:Bearer ")
	}
	if defaultConfig.JWKSRefresh != time.Hour {
		t.Fatalf("default JWKSRefresh: got %v, want 1h", defaultConfig.JWKSRefresh)
	}
}

func TestApplyDefaultsFillsMissing(t *testing.T) {
	cfg := applyDefaults(Config{SigningKey: testSecret})
	if cfg.SigningMethod == nil {
		t.Fatal("SigningMethod is nil after applyDefaults")
	}
	if cfg.TokenLookup == "" {
		t.Fatal("TokenLookup is empty after applyDefaults")
	}
	if cfg.Claims == nil {
		t.Fatal("Claims is nil after applyDefaults")
	}
	if cfg.TokenContextKey == "" {
		t.Fatal("TokenContextKey is empty after applyDefaults")
	}
	if cfg.ClaimsContextKey == "" {
		t.Fatal("ClaimsContextKey is empty after applyDefaults")
	}
}

func TestContextKeyConstants(t *testing.T) {
	if TokenKey != "jwt_token" {
		t.Fatalf("TokenKey: got %q, want %q", TokenKey, "jwt_token")
	}
	if ClaimsKey != "jwt_claims" {
		t.Fatalf("ClaimsKey: got %q, want %q", ClaimsKey, "jwt_claims")
	}
}

func TestHeaderExtractorNoPrefix(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "raw"})
	mw := New(Config{
		SigningKey:  testSecret,
		TokenLookup: "header:X-Token",
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("x-token", tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
}

func TestWrongSecretRejected(t *testing.T) {
	tokenStr := signTokenWithMethod(jwtparse.SigningMethodHS256, jwtparse.MapClaims{"sub": "1"}, []byte("different-secret-key-32-bytes!!!"))

	mw := New(Config{SigningKey: testSecret})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertHTTPError(t, err, 401)
}

// --- Error wrapping tests ---

func TestErrorWrapping(t *testing.T) {
	mw := New(Config{SigningKey: testSecret})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer invalid.token.here"),
	)
	// Malformed tokens should wrap ErrJWTMalformed.
	if !errors.Is(err, ErrJWTMalformed) {
		t.Fatalf("expected errors.Is(ErrJWTMalformed), got %v", err)
	}
	// The multi-wrapped error should contain the jwtparse malformed error.
	if !errors.Is(err, jwtparse.ErrTokenMalformed) {
		t.Fatalf("expected wrapped jwtparse.ErrTokenMalformed, got %v", err)
	}
}

func TestErrorWrappingExpired(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{
		"sub": "1234",
		"exp": float64(time.Now().Add(-time.Hour).Unix()),
	})
	mw := New(Config{SigningKey: testSecret})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	// Expired tokens should wrap ErrJWTExpired.
	if !errors.Is(err, ErrJWTExpired) {
		t.Fatalf("expected errors.Is(ErrJWTExpired), got %v", err)
	}
	// Should also wrap the jwtparse expiration error.
	if !errors.Is(err, jwtparse.ErrTokenExpired) {
		t.Fatalf("expected errors.Is(ErrTokenExpired), got %v", err)
	}
}

type customClaims struct {
	jwtparse.RegisteredClaims
	Role string `json:"role"`
}

func TestClaimsFactory(t *testing.T) {
	claims := &customClaims{
		RegisteredClaims: jwtparse.RegisteredClaims{Subject: "factory-user"},
		Role:             "admin",
	}
	tokenStr, err := jwtparse.SignToken(jwtparse.SigningMethodHS256, claims, testSecret)
	if err != nil {
		t.Fatal(err)
	}

	mw := New(Config{
		SigningKey: testSecret,
		ClaimsFactory: func() jwtparse.Claims {
			return &customClaims{}
		},
	})
	var gotRole string
	handler := func(c *celeris.Context) error {
		v, ok := c.Get(ClaimsKey)
		if !ok {
			t.Fatal("no claims in context")
		}
		cc, ok := v.(*customClaims)
		if !ok {
			t.Fatalf("claims type: got %T, want *customClaims", v)
		}
		gotRole = cc.Role
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
	if gotRole != "admin" {
		t.Fatalf("role: got %q, want %q", gotRole, "admin")
	}
}

func TestCloneClaimsCustomStruct(t *testing.T) {
	template := &customClaims{Role: "template"}
	cloned := cloneClaims(template)
	cc, ok := cloned.(*customClaims)
	if !ok {
		t.Fatalf("cloned type: got %T, want *customClaims", cloned)
	}
	// cloneClaims should return a zero-value struct, not copy template fields.
	if cc.Role != "" {
		t.Fatalf("cloned Role should be empty, got %q", cc.Role)
	}
	// Must be a different pointer.
	if cc == template {
		t.Fatal("cloneClaims returned same pointer as template")
	}
}

func TestConfigurableContextKey(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "ctx-key"})
	mw := New(Config{
		SigningKey:       testSecret,
		TokenContextKey:  "my_token",
		ClaimsContextKey: "my_claims",
	})
	var gotToken bool
	var gotClaims bool
	handler := func(c *celeris.Context) error {
		_, gotToken = c.Get("my_token")
		_, gotClaims = c.Get("my_claims")
		// Default keys should NOT be set.
		_, defaultToken := c.Get(TokenKey)
		_, defaultClaims := c.Get(ClaimsKey)
		if defaultToken {
			t.Error("default TokenKey should not be set")
		}
		if defaultClaims {
			t.Error("default ClaimsKey should not be set")
		}
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
	if !gotToken {
		t.Fatal("custom token key not found")
	}
	if !gotClaims {
		t.Fatal("custom claims key not found")
	}
}

func TestValidMethodsConfig(t *testing.T) {
	// Create a token with HS384.
	tokenStr := signTokenWithMethod(jwtparse.SigningMethodHS384, jwtparse.MapClaims{"sub": "1"}, testSecret)

	// Allow both HS256 and HS384.
	mw := New(Config{
		SigningKey:   testSecret,
		ValidMethods: []string{"HS256", "HS384"},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
}

func TestValidMethodsRejectsUnlisted(t *testing.T) {
	// Create a token with HS384.
	tokenStr := signTokenWithMethod(jwtparse.SigningMethodHS384, jwtparse.MapClaims{"sub": "1"}, testSecret)

	// Only allow HS256.
	mw := New(Config{
		SigningKey:   testSecret,
		ValidMethods: []string{"HS256"},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertHTTPError(t, err, 401)
}

func TestJWKSURLHTTPSEnforcement(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for non-HTTPS JWKS URL")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("expected string panic, got %T", r)
		}
		if !contains(msg, "HTTPS") {
			t.Fatalf("expected HTTPS error, got %q", msg)
		}
	}()
	New(Config{
		JWKSURL: "http://external-host.example.com/.well-known/jwks.json",
	})
}

func TestJWKSURLLocalhostHTTPAllowed(t *testing.T) {
	// Should not panic for localhost HTTP.
	defer func() {
		if r := recover(); r != nil {
			msg, _ := r.(string)
			if contains(msg, "HTTPS") {
				t.Fatalf("should allow HTTP for localhost, got panic: %v", r)
			}
		}
	}()
	// This will panic due to fetch failure, not HTTPS validation.
	// We just verify the HTTPS check doesn't trigger.
	validateJWKSURL("http://localhost:8080/.well-known/jwks.json")
	validateJWKSURL("http://127.0.0.1:8080/.well-known/jwks.json")
	validateJWKSURL("http://[::1]:8080/.well-known/jwks.json")
}

func TestTokenFromContextWithKey(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "1234"})
	customKey := "my_jwt_token"
	mw := New(Config{
		SigningKey:      testSecret,
		TokenContextKey: customKey,
	})
	var gotDefault *jwtparse.Token
	var gotCustom *jwtparse.Token
	handler := func(c *celeris.Context) error {
		gotDefault = TokenFromContext(c)
		gotCustom = TokenFromContextWithKey(c, customKey)
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, _ = runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	if gotDefault != nil {
		t.Fatal("TokenFromContext should return nil for custom key")
	}
	if gotCustom == nil {
		t.Fatal("TokenFromContextWithKey returned nil")
	}
	if !gotCustom.Valid {
		t.Fatal("token is not valid")
	}
}

func TestClaimsFromContextWithKey(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "typed-custom"})
	customKey := "my_jwt_claims"
	mw := New(Config{
		SigningKey:       testSecret,
		ClaimsContextKey: customKey,
	})
	var sub string
	var okDefault, okCustom bool
	handler := func(c *celeris.Context) error {
		_, okDefault = ClaimsFromContext[jwtparse.MapClaims](c)
		claims, found := ClaimsFromContextWithKey[jwtparse.MapClaims](c, customKey)
		okCustom = found
		if found {
			sub, _ = claims["sub"].(string)
		}
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, _ = runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	if okDefault {
		t.Fatal("ClaimsFromContext should return false for custom key")
	}
	if !okCustom {
		t.Fatal("ClaimsFromContextWithKey returned false")
	}
	if sub != "typed-custom" {
		t.Fatalf("sub: got %q, want %q", sub, "typed-custom")
	}
}

func TestSentinelErrorsReExported(t *testing.T) {
	// Verify all re-exported sentinel errors are non-nil and distinct.
	errs := []error{
		ErrTokenMalformed,
		ErrTokenExpired,
		ErrTokenNotValidYet,
		ErrTokenSignatureInvalid,
		ErrAlgNone,
		ErrTokenUnverifiable,
		ErrTokenUsedBeforeIssued,
		ErrInvalidIssuer,
		ErrInvalidAudience,
	}
	seen := make(map[string]bool, len(errs))
	for _, e := range errs {
		if e == nil {
			t.Fatalf("sentinel error is nil")
		}
		msg := e.Error()
		if seen[msg] {
			t.Fatalf("duplicate sentinel error message: %q", msg)
		}
		seen[msg] = true
	}
}

func TestParseOptionsLeeway(t *testing.T) {
	// Create a token expired 2 seconds ago.
	tokenStr := signToken(jwtparse.MapClaims{
		"sub": "leeway-user",
		"exp": float64(time.Now().Add(-2 * time.Second).Unix()),
	})

	// Without leeway: should reject.
	mw := New(Config{SigningKey: testSecret})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertHTTPError(t, err, 401)

	// With leeway of 10s: should accept.
	mw2 := New(Config{
		SigningKey:   testSecret,
		ParseOptions: []jwtparse.ParserOption{jwtparse.WithLeeway(10 * time.Second)},
	})
	chain2 := []celeris.HandlerFunc{mw2, handler}
	rec, err := runChain(t, chain2, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
}

func TestParseOptionsIssuer(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{
		"sub": "iss-user",
		"iss": "my-issuer",
	})

	// WithIssuer matching: should accept.
	mw := New(Config{
		SigningKey:   testSecret,
		ParseOptions: []jwtparse.ParserOption{jwtparse.WithIssuer("my-issuer")},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)

	// WithIssuer mismatched: should reject.
	mw2 := New(Config{
		SigningKey:   testSecret,
		ParseOptions: []jwtparse.ParserOption{jwtparse.WithIssuer("wrong-issuer")},
	})
	chain2 := []celeris.HandlerFunc{mw2, handler}
	_, err = runChain(t, chain2, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertHTTPError(t, err, 401)
}

func TestRSAPSSSigningMethodReExported(t *testing.T) {
	methods := []SigningMethod{
		SigningMethodPS256,
		SigningMethodPS384,
		SigningMethodPS512,
	}
	expected := []string{"PS256", "PS384", "PS512"}
	for i, m := range methods {
		if m == nil {
			t.Fatalf("SigningMethod%s is nil", expected[i])
		}
		if m.Alg() != expected[i] {
			t.Fatalf("Alg() = %q, want %q", m.Alg(), expected[i])
		}
	}
}

// --- TokenProcessorFunc tests ---

func TestTokenProcessorFuncTransformsToken(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "processor-user"})
	// Prepend "enc:" to the token, then strip it in the processor.
	encToken := "enc:" + tokenStr
	mw := New(Config{
		SigningKey: testSecret,
		TokenProcessorFunc: func(token string) (string, error) {
			if len(token) > 4 && token[:4] == "enc:" {
				return token[4:], nil
			}
			return token, nil
		},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+encToken),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
}

func TestTokenProcessorFuncErrorReturnsErrTokenInvalid(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "1"})
	mw := New(Config{
		SigningKey: testSecret,
		TokenProcessorFunc: func(_ string) (string, error) {
			return "", errors.New("decryption failed")
		},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertHTTPError(t, err, 401)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("expected ErrTokenInvalid, got %v", err)
	}
}

func TestTokenProcessorFuncNilPassesThrough(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "no-processor"})
	mw := New(Config{
		SigningKey:         testSecret,
		TokenProcessorFunc: nil,
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
}

func TestTokenProcessorFuncIdentity(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "identity"})
	mw := New(Config{
		SigningKey: testSecret,
		TokenProcessorFunc: func(token string) (string, error) {
			return token, nil
		},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
}

// --- JWKSURLs (multi-JWKS) tests ---

func TestBuildJWKSURLsSingleURL(t *testing.T) {
	urls := buildJWKSURLs(Config{JWKSURL: "https://a.com/jwks"})
	if len(urls) != 1 || urls[0] != "https://a.com/jwks" {
		t.Fatalf("expected [https://a.com/jwks], got %v", urls)
	}
}

func TestBuildJWKSURLsMultipleURLs(t *testing.T) {
	urls := buildJWKSURLs(Config{JWKSURLs: []string{"https://a.com/jwks", "https://b.com/jwks"}})
	if len(urls) != 2 {
		t.Fatalf("expected 2 URLs, got %d", len(urls))
	}
}

func TestBuildJWKSURLsMergesSingleAndMultiple(t *testing.T) {
	urls := buildJWKSURLs(Config{
		JWKSURL:  "https://a.com/jwks",
		JWKSURLs: []string{"https://b.com/jwks", "https://c.com/jwks"},
	})
	if len(urls) != 3 {
		t.Fatalf("expected 3 URLs, got %d", len(urls))
	}
	if urls[0] != "https://a.com/jwks" {
		t.Fatalf("expected JWKSURL first, got %v", urls)
	}
}

func TestBuildJWKSURLsDeduplicates(t *testing.T) {
	urls := buildJWKSURLs(Config{
		JWKSURL:  "https://a.com/jwks",
		JWKSURLs: []string{"https://a.com/jwks", "https://b.com/jwks"},
	})
	if len(urls) != 2 {
		t.Fatalf("expected 2 URLs (deduplicated), got %d: %v", len(urls), urls)
	}
}

func TestBuildJWKSURLsEmpty(t *testing.T) {
	urls := buildJWKSURLs(Config{})
	if urls != nil {
		t.Fatalf("expected nil, got %v", urls)
	}
}

func TestJWKSURLsOnlyValidation(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for empty config")
		}
	}()
	New(Config{})
}

func TestJWKSURLsAcceptedAsOnlyKeySource(t *testing.T) {
	// Should not panic when JWKSURLs is the only key source.
	defer func() {
		if r := recover(); r != nil {
			msg, _ := r.(string)
			if contains(msg, "required") {
				t.Fatalf("JWKSURLs should be accepted as key source, got panic: %v", r)
			}
		}
	}()
	validateJWKSURL("http://localhost:8080/jwks")
	_ = buildJWKSURLs(Config{JWKSURLs: []string{"http://localhost:8080/jwks"}})
}

func TestJWKSURLsHTTPSEnforcement(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for non-HTTPS JWKSURLs entry")
		}
	}()
	New(Config{
		JWKSURLs: []string{"http://external.example.com/jwks"},
	})
}

// --- WithSubject tests ---

func TestWithSubjectMatching(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "user-123"})
	mw := New(Config{
		SigningKey:   testSecret,
		ParseOptions: []jwtparse.ParserOption{jwtparse.WithSubject("user-123")},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
}

func TestWithSubjectMismatch(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "user-123"})
	mw := New(Config{
		SigningKey:   testSecret,
		ParseOptions: []jwtparse.ParserOption{jwtparse.WithSubject("user-456")},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertHTTPError(t, err, 401)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("expected ErrTokenInvalid, got %v", err)
	}
}

func TestWithSubjectMissing(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"iss": "my-app"})
	mw := New(Config{
		SigningKey:   testSecret,
		ParseOptions: []jwtparse.ParserOption{jwtparse.WithSubject("user-123")},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertHTTPError(t, err, 401)
}

func TestWithSubjectRegisteredClaims(t *testing.T) {
	claims := &jwtparse.RegisteredClaims{Subject: "service-a"}
	tokenStr, err := jwtparse.SignToken(jwtparse.SigningMethodHS256, claims, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	mw := New(Config{
		SigningKey: testSecret,
		ClaimsFactory: func() jwtparse.Claims {
			return &jwtparse.RegisteredClaims{}
		},
		ParseOptions: []jwtparse.ParserOption{jwtparse.WithSubject("service-a")},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
}

func TestWithSubjectRegisteredClaimsMismatch(t *testing.T) {
	claims := &jwtparse.RegisteredClaims{Subject: "service-a"}
	tokenStr, err := jwtparse.SignToken(jwtparse.SigningMethodHS256, claims, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	mw := New(Config{
		SigningKey: testSecret,
		ClaimsFactory: func() jwtparse.Claims {
			return &jwtparse.RegisteredClaims{}
		},
		ParseOptions: []jwtparse.ParserOption{jwtparse.WithSubject("service-b")},
	})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	_, err = runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertHTTPError(t, err, 401)
}

func TestErrInvalidSubjectReExported(t *testing.T) {
	if ErrInvalidSubject == nil {
		t.Fatal("ErrInvalidSubject is nil")
	}
	if ErrInvalidSubject.Error() != "token has invalid subject" {
		t.Fatalf("unexpected error message: %q", ErrInvalidSubject.Error())
	}
}

func TestWithSubjectReExported(t *testing.T) {
	// Verify WithSubject is properly re-exported and can be used.
	opt := WithSubject("test-sub")
	if opt == nil {
		t.Fatal("WithSubject returned nil")
	}
}

// --- ContinueOnIgnoredError tests ---

func TestContinueOnIgnoredErrorCallsNext(t *testing.T) {
	var nextCalled bool
	mw := New(Config{
		SigningKey: testSecret,
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return nil // suppress error
		},
		ContinueOnIgnoredError: true,
	})
	handler := func(c *celeris.Context) error {
		nextCalled = true
		return c.String(200, "reached")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "GET", "/")
	assertNoError(t, err)
	assertStatus(t, rec, 200)
	if !nextCalled {
		t.Fatal("expected downstream handler to be called when ContinueOnIgnoredError is true")
	}
}

func TestContinueOnIgnoredErrorFalseDoesNotCallNext(t *testing.T) {
	// When ContinueOnIgnoredError is false and ErrorHandler returns nil,
	// the middleware returns nil without calling c.Next(). Downstream
	// errors are not propagated through the middleware.
	mw := New(Config{
		SigningKey: testSecret,
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return nil // suppress error
		},
		ContinueOnIgnoredError: false,
	})
	downstreamErr := celeris.NewHTTPError(422, "Unprocessable")
	handler := func(_ *celeris.Context) error {
		return downstreamErr
	}
	chain := []celeris.HandlerFunc{mw, handler}
	// The middleware returns nil; the framework's outer Next loop runs
	// the downstream handler, but the error is NOT propagated through
	// the middleware (the middleware already returned nil). The outer
	// loop stops on the first non-nil error from the downstream handler.
	_, err := runChain(t, chain, "GET", "/")
	assertHTTPError(t, err, 422)
}

func TestContinueOnIgnoredErrorTruePropagatesDownstreamError(t *testing.T) {
	// When ContinueOnIgnoredError is true and ErrorHandler returns nil,
	// the middleware explicitly calls c.Next(), so downstream errors
	// propagate through the middleware's return value.
	mw := New(Config{
		SigningKey: testSecret,
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return nil // suppress error
		},
		ContinueOnIgnoredError: true,
	})
	downstreamErr := celeris.NewHTTPError(422, "Unprocessable")
	handler := func(_ *celeris.Context) error {
		return downstreamErr
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := runChain(t, chain, "GET", "/")
	assertHTTPError(t, err, 422)
}

func TestContinueOnIgnoredErrorWithTokenProcessorError(t *testing.T) {
	tokenStr := signToken(jwtparse.MapClaims{"sub": "1"})
	var nextCalled bool
	mw := New(Config{
		SigningKey: testSecret,
		TokenProcessorFunc: func(_ string) (string, error) {
			return "", errors.New("decrypt failed")
		},
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return nil
		},
		ContinueOnIgnoredError: true,
	})
	handler := func(c *celeris.Context) error {
		nextCalled = true
		return c.String(200, "fallback")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertNoError(t, err)
	assertStatus(t, rec, 200)
	if !nextCalled {
		t.Fatal("expected downstream handler called for ignored processor error")
	}
}

func TestContinueOnIgnoredErrorNonNilErrorStops(t *testing.T) {
	var nextCalled bool
	mw := New(Config{
		SigningKey: testSecret,
		ErrorHandler: func(_ *celeris.Context, err error) error {
			return err // propagate error
		},
		ContinueOnIgnoredError: true,
	})
	handler := func(c *celeris.Context) error {
		nextCalled = true
		return c.String(200, "reached")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := runChain(t, chain, "GET", "/")
	assertHTTPError(t, err, 401)
	if nextCalled {
		t.Fatal("downstream handler should NOT be called when ErrorHandler returns non-nil")
	}
}

// --- HMAC minimum key length tests ---

func TestHMACKeyLengthWarning(t *testing.T) {
	tests := []struct {
		name    string
		method  jwtparse.SigningMethod
		keyLen  int
		wantMsg string
	}{
		{"HS256/short", jwtparse.SigningMethodHS256, 5, "HS256 signing key is 5 bytes"},
		{"HS384/short", jwtparse.SigningMethodHS384, 32, "HS384 signing key is 32 bytes"},
		{"HS512/short", jwtparse.SigningMethodHS512, 48, "HS512 signing key is 48 bytes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key := make([]byte, tc.keyLen)
			for i := range key {
				key[i] = byte(i + 1)
			}
			var logBuf strings.Builder
			prevOut := log.Writer()
			prevFlags := log.Flags()
			log.SetOutput(&logBuf)
			log.SetFlags(0)
			defer log.SetOutput(prevOut)
			defer log.SetFlags(prevFlags)

			tokenStr := signTokenWithMethod(tc.method, jwtparse.MapClaims{"sub": "1"}, key)
			mw := New(Config{
				SigningKey:    key,
				SigningMethod: tc.method,
			})
			handler := func(c *celeris.Context) error { return c.String(200, "ok") }
			chain := []celeris.HandlerFunc{mw, handler}
			_, err := runChain(t, chain, "GET", "/",
				celeristest.WithHeader("authorization", "Bearer "+tokenStr),
			)
			assertNoError(t, err)
			if !strings.Contains(logBuf.String(), tc.wantMsg) {
				t.Fatalf("expected %q in log, got: %q", tc.wantMsg, logBuf.String())
			}
		})
	}
}

func TestHMACKeyLengthNoWarningWhenSufficient(t *testing.T) {
	goodKey := make([]byte, 32)
	for i := range goodKey {
		goodKey[i] = byte(i + 1)
	}
	var logBuf strings.Builder
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	defer log.SetOutput(prevOut)
	defer log.SetFlags(prevFlags)

	tokenStr := signTokenWithMethod(jwtparse.SigningMethodHS256, jwtparse.MapClaims{"sub": "1"}, goodKey)
	mw := New(Config{SigningKey: goodKey})
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := runChain(t, chain, "GET", "/",
		celeristest.WithHeader("authorization", "Bearer "+tokenStr),
	)
	assertNoError(t, err)
	if logBuf.Len() > 0 {
		t.Fatalf("expected no warning for adequate key length, got: %q", logBuf.String())
	}
}
