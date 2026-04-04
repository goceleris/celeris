package basicauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

func validatorFor(user, pass string) func(string, string) bool {
	return func(u, p string) bool {
		return subtle.ConstantTimeCompare([]byte(u), []byte(user)) == 1 &&
			subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1
	}
}

func TestValidCredentialsPass(t *testing.T) {
	mw := New(Config{Validator: validatorFor("admin", "secret")})
	var storedUser string
	handler := func(c *celeris.Context) error {
		storedUser = UsernameFromContext(c)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithBasicAuth("admin", "secret"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
	if storedUser != "admin" {
		t.Fatalf("user in context: got %q, want %q", storedUser, "admin")
	}
}

func TestAuthFailuresReturn401(t *testing.T) {
	mw := New(Config{Validator: validatorFor("admin", "secret")})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	tests := []struct {
		name string
		opts []celeristest.Option
	}{
		{"invalid credentials", []celeristest.Option{celeristest.WithBasicAuth("admin", "wrong")}},
		{"missing auth header", nil},
		{"wrong scheme", []celeristest.Option{celeristest.WithHeader("authorization", "NotBasic abc123")}},
		{"bad base64", []celeristest.Option{celeristest.WithHeader("authorization", "Basic !!!notbase64!!!")}},
		{"wrong user right password", []celeristest.Option{celeristest.WithBasicAuth("wrong", "secret")}},
		{"bearer token", []celeristest.Option{celeristest.WithHeader("authorization", "Bearer token123")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := testutil.RunChain(t, chain, "GET", "/", tt.opts...)
			testutil.AssertHTTPError(t, err, 401)
		})
	}
}

func TestWWWAuthenticateHeaderSet(t *testing.T) {
	mw := New(Config{Validator: validatorFor("admin", "secret")})
	ctx, _ := celeristest.NewContextT(t, "GET", "/")
	err := mw(ctx)
	testutil.AssertHTTPError(t, err, 401)
	found := false
	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "www-authenticate" && h[1] == `Basic realm="Restricted"` {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected www-authenticate header with realm Restricted, got %v", ctx.ResponseHeaders())
	}
}

func TestCustomRealm(t *testing.T) {
	mw := New(Config{
		Validator: validatorFor("admin", "secret"),
		Realm:     "MyApp",
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/")
	err := mw(ctx)
	testutil.AssertHTTPError(t, err, 401)
	found := false
	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "www-authenticate" && h[1] == `Basic realm="MyApp"` {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected www-authenticate header with realm MyApp, got %v", ctx.ResponseHeaders())
	}
}

func TestCustomErrorHandler(t *testing.T) {
	mw := New(Config{
		Validator: validatorFor("admin", "secret"),
		ErrorHandler: func(_ *celeris.Context, _ error) error {
			return celeris.NewHTTPError(403, "Forbidden")
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithBasicAuth("admin", "wrong"),
	)
	testutil.AssertHTTPError(t, err, 403)
}

func TestSkipBypassesAuth(t *testing.T) {
	mw := New(Config{
		Validator: validatorFor("admin", "secret"),
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

func TestNilValidatorPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil Validator")
		}
	}()
	New(Config{Validator: nil})
}

func TestEmptyUsernamePassword(t *testing.T) {
	mw := New(Config{
		Validator: func(u, p string) bool { return u == "" && p == "" },
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithBasicAuth("", ""),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestDefaultConfigRealm(t *testing.T) {
	if defaultConfig.Realm != "Restricted" {
		t.Fatalf("default Realm: got %q, want %q", defaultConfig.Realm, "Restricted")
	}
}

func TestApplyDefaultsFixesEmptyRealm(t *testing.T) {
	cfg := applyDefaults(Config{Realm: ""})
	if cfg.Realm != "Restricted" {
		t.Fatalf("expected Restricted, got %q", cfg.Realm)
	}
}

func TestUsersMap(t *testing.T) {
	tests := []struct {
		name     string
		user     string
		pass     string
		wantCode int
		wantUser string
	}{
		{"valid admin", "admin", "secret", 200, "admin"},
		{"valid user2", "user2", "pass2", 200, "user2"},
		{"wrong password", "admin", "wrong", 401, ""},
		{"unknown user", "unknown", "secret", 401, ""},
	}
	users := map[string]string{"admin": "secret", "user2": "pass2"}
	mw := New(Config{Users: users})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var storedUser string
			handler := func(c *celeris.Context) error {
				storedUser = UsernameFromContext(c)
				return c.String(200, "ok")
			}
			chain := []celeris.HandlerFunc{mw, handler}
			rec, err := testutil.RunChain(t, chain, "GET", "/",
				celeristest.WithBasicAuth(tt.user, tt.pass))
			if tt.wantCode == 200 {
				testutil.AssertNoError(t, err)
				testutil.AssertStatus(t, rec, 200)
				if storedUser != tt.wantUser {
					t.Fatalf("UsernameFromContext: got %q, want %q", storedUser, tt.wantUser)
				}
			} else {
				testutil.AssertHTTPError(t, err, tt.wantCode)
			}
		})
	}
}

func TestNilValidatorAndNilUsersPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when both Validator and Users are nil")
		}
	}()
	New(Config{})
}

func TestValidatorTakesPrecedenceOverUsers(t *testing.T) {
	called := false
	mw := New(Config{
		Validator: func(u, p string) bool {
			called = true
			return u == "custom" && p == "val"
		},
		Users: map[string]string{"admin": "secret"},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithBasicAuth("custom", "val"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if !called {
		t.Fatal("expected custom Validator to be called")
	}
}

func TestUsernameFromContext(t *testing.T) {
	tests := []struct {
		name     string
		withAuth bool
		wantUser string
	}{
		{"no auth", false, ""},
		{"with auth", true, "admin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.withAuth {
				ctx, _ := celeristest.NewContextT(t, "GET", "/")
				if got := UsernameFromContext(ctx); got != tt.wantUser {
					t.Fatalf("expected %q, got %q", tt.wantUser, got)
				}
				return
			}
			mw := New(Config{Users: map[string]string{"admin": "pass"}})
			var username string
			handler := func(c *celeris.Context) error {
				username = UsernameFromContext(c)
				return c.String(200, "ok")
			}
			chain := []celeris.HandlerFunc{mw, handler}
			_, _ = testutil.RunChain(t, chain, "GET", "/",
				celeristest.WithBasicAuth("admin", "pass"))
			if username != tt.wantUser {
				t.Fatalf("UsernameFromContext: got %q, want %q", username, tt.wantUser)
			}
		})
	}
}

func TestUsernameKeyConstant(t *testing.T) {
	if UsernameKey != "basicauth_username" {
		t.Fatalf("UsernameKey: got %q, want %q", UsernameKey, "basicauth_username")
	}
}

func TestValidatorWithContext(t *testing.T) {
	mw := New(Config{
		ValidatorWithContext: func(c *celeris.Context, user, pass string) bool {
			return user == "admin" && pass == "secret" && c.Header("x-tenant") == "acme"
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithBasicAuth("admin", "secret"),
		celeristest.WithHeader("x-tenant", "acme"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	_, err = testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithBasicAuth("admin", "secret"),
	)
	testutil.AssertHTTPError(t, err, 401)
}

func TestValidatorWithContextTakesPrecedence(t *testing.T) {
	called := false
	mw := New(Config{
		Validator: func(_, _ string) bool { return true },
		ValidatorWithContext: func(_ *celeris.Context, u, p string) bool {
			called = true
			return u == "ctx" && p == "pass"
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithBasicAuth("ctx", "pass"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if !called {
		t.Fatal("expected ValidatorWithContext to be called")
	}
}

func TestValidatorWithContextAloneWorks(t *testing.T) {
	mw := New(Config{
		ValidatorWithContext: func(_ *celeris.Context, u, p string) bool {
			return u == "solo" && p == "pass"
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithBasicAuth("solo", "pass"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestRealmEscaping(t *testing.T) {
	tests := []struct {
		name      string
		realm     string
		wantRealm string
	}{
		{"quotes", `My "App"`, `Basic realm="My \"App\""`},
		{"backslash", `Path\to`, `Basic realm="Path\\to"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := New(Config{
				Validator: validatorFor("admin", "secret"),
				Realm:     tt.realm,
			})
			ctx, _ := celeristest.NewContextT(t, "GET", "/")
			err := mw(ctx)
			testutil.AssertHTTPError(t, err, 401)
			found := false
			for _, h := range ctx.ResponseHeaders() {
				if h[0] == "www-authenticate" && h[1] == tt.wantRealm {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected realm %q, got %v", tt.wantRealm, ctx.ResponseHeaders())
			}
		})
	}
}

func TestUsersMapDeepCopy(t *testing.T) {
	users := map[string]string{"admin": "secret"}
	mw := New(Config{Users: users})

	users["admin"] = "changed"
	users["hacker"] = "injected"

	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithBasicAuth("admin", "secret"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	_, err = testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithBasicAuth("admin", "changed"))
	testutil.AssertHTTPError(t, err, 401)

	_, err = testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithBasicAuth("hacker", "injected"))
	testutil.AssertHTTPError(t, err, 401)
}

func TestErrorHandlerReceivesErrUnauthorized(t *testing.T) {
	var receivedErr error
	mw := New(Config{
		Validator: validatorFor("admin", "secret"),
		ErrorHandler: func(c *celeris.Context, err error) error {
			receivedErr = err
			c.SetHeader("www-authenticate", `Basic realm="Test"`)
			return celeris.NewHTTPError(401, "custom")
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithBasicAuth("admin", "wrong"),
	)
	testutil.AssertHTTPError(t, err, 401)
	if receivedErr == nil {
		t.Fatal("ErrorHandler did not receive error parameter")
	}
	if receivedErr != ErrUnauthorized {
		t.Fatalf("expected ErrUnauthorized, got %v", receivedErr)
	}
}

func FuzzBasicAuthHeader(f *testing.F) {
	f.Add("Basic YWRtaW46c2VjcmV0") // admin:secret
	f.Add("Basic !!!")
	f.Add("Bearer token")
	f.Add("")
	f.Add("Basic ")
	f.Add("basic YWRtaW46c2VjcmV0")
	mw := New(Config{
		Validator: func(u, p string) bool { return u == "admin" && p == "secret" },
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	f.Fuzz(func(t *testing.T, header string) {
		opts := []celeristest.Option{}
		if header != "" {
			opts = append(opts, celeristest.WithHeader("authorization", header))
		}
		_, _ = testutil.RunChain(t, chain, "GET", "/fuzz", opts...)
	})
}

// --- HashedUsers (SHA-256) tests ---

func TestHashPasswordDeterministic(t *testing.T) {
	h1 := HashPassword("secret")
	h2 := HashPassword("secret")
	if h1 != h2 {
		t.Fatalf("HashPassword not deterministic: %q != %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("HashPassword length: got %d, want 64", len(h1))
	}
	if _, err := hex.DecodeString(h1); err != nil {
		t.Fatalf("HashPassword not valid hex: %v", err)
	}
}

func TestHashPasswordMatchesSHA256(t *testing.T) {
	password := "secret"
	expected := sha256.Sum256([]byte(password))
	got := HashPassword(password)
	if got != hex.EncodeToString(expected[:]) {
		t.Fatalf("HashPassword mismatch: got %q, want %q",
			got, hex.EncodeToString(expected[:]))
	}
}

func TestHashedUsers(t *testing.T) {
	tests := []struct {
		name     string
		user     string
		pass     string
		wantCode int
		wantUser string
	}{
		{"valid admin", "admin", "secret", 200, "admin"},
		{"valid user2", "user2", "pass2", 200, "user2"},
		{"wrong password", "admin", "wrong", 401, ""},
		{"unknown user", "unknown", "secret", 401, ""},
	}
	mw := New(Config{
		HashedUsers: map[string]string{
			"admin": HashPassword("secret"),
			"user2": HashPassword("pass2"),
		},
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var storedUser string
			handler := func(c *celeris.Context) error {
				storedUser = UsernameFromContext(c)
				return c.String(200, "ok")
			}
			chain := []celeris.HandlerFunc{mw, handler}
			rec, err := testutil.RunChain(t, chain, "GET", "/",
				celeristest.WithBasicAuth(tt.user, tt.pass))
			if tt.wantCode == 200 {
				testutil.AssertNoError(t, err)
				testutil.AssertStatus(t, rec, 200)
				if storedUser != tt.wantUser {
					t.Fatalf("stored user: got %q, want %q", storedUser, tt.wantUser)
				}
			} else {
				testutil.AssertHTTPError(t, err, tt.wantCode)
			}
		})
	}
}

func TestHashedUsersInvalidHexPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for invalid hex hash")
		}
	}()
	New(Config{HashedUsers: map[string]string{"admin": "not-hex"}})
}

func TestHashedUsersWrongLengthPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for wrong-length hash")
		}
	}()
	New(Config{HashedUsers: map[string]string{"admin": "abcd"}})
}

func TestHashedUsersDeepCopy(t *testing.T) {
	hashed := map[string]string{"admin": HashPassword("secret")}
	mw := New(Config{HashedUsers: hashed})

	hashed["admin"] = HashPassword("changed")
	hashed["hacker"] = HashPassword("injected")

	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithBasicAuth("admin", "secret"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	_, err = testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithBasicAuth("admin", "changed"))
	testutil.AssertHTTPError(t, err, 401)

	_, err = testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithBasicAuth("hacker", "injected"))
	testutil.AssertHTTPError(t, err, 401)
}

func TestUsersMapTakesPrecedenceOverHashedUsers(t *testing.T) {
	mw := New(Config{
		Users:       map[string]string{"admin": "plain"},
		HashedUsers: map[string]string{"admin": HashPassword("hashed")},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithBasicAuth("admin", "plain"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	_, err = testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithBasicAuth("admin", "hashed"))
	testutil.AssertHTTPError(t, err, 401)
}

// --- SkipPaths tests ---

func TestSkipPaths(t *testing.T) {
	mw := New(Config{
		Validator: validatorFor("admin", "secret"),
		SkipPaths: []string{"/health", "/ready", "/metrics"},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	for _, path := range []string{"/health", "/ready", "/metrics"} {
		t.Run("skip "+path, func(t *testing.T) {
			rec, err := testutil.RunChain(t, chain, "GET", path)
			testutil.AssertNoError(t, err)
			testutil.AssertStatus(t, rec, 200)
			testutil.AssertBodyContains(t, rec, "ok")
		})
	}

	t.Run("non-skip requires auth", func(t *testing.T) {
		_, err := testutil.RunChain(t, chain, "GET", "/secret")
		testutil.AssertHTTPError(t, err, 401)
	})

	t.Run("non-skip with auth works", func(t *testing.T) {
		rec, err := testutil.RunChain(t, chain, "GET", "/api",
			celeristest.WithBasicAuth("admin", "secret"))
		testutil.AssertNoError(t, err)
		testutil.AssertStatus(t, rec, 200)
	})
}

// --- Cache-Control + Vary header tests ---

func TestCacheControlAndVaryOnFailure(t *testing.T) {
	mw := New(Config{Validator: validatorFor("admin", "secret")})

	tests := []struct {
		name string
		opts []celeristest.Option
	}{
		{"missing header", nil},
		{"wrong creds", []celeristest.Option{celeristest.WithBasicAuth("admin", "wrong")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, _ := celeristest.NewContextT(t, "GET", "/", tt.opts...)
			err := mw(ctx)
			testutil.AssertHTTPError(t, err, 401)
			assertResponseHeader(t, ctx, "cache-control", "no-store")
			assertResponseHeader(t, ctx, "vary", "authorization")
		})
	}
}

func TestNoCacheControlOnSuccess(t *testing.T) {
	mw := New(Config{Validator: validatorFor("admin", "secret")})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	ctx, rec := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithHandlers(chain...),
		celeristest.WithBasicAuth("admin", "secret"))
	err := ctx.Next()
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "cache-control" {
			t.Fatal("cache-control should not be set on successful auth")
		}
	}
}

// --- HashedUsersFunc (custom verify) tests ---

func TestHashedUsersFunc(t *testing.T) {
	tests := []struct {
		name     string
		user     string
		pass     string
		wantCode int
	}{
		{"valid", "admin", "secret", 200},
		{"wrong password", "admin", "wrong", 401},
		{"unknown user", "unknown", "secret", 401},
	}
	mw := New(Config{
		HashedUsers: map[string]string{
			"admin": "bcrypt:secret",
		},
		HashedUsersFunc: func(hash, password string) bool {
			return hash == "bcrypt:"+password
		},
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := func(c *celeris.Context) error {
				return c.String(200, "ok")
			}
			chain := []celeris.HandlerFunc{mw, handler}
			rec, err := testutil.RunChain(t, chain, "GET", "/",
				celeristest.WithBasicAuth(tt.user, tt.pass))
			if tt.wantCode == 200 {
				testutil.AssertNoError(t, err)
				testutil.AssertStatus(t, rec, 200)
			} else {
				testutil.AssertHTTPError(t, err, tt.wantCode)
			}
		})
	}
}

func TestHashedUsersFuncSkipsSHA256Validation(t *testing.T) {
	mw := New(Config{
		HashedUsers: map[string]string{
			"admin": "$2a$10$notHexAtAll",
		},
		HashedUsersFunc: func(hash, password string) bool {
			return hash == "$2a$10$notHexAtAll" && password == "pass"
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithBasicAuth("admin", "pass"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

// assertResponseHeader is a test helper that checks a response header on Context.
func assertResponseHeader(t *testing.T, ctx *celeris.Context, key, want string) {
	t.Helper()
	for _, h := range ctx.ResponseHeaders() {
		if h[0] == key && h[1] == want {
			return
		}
	}
	t.Fatalf("expected response header %q=%q, got %v", key, want, ctx.ResponseHeaders())
}
