package swagger

import (
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

var jsonSpec = []byte(`{"openapi":"3.0.0","info":{"title":"Test","version":"1.0"}}`)
var yamlSpec = []byte(`openapi: "3.0.0"
info:
  title: Test
  version: "1.0"
`)

func okHandler(c *celeris.Context) error {
	return c.String(200, "ok")
}

// --- Basic functionality ---

func TestDefaultsServeUI(t *testing.T) {
	t.Parallel()
	mw := New(Config{SpecContent: jsonSpec})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertHeaderContains(t, rec, "content-type", "text/html")
	testutil.AssertBodyContains(t, rec, "swagger-ui")
	testutil.AssertBodyContains(t, rec, "API Documentation")
}

func TestDefaultsServeSpec(t *testing.T) {
	t.Parallel()
	mw := New(Config{SpecContent: jsonSpec})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/spec")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertHeaderContains(t, rec, "content-type", "application/json")
	testutil.AssertBodyContains(t, rec, `"openapi"`)
}

func TestBasePathRedirect(t *testing.T) {
	t.Parallel()
	mw := New(Config{SpecContent: jsonSpec})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "/swagger/")
}

func TestNonSwaggerPathPassesThrough(t *testing.T) {
	t.Parallel()
	mw := New(Config{SpecContent: jsonSpec})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/api/users")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestPOSTReturns405(t *testing.T) {
	t.Parallel()
	mw := New(Config{SpecContent: jsonSpec})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "POST", "/swagger/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 405)
}

func TestHEADMethodAllowed(t *testing.T) {
	t.Parallel()
	mw := New(Config{SpecContent: jsonSpec})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "HEAD", "/swagger/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

// --- Issue 1: Content-Type detection ---

func TestSpecContentTypeJSON(t *testing.T) {
	t.Parallel()
	mw := New(Config{SpecContent: jsonSpec})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/spec")
	testutil.AssertNoError(t, err)
	testutil.AssertHeaderContains(t, rec, "content-type", "application/json")
}

func TestSpecContentTypeYAML(t *testing.T) {
	t.Parallel()
	mw := New(Config{SpecContent: yamlSpec})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/spec")
	testutil.AssertNoError(t, err)
	testutil.AssertHeaderContains(t, rec, "content-type", "application/x-yaml")
}

func TestSpecContentTypeFromExtension(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		file     string
		content  []byte
		expected string
	}{
		{"json ext", "openapi.json", yamlSpec, "application/json"},
		{"yaml ext", "openapi.yaml", jsonSpec, "application/x-yaml"},
		{"yml ext", "openapi.yml", jsonSpec, "application/x-yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mw := New(Config{SpecContent: tt.content, SpecFile: tt.file})
			rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/spec")
			testutil.AssertNoError(t, err)
			testutil.AssertHeaderContains(t, rec, "content-type", tt.expected)
		})
	}
}

func TestDetectSpecContentType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		content  []byte
		filename string
		expected string
	}{
		{"json bytes", []byte(`{"openapi":"3.0"}`), "", "application/json"},
		{"json array", []byte(`[{"openapi":"3.0"}]`), "", "application/json"},
		{"yaml bytes", []byte(`openapi: "3.0"`), "", "application/x-yaml"},
		{"whitespace then json", []byte("  \n\t{\"openapi\":\"3.0\"}"), "", "application/json"},
		{"whitespace then yaml", []byte("  \n\topenapi: 3.0"), "", "application/x-yaml"},
		{"ext overrides content", []byte(`openapi: "3.0"`), "spec.json", "application/json"},
		{"yaml ext overrides", []byte(`{"openapi":"3.0"}`), "spec.yaml", "application/x-yaml"},
		{"yml ext", []byte(`{"openapi":"3.0"}`), "spec.yml", "application/x-yaml"},
		{"empty content", []byte{}, "", "application/json"},
		{"unknown ext falls back to content", []byte(`openapi: "3.0"`), "spec.txt", "application/x-yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := detectSpecContentType(tt.content, tt.filename)
			if got != tt.expected {
				t.Fatalf("detectSpecContentType: got %q, want %q", got, tt.expected)
			}
		})
	}
}

