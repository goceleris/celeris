package static

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/internal/testutil"
)

func tmpDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func noop(_ *celeris.Context) error { return nil }

func TestStaticServeFile(t *testing.T) {
	dir := tmpDir(t, map[string]string{
		"hello.txt": "hello world",
	})
	mw := New(Config{Root: dir})
	chain := []celeris.HandlerFunc{mw, noop}
	rec, err := testutil.RunChain(t, chain, "GET", "/hello.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "hello world")
}

func TestStaticServeIndex(t *testing.T) {
	dir := tmpDir(t, map[string]string{
		"sub/index.html": "<h1>index</h1>",
	})
	mw := New(Config{Root: dir})
	chain := []celeris.HandlerFunc{mw, noop}
	rec, err := testutil.RunChain(t, chain, "GET", "/sub/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "<h1>index</h1>")
}

func TestStaticSPAFallback(t *testing.T) {
	dir := tmpDir(t, map[string]string{
		"index.html": "<h1>spa</h1>",
	})
	mw := New(Config{Root: dir, SPA: true})
	chain := []celeris.HandlerFunc{mw, noop}
	rec, err := testutil.RunChain(t, chain, "GET", "/nonexistent/page")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "<h1>spa</h1>")
}

func TestStaticSPAExistingFile(t *testing.T) {
	dir := tmpDir(t, map[string]string{
		"index.html": "<h1>spa</h1>",
		"app.js":     "console.log('ok')",
	})
	mw := New(Config{Root: dir, SPA: true})
	chain := []celeris.HandlerFunc{mw, noop}
	rec, err := testutil.RunChain(t, chain, "GET", "/app.js")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "console.log")
}

func TestStaticFromFS(t *testing.T) {
	mapFS := fstest.MapFS{
		"style.css":  {Data: []byte("body{}")},
		"index.html": {Data: []byte("<html></html>")},
	}
	mw := New(Config{Filesystem: mapFS})
	chain := []celeris.HandlerFunc{mw, noop}

	rec, err := testutil.RunChain(t, chain, "GET", "/style.css")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "body{}")
}

func TestStaticCacheControl(t *testing.T) {
	dir := tmpDir(t, map[string]string{
		"file.txt": "cached",
	})
	mw := New(Config{Root: dir, MaxAge: 3600})
	chain := []celeris.HandlerFunc{mw, noop}
	rec, err := testutil.RunChain(t, chain, "GET", "/file.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertHeader(t, rec, "cache-control", "public, max-age=3600")
}

func TestStaticPathTraversal(t *testing.T) {
	dir := tmpDir(t, map[string]string{
		"safe.txt": "safe",
	})
	mw := New(Config{Root: dir})
	chain := []celeris.HandlerFunc{mw, noop}

	// The sanitize function rejects ".." so this falls through to noop.
	rec, err := testutil.RunChain(t, chain, "GET", "/../etc/passwd")
	testutil.AssertNoError(t, err)
	// Falls through to next handler which writes nothing (noop).
	testutil.AssertBodyEmpty(t, rec)
}

func TestStaticPassthroughPOST(t *testing.T) {
	dir := tmpDir(t, map[string]string{
		"file.txt": "content",
	})
	mw := New(Config{Root: dir})
	handler := func(c *celeris.Context) error {
		return c.String(200, "post-handler")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "POST", "/file.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "post-handler")
}

func TestStaticNotFound(t *testing.T) {
	dir := tmpDir(t, map[string]string{
		"exists.txt": "yes",
	})
	mw := New(Config{Root: dir})
	handler := func(c *celeris.Context) error {
		return c.String(404, "not found")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/missing.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 404)
	testutil.AssertBodyContains(t, rec, "not found")
}

func TestStaticPrefix(t *testing.T) {
	dir := tmpDir(t, map[string]string{
		"app.js": "var x=1",
	})
	mw := New(Config{Root: dir, Prefix: "/static"})
	chain := []celeris.HandlerFunc{mw, noop}
	rec, err := testutil.RunChain(t, chain, "GET", "/static/app.js")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "var x=1")
}

func TestStaticBrowse(t *testing.T) {
	dir := tmpDir(t, map[string]string{
		"docs/a.txt": "aaa",
		"docs/b.txt": "bbb",
	})
	mw := New(Config{Root: dir, Browse: true})
	chain := []celeris.HandlerFunc{mw, noop}
	rec, err := testutil.RunChain(t, chain, "GET", "/docs/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	body := rec.BodyString()
	if !strings.Contains(body, "a.txt") || !strings.Contains(body, "b.txt") {
		t.Fatalf("expected directory listing with a.txt and b.txt, got: %s", body)
	}
}

func TestStaticSkipPaths(t *testing.T) {
	dir := tmpDir(t, map[string]string{
		"health": "ok",
	})
	mw := New(Config{Root: dir, SkipPaths: []string{"/health"}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "dynamic-health")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/health")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "dynamic-health")
}

func TestValidatePanicNoRoot(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when neither Root nor Filesystem is set")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "Root") {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	New(Config{})
}

func TestStaticPrefixMismatch(t *testing.T) {
	dir := tmpDir(t, map[string]string{
		"file.txt": "content",
	})
	mw := New(Config{Root: dir, Prefix: "/assets"})
	handler := func(c *celeris.Context) error {
		return c.String(200, "fallback")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/other/file.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "fallback")
}

func TestStaticFSIndex(t *testing.T) {
	mapFS := fstest.MapFS{
		"sub/index.html": {Data: []byte("<h1>fs-index</h1>")},
	}
	mw := New(Config{Filesystem: mapFS})
	chain := []celeris.HandlerFunc{mw, noop}
	rec, err := testutil.RunChain(t, chain, "GET", "/sub/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "<h1>fs-index</h1>")
}

func TestStaticFSSPAFallback(t *testing.T) {
	mapFS := fstest.MapFS{
		"index.html": {Data: []byte("<h1>spa-fs</h1>")},
	}
	mw := New(Config{Filesystem: mapFS, SPA: true})
	chain := []celeris.HandlerFunc{mw, noop}
	rec, err := testutil.RunChain(t, chain, "GET", "/does/not/exist")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "<h1>spa-fs</h1>")
}

func TestStaticFSBrowse(t *testing.T) {
	mapFS := fstest.MapFS{
		"dir/one.txt": {Data: []byte("1")},
		"dir/two.txt": {Data: []byte("2")},
	}
	mw := New(Config{Filesystem: mapFS, Browse: true})
	chain := []celeris.HandlerFunc{mw, noop}
	rec, err := testutil.RunChain(t, chain, "GET", "/dir/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	body := rec.BodyString()
	if !strings.Contains(body, "one.txt") || !strings.Contains(body, "two.txt") {
		t.Fatalf("expected directory listing, got: %s", body)
	}
}

func TestStaticRootIndex(t *testing.T) {
	dir := tmpDir(t, map[string]string{
		"index.html": "<h1>root</h1>",
	})
	mw := New(Config{Root: dir})
	chain := []celeris.HandlerFunc{mw, noop}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "<h1>root</h1>")
}

func TestValidatePanicBadPrefix(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for prefix not starting with /")
		}
	}()
	New(Config{Root: "/tmp", Prefix: "bad"})
}

func TestStaticHeadRequest(t *testing.T) {
	dir := tmpDir(t, map[string]string{
		"file.txt": "head-content",
	})
	mw := New(Config{Root: dir})
	chain := []celeris.HandlerFunc{mw, noop}
	rec, err := testutil.RunChain(t, chain, "HEAD", "/file.txt")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}
