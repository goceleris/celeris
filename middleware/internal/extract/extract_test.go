package extract_test

import (
	"testing"

	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/extract"
)

func TestHeaderWithPrefix(t *testing.T) {
	fn := extract.Parse("header:Authorization:Bearer ")
	ctx, _ := celeristest.NewContext("GET", "/",
		celeristest.WithHeader("authorization", "Bearer tok123"),
	)
	defer celeristest.ReleaseContext(ctx)

	got := fn(ctx)
	if got != "tok123" {
		t.Fatalf("expected tok123, got %q", got)
	}
}

func TestHeaderWithoutPrefix(t *testing.T) {
	fn := extract.Parse("header:X-API-Key")
	ctx, _ := celeristest.NewContext("GET", "/",
		celeristest.WithHeader("x-api-key", "secret"),
	)
	defer celeristest.ReleaseContext(ctx)

	got := fn(ctx)
	if got != "secret" {
		t.Fatalf("expected secret, got %q", got)
	}
}

func TestQueryExtraction(t *testing.T) {
	fn := extract.Parse("query:token")
	ctx, _ := celeristest.NewContext("GET", "/",
		celeristest.WithQuery("token", "abc"),
	)
	defer celeristest.ReleaseContext(ctx)

	got := fn(ctx)
	if got != "abc" {
		t.Fatalf("expected abc, got %q", got)
	}
}

func TestCookieExtraction(t *testing.T) {
	fn := extract.Parse("cookie:session")
	ctx, _ := celeristest.NewContext("GET", "/",
		celeristest.WithCookie("session", "xyz"),
	)
	defer celeristest.ReleaseContext(ctx)

	got := fn(ctx)
	if got != "xyz" {
		t.Fatalf("expected xyz, got %q", got)
	}
}

func TestMultiSource(t *testing.T) {
	fn := extract.Parse("header:X-Token, query:token")

	// Header present: should use header.
	ctx, _ := celeristest.NewContext("GET", "/",
		celeristest.WithHeader("x-token", "from-header"),
		celeristest.WithQuery("token", "from-query"),
	)
	defer celeristest.ReleaseContext(ctx)

	got := fn(ctx)
	if got != "from-header" {
		t.Fatalf("expected from-header, got %q", got)
	}

	// Header missing: should fall back to query.
	ctx2, _ := celeristest.NewContext("GET", "/",
		celeristest.WithQuery("token", "from-query"),
	)
	defer celeristest.ReleaseContext(ctx2)

	got = fn(ctx2)
	if got != "from-query" {
		t.Fatalf("expected from-query, got %q", got)
	}
}

func TestUnknownSourcePanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for unknown source")
		}
		s, ok := r.(string)
		if !ok || s != "extract: unsupported source: body" {
			t.Fatalf("unexpected panic value: %v", r)
		}
	}()
	extract.Parse("body:data")
}

func TestMissingNamePanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for missing name")
		}
		s, ok := r.(string)
		if !ok || s != "extract: invalid lookup format: header" {
			t.Fatalf("unexpected panic value: %v", r)
		}
	}()
	extract.Parse("header")
}

func TestEmptyResult(t *testing.T) {
	fn := extract.Parse("header:X-Missing")
	ctx, _ := celeristest.NewContext("GET", "/")
	defer celeristest.ReleaseContext(ctx)

	got := fn(ctx)
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestHeaderPrefixNoMatch(t *testing.T) {
	fn := extract.Parse("header:Authorization:Bearer ")
	ctx, _ := celeristest.NewContext("GET", "/",
		celeristest.WithHeader("authorization", "Basic dXNlcjpwYXNz"),
	)
	defer celeristest.ReleaseContext(ctx)

	got := fn(ctx)
	if got != "" {
		t.Fatalf("expected empty when prefix doesn't match, got %q", got)
	}
}

func TestHeaderPrefixCaseInsensitive(t *testing.T) {
	fn := extract.Parse("header:Authorization:Bearer ")
	ctx, _ := celeristest.NewContext("GET", "/",
		celeristest.WithHeader("authorization", "bearer tok123"),
	)
	defer celeristest.ReleaseContext(ctx)

	got := fn(ctx)
	if got != "tok123" {
		t.Fatalf("expected tok123, got %q", got)
	}
}