// --- Issue 2: UI customization ---

func TestUIConfigSwaggerUI(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		SpecContent: jsonSpec,
		UI: UIConfig{
			DocExpansion:             "full",
			DeepLinking:              true,
			PersistAuthorization:     true,
			DefaultModelsExpandDepth: -1,
			Title:                    "My API",
		},
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	body := rec.BodyString()
	assertContains(t, body, `docExpansion: "full"`)
	assertContains(t, body, `deepLinking: true`)
	assertContains(t, body, `persistAuthorization: true`)
	assertContains(t, body, `defaultModelsExpandDepth: -1`)
	assertContains(t, body, `<title>My API</title>`)
}

func TestUIConfigDefaults(t *testing.T) {
	t.Parallel()
	mw := New(Config{SpecContent: jsonSpec})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/")
	testutil.AssertNoError(t, err)
	body := rec.BodyString()
	assertContains(t, body, `docExpansion: "list"`)
	assertContains(t, body, `deepLinking: false`)
	assertContains(t, body, `persistAuthorization: false`)
	assertContains(t, body, `defaultModelsExpandDepth: 1`)
	assertContains(t, body, `<title>API Documentation</title>`)
}

func TestScalarRenderer(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		SpecContent: jsonSpec,
		Renderer:    RendererScalar,
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	body := rec.BodyString()
	assertContains(t, body, "api-reference")
	assertContains(t, body, "@scalar/api-reference")
	assertContains(t, body, `<title>API Documentation</title>`)
}

func TestScalarRendererWithTitle(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		SpecContent: jsonSpec,
		Renderer:    RendererScalar,
		UI: UIConfig{
			Title: "My Scalar API",
		},
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/")
	testutil.AssertNoError(t, err)
	body := rec.BodyString()
	assertContains(t, body, `<title>My Scalar API</title>`)
}

// --- Issue 3: Local assets ---

func TestAssetsPathSwaggerUI(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		SpecContent: jsonSpec,
		AssetsPath:  "/static/swagger",
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/")
	testutil.AssertNoError(t, err)
	body := rec.BodyString()
	assertContains(t, body, `/static/swagger/swagger-ui.css`)
	assertContains(t, body, `/static/swagger/swagger-ui-bundle.js`)
	assertContains(t, body, `/static/swagger/swagger-ui-standalone-preset.js`)
	assertNotContains(t, body, "unpkg.com")
}

func TestAssetsPathScalar(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		SpecContent: jsonSpec,
		Renderer:    RendererScalar,
		AssetsPath:  "/assets",
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/")
	testutil.AssertNoError(t, err)
	body := rec.BodyString()
	assertContains(t, body, `/assets/standalone.min.js`)
	assertNotContains(t, body, "cdn.jsdelivr.net")
}

func TestDefaultCDNUsed(t *testing.T) {
	t.Parallel()
	mw := New(Config{SpecContent: jsonSpec})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/")
	testutil.AssertNoError(t, err)
	body := rec.BodyString()
	assertContains(t, body, "unpkg.com/swagger-ui-dist@5")
}

func TestAssetsPathTrailingSlash(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		SpecContent: jsonSpec,
		AssetsPath:  "/assets/",
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/")
	testutil.AssertNoError(t, err)
	body := rec.BodyString()
	assertContains(t, body, `/assets/swagger-ui-bundle.js`)
	assertNotContains(t, body, `/assets//swagger-ui-bundle.js`)
}

// --- Config edge cases ---

func TestCustomBasePath(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		SpecContent: jsonSpec,
		BasePath:    "/docs",
	})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/docs/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertHeaderContains(t, rec, "content-type", "text/html")

	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/docs/spec")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	// Original path should pass through.
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err = testutil.RunChain(t, chain, "GET", "/swagger/")
	testutil.AssertNoError(t, err)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestSpecURL(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		SpecURL: "https://petstore.swagger.io/v2/swagger.json",
	})

	// UI page should reference the external URL.
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "petstore.swagger.io")

	// Spec endpoint returns 404 since content is external.
	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/spec")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 404)
}

