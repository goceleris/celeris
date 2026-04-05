package methodoverride

import (
	"net/url"
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// --- Override via header ---

func TestHeaderOverride(t *testing.T) {
	mw := New()
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithHeader(strings.ToLower(DefaultHeader), "PUT"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "PUT" {
		t.Fatalf("method: got %q, want PUT", method)
	}
}

func TestHeaderOverrideCaseInsensitive(t *testing.T) {
	mw := New()
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithHeader(strings.ToLower(DefaultHeader), "delete"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "DELETE" {
		t.Fatalf("method: got %q, want DELETE", method)
	}
}

// --- Override via form field ---

func TestFormFieldOverride(t *testing.T) {
	mw := New()
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	form := url.Values{DefaultFormField: {"PATCH"}}
	rec, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithBody([]byte(form.Encode())),
		celeristest.WithContentType("application/x-www-form-urlencoded"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "PATCH" {
		t.Fatalf("method: got %q, want PATCH", method)
	}
}

// --- Form field takes precedence over header ---

func TestFormFieldPrecedenceOverHeader(t *testing.T) {
	mw := New()
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	form := url.Values{DefaultFormField: {"PATCH"}}
	rec, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithBody([]byte(form.Encode())),
		celeristest.WithContentType("application/x-www-form-urlencoded"),
		celeristest.WithHeader(strings.ToLower(DefaultHeader), "DELETE"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "PATCH" {
		t.Fatalf("method: got %q, want PATCH (form field precedence)", method)
	}
}

// --- Non-POST requests are not overridden ---

func TestNonPostNotOverridden(t *testing.T) {
	mw := New()
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	for _, m := range []string{"GET", "PUT", "DELETE", "PATCH"} {
		t.Run(m, func(t *testing.T) {
			rec, err := testutil.RunChain(t, chain, m, "/",
				celeristest.WithHeader(strings.ToLower(DefaultHeader), "POST"),
			)
			testutil.AssertNoError(t, err)
			testutil.AssertStatus(t, rec, 200)
			if method != m {
				t.Fatalf("method: got %q, want %q (should not override)", method, m)
			}
		})
	}
}

// --- No override header/field → method unchanged ---

func TestNoOverrideValue(t *testing.T) {
	mw := New()
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "POST" {
		t.Fatalf("method: got %q, want POST", method)
	}
}

// --- Custom AllowedMethods ---

func TestCustomAllowedMethods(t *testing.T) {
	mw := New(Config{
		AllowedMethods: []string{"GET", "POST"},
	})
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// GET with override header should work.
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader(strings.ToLower(DefaultHeader), "DELETE"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "DELETE" {
		t.Fatalf("method: got %q, want DELETE", method)
	}
}

// --- Custom Getter ---

func TestCustomGetter(t *testing.T) {
	mw := New(Config{
		Getter: func(c *celeris.Context) string {
			return c.Header("x-custom-method")
		},
	})
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithHeader("x-custom-method", "PUT"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "PUT" {
		t.Fatalf("method: got %q, want PUT", method)
	}
}

// --- HeaderGetter factory ---

func TestHeaderGetter(t *testing.T) {
	mw := New(Config{Getter: HeaderGetter("X-Method")})
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithHeader("x-method", "DELETE"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "DELETE" {
		t.Fatalf("method: got %q, want DELETE", method)
	}
}

// --- FormFieldGetter factory ---

func TestFormFieldGetter(t *testing.T) {
	mw := New(Config{Getter: FormFieldGetter("http_method")})
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	form := url.Values{"http_method": {"PATCH"}}
	rec, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithBody([]byte(form.Encode())),
		celeristest.WithContentType("application/x-www-form-urlencoded"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "PATCH" {
		t.Fatalf("method: got %q, want PATCH", method)
	}
}

// --- FormThenHeaderGetter factory ---

func TestFormThenHeaderGetter(t *testing.T) {
	getter := FormThenHeaderGetter("_method", "X-Override")

	t.Run("form field wins", func(t *testing.T) {
		mw := New(Config{Getter: getter})
		var method string
		handler := func(c *celeris.Context) error {
			method = c.Method()
			return c.String(200, "ok")
		}
		chain := []celeris.HandlerFunc{mw, handler}
		form := url.Values{"_method": {"PATCH"}}
		rec, err := testutil.RunChain(t, chain, "POST", "/",
			celeristest.WithBody([]byte(form.Encode())),
			celeristest.WithContentType("application/x-www-form-urlencoded"),
			celeristest.WithHeader("x-override", "DELETE"),
		)
		testutil.AssertNoError(t, err)
		testutil.AssertStatus(t, rec, 200)
		if method != "PATCH" {
			t.Fatalf("method: got %q, want PATCH", method)
		}
	})

	t.Run("header fallback", func(t *testing.T) {
		mw := New(Config{Getter: getter})
		var method string
		handler := func(c *celeris.Context) error {
			method = c.Method()
			return c.String(200, "ok")
		}
		chain := []celeris.HandlerFunc{mw, handler}
		rec, err := testutil.RunChain(t, chain, "POST", "/",
			celeristest.WithHeader("x-override", "DELETE"),
		)
		testutil.AssertNoError(t, err)
		testutil.AssertStatus(t, rec, 200)
		if method != "DELETE" {
			t.Fatalf("method: got %q, want DELETE", method)
		}
	})

	t.Run("neither set", func(t *testing.T) {
		mw := New(Config{Getter: getter})
		var method string
		handler := func(c *celeris.Context) error {
			method = c.Method()
			return c.String(200, "ok")
		}
		chain := []celeris.HandlerFunc{mw, handler}
		rec, err := testutil.RunChain(t, chain, "POST", "/")
		testutil.AssertNoError(t, err)
		testutil.AssertStatus(t, rec, 200)
		if method != "POST" {
			t.Fatalf("method: got %q, want POST (no override)", method)
		}
	})
}

// --- QueryGetter factory ---

func TestQueryGetter(t *testing.T) {
	mw := New(Config{
		Getter: QueryGetter("_method"),
	})
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// POST /api?_method=PUT → method becomes PUT
	rec, err := testutil.RunChain(t, chain, "POST", "/api",
		celeristest.WithQuery("_method", "PUT"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "PUT" {
		t.Fatalf("method: got %q, want PUT", method)
	}
}

func TestQueryGetterNoParam(t *testing.T) {
	mw := New(Config{
		Getter: QueryGetter("_method"),
	})
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// POST /api without query param → method stays POST
	rec, err := testutil.RunChain(t, chain, "POST", "/api")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "POST" {
		t.Fatalf("method: got %q, want POST (no query param)", method)
	}
}

func TestQueryGetterCaseInsensitive(t *testing.T) {
	mw := New(Config{
		Getter: QueryGetter("_method"),
	})
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// POST /api?_method=delete → method becomes DELETE (uppercased)
	rec, err := testutil.RunChain(t, chain, "POST", "/api",
		celeristest.WithQuery("_method", "delete"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "DELETE" {
		t.Fatalf("method: got %q, want DELETE", method)
	}
}

// --- Skip ---

func TestSkipBypassesOverride(t *testing.T) {
	mw := New(Config{
		Skip: func(_ *celeris.Context) bool { return true },
	})
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithHeader(strings.ToLower(DefaultHeader), "PUT"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "POST" {
		t.Fatalf("method: got %q, want POST (skip should bypass)", method)
	}
}

func TestSkipPaths(t *testing.T) {
	mw := New(Config{SkipPaths: []string{"/health"}})
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := testutil.RunChain(t, chain, "POST", "/health",
		celeristest.WithHeader(strings.ToLower(DefaultHeader), "PUT"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "POST" {
		t.Fatalf("method: got %q, want POST (skip path)", method)
	}

	rec, err = testutil.RunChain(t, chain, "POST", "/api",
		celeristest.WithHeader(strings.ToLower(DefaultHeader), "DELETE"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "DELETE" {
		t.Fatalf("method: got %q, want DELETE", method)
	}
}

// --- Validation ---

func TestValidateEmptyStringPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for empty string in AllowedMethods")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "AllowedMethods must not contain empty") {
			t.Fatalf("unexpected panic message: %v", r)
		}
	}()
	New(Config{AllowedMethods: []string{"POST", ""}})
}

func TestValidateWhitespaceOnlyPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for whitespace-only string in AllowedMethods")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "AllowedMethods must not contain empty") {
			t.Fatalf("unexpected panic message: %v", r)
		}
	}()
	New(Config{AllowedMethods: []string{"  \t  "}})
}

func TestValidateValidMethodsNoPanic(t *testing.T) {
	New(Config{AllowedMethods: []string{"POST", "GET"}})
}

// --- Default config ---

func TestDefaultConfigOverridesOnlyPOST(t *testing.T) {
	mw := New()
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	rec, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithHeader(strings.ToLower(DefaultHeader), "PUT"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "PUT" {
		t.Fatalf("method: got %q, want PUT", method)
	}
}

// --- Constants ---

func TestDefaultConstants(t *testing.T) {
	if DefaultHeader != "X-HTTP-Method-Override" {
		t.Fatalf("DefaultHeader: got %q, want X-HTTP-Method-Override", DefaultHeader)
	}
	if DefaultFormField != "_method" {
		t.Fatalf("DefaultFormField: got %q, want _method", DefaultFormField)
	}
}

// --- TargetMethods ---

func TestTargetMethodsBlocksCONNECT(t *testing.T) {
	mw := New() // default TargetMethods: PUT, DELETE, PATCH
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithHeader(strings.ToLower(DefaultHeader), "CONNECT"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "POST" {
		t.Fatalf("method: got %q, want POST (CONNECT should be blocked by default TargetMethods)", method)
	}
}

func TestTargetMethodsBlocksGETByDefault(t *testing.T) {
	mw := New() // default TargetMethods: PUT, DELETE, PATCH
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithHeader(strings.ToLower(DefaultHeader), "GET"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "POST" {
		t.Fatalf("method: got %q, want POST (GET should be blocked by default TargetMethods)", method)
	}
}

func TestTargetMethodsBlocksTRACE(t *testing.T) {
	mw := New()
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithHeader(strings.ToLower(DefaultHeader), "TRACE"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "POST" {
		t.Fatalf("method: got %q, want POST (TRACE should be blocked by default TargetMethods)", method)
	}
}

func TestCustomTargetMethods(t *testing.T) {
	mw := New(Config{
		TargetMethods: []string{"GET", "HEAD"},
	})
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// GET is in custom TargetMethods, should work.
	rec, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithHeader(strings.ToLower(DefaultHeader), "GET"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "GET" {
		t.Fatalf("method: got %q, want GET (custom TargetMethods)", method)
	}

	// PUT is NOT in custom TargetMethods, should be blocked.
	rec, err = testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithHeader(strings.ToLower(DefaultHeader), "PUT"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "POST" {
		t.Fatalf("method: got %q, want POST (PUT not in custom TargetMethods)", method)
	}
}

func TestTargetMethodsCaseInsensitive(t *testing.T) {
	mw := New(Config{
		TargetMethods: []string{"put"},
	})
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithHeader(strings.ToLower(DefaultHeader), "PUT"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "PUT" {
		t.Fatalf("method: got %q, want PUT (case insensitive TargetMethods)", method)
	}
}

func TestValidateTargetMethodsEmptyPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for empty string in TargetMethods")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "TargetMethods must not contain empty") {
			t.Fatalf("unexpected panic message: %v", r)
		}
	}()
	New(Config{TargetMethods: []string{"PUT", ""}})
}

func TestValidateTargetMethodsWhitespacePanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for whitespace-only string in TargetMethods")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "TargetMethods must not contain empty") {
			t.Fatalf("unexpected panic message: %v", r)
		}
	}()
	New(Config{TargetMethods: []string{"  \t  "}})
}

// --- Same method → no SetMethod call ---

func TestSameMethodNoRewrite(t *testing.T) {
	mw := New()
	var method string
	handler := func(c *celeris.Context) error {
		method = c.Method()
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/",
		celeristest.WithHeader(strings.ToLower(DefaultHeader), "POST"),
	)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if method != "POST" {
		t.Fatalf("method: got %q, want POST", method)
	}
}
