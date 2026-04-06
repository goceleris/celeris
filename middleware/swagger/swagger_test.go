package swagger

import (
	"testing"
	"testing/fstest"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/internal/testutil"
)

var testSpec = []byte(`{"openapi":"3.0.0","info":{"title":"Test","version":"1.0"}}`)

func okHandler(c *celeris.Context) error {
	return c.String(200, "ok")
}

func TestSwaggerUIPage(t *testing.T) {
	t.Parallel()
	mw := New(Config{SpecContent: testSpec})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "swagger-ui")
	testutil.AssertHeaderContains(t, rec, "content-type", "text/html")
}

func TestSwaggerSpec(t *testing.T) {
	t.Parallel()
	mw := New(Config{SpecContent: testSpec})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/doc.json")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertHeaderContains(t, rec, "content-type", "application/json")
	body := rec.BodyString()
	if body != string(testSpec) {
		t.Fatalf("spec body: got %q, want %q", body, string(testSpec))
	}
}

func TestSwaggerScalarUI(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		SpecContent: testSpec,
		UIEngine:    "scalar",
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "scalar")
	testutil.AssertHeaderContains(t, rec, "content-type", "text/html")
}

func TestSwaggerAuthDenied(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		SpecContent: testSpec,
		AuthFunc:    func(_ *celeris.Context) bool { return false },
	})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 403)

	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/doc.json")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 403)
}

func TestSwaggerFromFS(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"openapi.json": &fstest.MapFile{Data: testSpec},
	}
	mw := New(Config{Filesystem: fsys})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/doc.json")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	body := rec.BodyString()
	if body != string(testSpec) {
		t.Fatalf("spec body: got %q, want %q", body, string(testSpec))
	}
}

func TestSwaggerPassthrough(t *testing.T) {
	t.Parallel()
	mw := New(Config{SpecContent: testSpec})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/api/users")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestSwaggerCustomPaths(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		SpecContent: testSpec,
		UIPath:      "/docs",
		SpecURL:     "/docs/openapi.json",
	})

	// Custom UI path.
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/docs")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "swagger-ui")
	testutil.AssertBodyContains(t, rec, "/docs/openapi.json")

	// Custom spec URL.
	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/docs/openapi.json")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	body := rec.BodyString()
	if body != string(testSpec) {
		t.Fatalf("spec body: got %q, want %q", body, string(testSpec))
	}

	// Default paths should pass through.
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err = testutil.RunChain(t, chain, "GET", "/swagger")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestSwaggerSkipPaths(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		SpecContent: testSpec,
		SkipPaths:   []string{"/swagger"},
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/swagger")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestValidatePanicNoSpec(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when neither SpecContent nor Filesystem is set")
		}
	}()
	New(Config{})
}

func TestSwaggerUIPathTrailingSlash(t *testing.T) {
	t.Parallel()
	mw := New(Config{SpecContent: testSpec})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "swagger-ui")
}

func TestSwaggerAuthAllowed(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		SpecContent: testSpec,
		AuthFunc:    func(_ *celeris.Context) bool { return true },
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "swagger-ui")
}

func TestSwaggerSpecContentIsCopied(t *testing.T) {
	t.Parallel()
	spec := make([]byte, len(testSpec))
	copy(spec, testSpec)
	mw := New(Config{SpecContent: spec})

	// Mutate the original slice after creating the middleware.
	spec[0] = 'X'

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/doc.json")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	body := rec.BodyString()
	if body != string(testSpec) {
		t.Fatalf("spec mutation leaked: got %q, want %q", body, string(testSpec))
	}
}

func TestValidatePanicUIPathNoSlash(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for UIPath without leading /")
		}
	}()
	New(Config{SpecContent: testSpec, UIPath: "swagger"})
}

func TestValidatePanicSpecURLNoSlash(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for SpecURL without leading /")
		}
	}()
	New(Config{SpecContent: testSpec, SpecURL: "swagger/doc.json"})
}

func TestSwaggerFSCustomSpecFile(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"api/spec.yaml": &fstest.MapFile{Data: testSpec},
	}
	mw := New(Config{
		Filesystem: fsys,
		SpecFile:   "api/spec.yaml",
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/doc.json")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	body := rec.BodyString()
	if body != string(testSpec) {
		t.Fatalf("spec body: got %q, want %q", body, string(testSpec))
	}
}

func TestSwaggerConcurrentAccess(t *testing.T) {
	t.Parallel()
	mw := New(Config{SpecContent: testSpec})

	const goroutines = 10
	const iterations = 50

	errs := make(chan error, goroutines*iterations)
	done := make(chan struct{})
	for range goroutines {
		go func() {
			for range iterations {
				ctx, _ := celeristest.NewContext("GET", "/swagger/doc.json")
				err := mw(ctx)
				if err != nil {
					errs <- err
				}
				celeristest.ReleaseContext(ctx)
			}
			done <- struct{}{}
		}()
	}
	for range goroutines {
		<-done
	}
	close(errs)
	for err := range errs {
		t.Fatalf("unexpected error: %v", err)
	}
}
