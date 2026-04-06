package rewrite

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/internal/testutil"
)

func pathRecorder(c *celeris.Context) error {
	return c.Blob(200, "text/plain", []byte(c.Path()))
}

func TestRewriteSimple(t *testing.T) {
	mw := New(Config{
		Rules: map[string]string{
			"^/old$": "/new",
		},
	})
	chain := []celeris.HandlerFunc{mw, pathRecorder}
	rec, err := testutil.RunChain(t, chain, "GET", "/old")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "/new" {
		t.Fatalf("path: got %q, want %q", rec.BodyString(), "/new")
	}
}

func TestRewriteCaptureGroups(t *testing.T) {
	mw := New(Config{
		Rules: map[string]string{
			`^/users/(\d+)/posts$`: "/api/v2/users/$1/posts",
		},
	})
	chain := []celeris.HandlerFunc{mw, pathRecorder}
	rec, err := testutil.RunChain(t, chain, "GET", "/users/42/posts")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "/api/v2/users/42/posts" {
		t.Fatalf("path: got %q, want %q", rec.BodyString(), "/api/v2/users/42/posts")
	}
}

func TestRewriteFirstMatchWins(t *testing.T) {
	mw := New(Config{
		Rules: map[string]string{
			"^/a/.*": "/alpha",
			"^/b/.*": "/beta",
		},
	})

	t.Run("matches first rule", func(t *testing.T) {
		chain := []celeris.HandlerFunc{mw, pathRecorder}
		rec, err := testutil.RunChain(t, chain, "GET", "/a/something")
		testutil.AssertNoError(t, err)
		testutil.AssertStatus(t, rec, 200)
		if rec.BodyString() != "/alpha" {
			t.Fatalf("path: got %q, want %q", rec.BodyString(), "/alpha")
		}
	})

	t.Run("matches second rule", func(t *testing.T) {
		chain := []celeris.HandlerFunc{mw, pathRecorder}
		rec, err := testutil.RunChain(t, chain, "GET", "/b/something")
		testutil.AssertNoError(t, err)
		testutil.AssertStatus(t, rec, 200)
		if rec.BodyString() != "/beta" {
			t.Fatalf("path: got %q, want %q", rec.BodyString(), "/beta")
		}
	})
}

func TestRewriteRedirectMode(t *testing.T) {
	mw := New(Config{
		Rules: map[string]string{
			"^/old$": "/new",
		},
		RedirectCode: 301,
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/old",
		celeristest.WithScheme("https"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 301)
	testutil.AssertHeader(t, rec, "location", "https://localhost/new")
}

func TestRewriteNoMatch(t *testing.T) {
	mw := New(Config{
		Rules: map[string]string{
			"^/old$": "/new",
		},
	})
	chain := []celeris.HandlerFunc{mw, pathRecorder}
	rec, err := testutil.RunChain(t, chain, "GET", "/other")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "/other" {
		t.Fatalf("path: got %q, want %q", rec.BodyString(), "/other")
	}
}

func TestRewritePreserveQuery(t *testing.T) {
	mw := New(Config{
		Rules: map[string]string{
			"^/old$": "/new",
		},
		RedirectCode: 302,
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/old",
		celeristest.WithScheme("https"),
		celeristest.WithQuery("key", "value"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 302)
	testutil.AssertHeader(t, rec, "location", "https://localhost/new?key=value")
}

func TestRewriteSkipPaths(t *testing.T) {
	mw := New(Config{
		Rules: map[string]string{
			"^/old$": "/new",
		},
		SkipPaths: []string{"/old"},
	})
	chain := []celeris.HandlerFunc{mw, pathRecorder}
	rec, err := testutil.RunChain(t, chain, "GET", "/old")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if rec.BodyString() != "/old" {
		t.Fatalf("path: got %q, want %q (should be unchanged due to skip)", rec.BodyString(), "/old")
	}
}

func TestRewriteSkipFunc(t *testing.T) {
	mw := New(Config{
		Rules: map[string]string{
			"^/old$": "/new",
		},
		Skip: func(c *celeris.Context) bool {
			return c.Method() == "POST"
		},
	})

	t.Run("GET is rewritten", func(t *testing.T) {
		chain := []celeris.HandlerFunc{mw, pathRecorder}
		rec, err := testutil.RunChain(t, chain, "GET", "/old")
		testutil.AssertNoError(t, err)
		if rec.BodyString() != "/new" {
			t.Fatalf("path: got %q, want %q", rec.BodyString(), "/new")
		}
	})

	t.Run("POST is skipped", func(t *testing.T) {
		chain := []celeris.HandlerFunc{mw, pathRecorder}
		rec, err := testutil.RunChain(t, chain, "POST", "/old")
		testutil.AssertNoError(t, err)
		if rec.BodyString() != "/old" {
			t.Fatalf("path: got %q, want %q (should be unchanged due to skip)", rec.BodyString(), "/old")
		}
	})
}

func TestValidatePanics(t *testing.T) {
	t.Run("empty rules", func(t *testing.T) {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected panic for empty Rules")
			}
			msg, ok := r.(string)
			if !ok || msg != "rewrite: Rules must not be empty" {
				t.Fatalf("unexpected panic: %v", r)
			}
		}()
		New(Config{Rules: map[string]string{}})
	})

	t.Run("nil rules", func(t *testing.T) {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected panic for nil Rules")
			}
		}()
		New(Config{})
	})

	t.Run("invalid redirect code", func(t *testing.T) {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected panic for invalid RedirectCode")
			}
			msg, ok := r.(string)
			if !ok {
				t.Fatalf("unexpected panic type: %T", r)
			}
			if msg == "" {
				t.Fatal("expected non-empty panic message")
			}
		}()
		New(Config{
			Rules:        map[string]string{"^/old$": "/new"},
			RedirectCode: 200,
		})
	})

	t.Run("invalid regex panics", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic for invalid regex")
			}
		}()
		New(Config{
			Rules: map[string]string{"[invalid": "/new"},
		})
	})
}