func TestSkipBypassesSwagger(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		SpecContent: jsonSpec,
		Skip:        func(_ *celeris.Context) bool { return true },
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/swagger/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestSkipPaths(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		SpecContent: jsonSpec,
		SkipPaths:   []string{"/swagger/"},
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/swagger/")
	testutil.AssertNoError(t, err)
	testutil.AssertBodyContains(t, rec, "ok")

	// spec should still work.
	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/spec")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

// --- Validation panics ---

func TestValidatePanicsNoSpec(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for missing spec")
		}
	}()
	New(Config{})
}

func TestValidatePanicsBadBasePath(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for bad BasePath")
		}
	}()
	New(Config{SpecContent: jsonSpec, BasePath: "no-slash"})
}

func TestValidatePanicsUnknownRenderer(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for unknown renderer")
		}
	}()
	New(Config{SpecContent: jsonSpec, Renderer: "redoc"})
}

func TestValidatePanicsUnknownDocExpansion(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for unknown DocExpansion")
		}
	}()
	New(Config{SpecContent: jsonSpec, UI: UIConfig{DocExpansion: "invalid"}})
}

// --- applyDefaults ---

func TestApplyDefaultsFillsEmpty(t *testing.T) {
	t.Parallel()
	cfg := applyDefaults(Config{})
	if cfg.BasePath != "/swagger" {
		t.Fatalf("BasePath: got %q, want /swagger", cfg.BasePath)
	}
	if cfg.Renderer != RendererSwaggerUI {
		t.Fatalf("Renderer: got %q, want %q", cfg.Renderer, RendererSwaggerUI)
	}
	if cfg.UI.DocExpansion != "list" {
		t.Fatalf("DocExpansion: got %q, want list", cfg.UI.DocExpansion)
	}
	if cfg.UI.DefaultModelsExpandDepth != 1 {
		t.Fatalf("DefaultModelsExpandDepth: got %d, want 1", cfg.UI.DefaultModelsExpandDepth)
	}
	if cfg.UI.Title != "API Documentation" {
		t.Fatalf("Title: got %q, want API Documentation", cfg.UI.Title)
	}
}

func TestApplyDefaultsPreservesCustom(t *testing.T) {
	t.Parallel()
	cfg := applyDefaults(Config{
		BasePath: "/api-docs",
		Renderer: RendererScalar,
		UI: UIConfig{
			DocExpansion:             "full",
			DefaultModelsExpandDepth: -1,
			Title:                    "Custom",
		},
	})
	if cfg.BasePath != "/api-docs" {
		t.Fatalf("BasePath: got %q, want /api-docs", cfg.BasePath)
	}
	if cfg.Renderer != RendererScalar {
		t.Fatalf("Renderer: got %q, want %q", cfg.Renderer, RendererScalar)
	}
	if cfg.UI.DocExpansion != "full" {
		t.Fatalf("DocExpansion: got %q, want full", cfg.UI.DocExpansion)
	}
	if cfg.UI.DefaultModelsExpandDepth != -1 {
		t.Fatalf("DefaultModelsExpandDepth: got %d, want -1", cfg.UI.DefaultModelsExpandDepth)
	}
	if cfg.UI.Title != "Custom" {
		t.Fatalf("Title: got %q, want Custom", cfg.UI.Title)
	}
}

// --- Fuzz ---

func FuzzSwaggerPaths(f *testing.F) {
	f.Add("/swagger/")
	f.Add("/swagger/spec")
	f.Add("/swagger")
	f.Add("/api/users")
	f.Add("")
	f.Add("/")
	f.Fuzz(func(t *testing.T, path string) {
		mw := New(Config{SpecContent: jsonSpec})
		ctx, _ := celeristest.NewContextT(t, "GET", path)
		_ = mw(ctx)
	})
}

// --- helpers ---

func assertContains(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Fatalf("expected body to contain %q, got:\n%s", substr, s)
	}
}

func assertNotContains(t *testing.T, s, substr string) {
	t.Helper()
	if strings.Contains(s, substr) {
		t.Fatalf("expected body NOT to contain %q, got:\n%s", substr, s)
	}
}
