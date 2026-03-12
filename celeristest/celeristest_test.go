package celeristest

import (
	"testing"

	"github.com/goceleris/celeris"
)

func TestNewContext(t *testing.T) {
	ctx, rec := NewContext("GET", "/hello")
	defer ReleaseContext(ctx)

	if ctx.Method() != "GET" {
		t.Fatalf("expected GET, got %s", ctx.Method())
	}
	if ctx.Path() != "/hello" {
		t.Fatalf("expected /hello, got %s", ctx.Path())
	}

	err := ctx.String(200, "ok")
	if err != nil {
		t.Fatal(err)
	}
	if rec.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", rec.StatusCode)
	}
	if rec.BodyString() != "ok" {
		t.Fatalf("expected ok, got %s", rec.BodyString())
	}
}

func TestNewContextWithBody(t *testing.T) {
	ctx, _ := NewContext("POST", "/data",
		WithBody([]byte(`{"name":"test"}`)),
		WithContentType("application/json"),
	)
	defer ReleaseContext(ctx)

	var v struct{ Name string }
	if err := ctx.BindJSON(&v); err != nil {
		t.Fatal(err)
	}
	if v.Name != "test" {
		t.Fatalf("expected test, got %s", v.Name)
	}
}

func TestNewContextWithQuery(t *testing.T) {
	ctx, _ := NewContext("GET", "/search",
		WithQuery("q", "hello"),
		WithQuery("page", "2"),
	)
	defer ReleaseContext(ctx)

	if ctx.Query("q") != "hello" {
		t.Fatalf("expected hello, got %s", ctx.Query("q"))
	}
	if ctx.Query("page") != "2" {
		t.Fatalf("expected 2, got %s", ctx.Query("page"))
	}
}

func TestNewContextWithParam(t *testing.T) {
	ctx, _ := NewContext("GET", "/users/42",
		WithParam("id", "42"),
	)
	defer ReleaseContext(ctx)

	if ctx.Param("id") != "42" {
		t.Fatalf("expected 42, got %s", ctx.Param("id"))
	}
}

func TestNewContextWithHeader(t *testing.T) {
	ctx, _ := NewContext("GET", "/test",
		WithHeader("x-custom", "value"),
	)
	defer ReleaseContext(ctx)

	if ctx.Header("x-custom") != "value" {
		t.Fatalf("expected value, got %s", ctx.Header("x-custom"))
	}
}

func TestHandlerWithRecorder(t *testing.T) {
	handler := func(c *celeris.Context) error {
		name := c.Query("name")
		return c.JSON(200, map[string]string{"hello": name})
	}

	ctx, rec := NewContext("GET", "/greet",
		WithQuery("name", "world"),
	)
	defer ReleaseContext(ctx)

	if err := handler(ctx); err != nil {
		t.Fatal(err)
	}
	if rec.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", rec.StatusCode)
	}
	if ct := rec.Header("content-type"); ct != "application/json" {
		t.Fatalf("expected content-type application/json, got %s", ct)
	}
}

func TestNewContextT(t *testing.T) {
	// NewContextT should automatically clean up — no defer ReleaseContext needed.
	ctx, rec := NewContextT(t, "GET", "/auto-cleanup")

	if ctx.Method() != "GET" {
		t.Fatalf("expected GET, got %s", ctx.Method())
	}
	if ctx.Path() != "/auto-cleanup" {
		t.Fatalf("expected /auto-cleanup, got %s", ctx.Path())
	}

	_ = ctx.String(200, "ok")
	if rec.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", rec.StatusCode)
	}
	// Context will be released by t.Cleanup — no manual defer needed.
}

func TestResponseRecorderHeader(t *testing.T) {
	rec := &ResponseRecorder{
		Headers: [][2]string{
			{"content-type", "text/plain"},
			{"x-request-id", "abc123"},
		},
	}

	if rec.Header("content-type") != "text/plain" {
		t.Fatalf("expected text/plain, got %s", rec.Header("content-type"))
	}
	if rec.Header("x-request-id") != "abc123" {
		t.Fatalf("expected abc123, got %s", rec.Header("x-request-id"))
	}
	if rec.Header("missing") != "" {
		t.Fatalf("expected empty, got %s", rec.Header("missing"))
	}
}

func TestResponseRecorderBodyString(t *testing.T) {
	rec := &ResponseRecorder{Body: []byte("hello world")}
	if rec.BodyString() != "hello world" {
		t.Fatalf("expected hello world, got %s", rec.BodyString())
	}

	empty := &ResponseRecorder{}
	if empty.BodyString() != "" {
		t.Fatalf("expected empty, got %s", empty.BodyString())
	}
}
