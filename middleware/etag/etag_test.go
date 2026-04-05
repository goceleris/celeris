package etag

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"strings"
	"sync"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

func bodyHandler(body string) celeris.HandlerFunc {
	return func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", []byte(body))
	}
}

func notFoundHandler(c *celeris.Context) error {
	return c.Blob(404, "text/plain", []byte("not found"))
}

func emptyHandler(c *celeris.Context) error {
	return c.NoContent(204)
}

func etagHandler(tag string) celeris.HandlerFunc {
	return func(c *celeris.Context) error {
		c.SetHeader("etag", tag)
		return c.Blob(200, "text/plain", []byte("hello"))
	}
}

func expectedWeakTag(body string) string {
	checksum := crc32.ChecksumIEEE([]byte(body))
	return formatCRC32ETag(checksum, true)
}

func expectedStrongTag(body string) string {
	checksum := crc32.ChecksumIEEE([]byte(body))
	return formatCRC32ETag(checksum, false)
}

func TestETag(t *testing.T) {
	tests := []struct {
		name       string
		config     Config
		method     string
		path       string
		headers    [][2]string
		handler    celeris.HandlerFunc
		wantStatus int
		wantETag   string
		wantBody   string
		wantNoETag bool
		wantErr    bool
	}{
		{
			name:       "GET 200 with body adds weak ETag",
			config:     Config{},
			method:     "GET",
			path:       "/",
			handler:    bodyHandler("hello world"),
			wantStatus: 200,
			wantETag:   expectedWeakTag("hello world"),
			wantBody:   "hello world",
		},
		{
			name:       "GET 200 with body strong ETag",
			config:     Config{Strong: true},
			method:     "GET",
			path:       "/",
			handler:    bodyHandler("hello world"),
			wantStatus: 200,
			wantETag:   expectedStrongTag("hello world"),
			wantBody:   "hello world",
		},
		{
			name:       "GET 200 matching If-None-Match returns 304",
			config:     Config{},
			method:     "GET",
			path:       "/",
			headers:    [][2]string{{"if-none-match", expectedWeakTag("hello")}},
			handler:    bodyHandler("hello"),
			wantStatus: 304,
			wantETag:   expectedWeakTag("hello"),
			wantBody:   "",
		},
		{
			name:       "GET 200 non-matching If-None-Match returns 200",
			config:     Config{},
			method:     "GET",
			path:       "/",
			headers:    [][2]string{{"if-none-match", `W/"00000000"`}},
			handler:    bodyHandler("hello"),
			wantStatus: 200,
			wantETag:   expectedWeakTag("hello"),
			wantBody:   "hello",
		},
		{
			name:       "HEAD request with matching If-None-Match returns 304",
			config:     Config{},
			method:     "HEAD",
			path:       "/",
			headers:    [][2]string{{"if-none-match", expectedWeakTag("hello")}},
			handler:    bodyHandler("hello"),
			wantStatus: 304,
			wantETag:   expectedWeakTag("hello"),
			wantBody:   "",
		},
		{
			name:       "POST request skips middleware",
			config:     Config{},
			method:     "POST",
			path:       "/",
			handler:    bodyHandler("created"),
			wantStatus: 200,
			wantNoETag: true,
			wantBody:   "created",
		},
		{
			name:       "PUT request skips middleware",
			config:     Config{},
			method:     "PUT",
			path:       "/",
			handler:    bodyHandler("updated"),
			wantStatus: 200,
			wantNoETag: true,
			wantBody:   "updated",
		},
		{
			name:       "DELETE request skips middleware",
			config:     Config{},
			method:     "DELETE",
			path:       "/",
			handler:    bodyHandler("deleted"),
			wantStatus: 200,
			wantNoETag: true,
			wantBody:   "deleted",
		},
		{
			name:       "OPTIONS request skips middleware",
			config:     Config{},
			method:     "OPTIONS",
			path:       "/",
			handler:    bodyHandler("options"),
			wantStatus: 200,
			wantNoETag: true,
			wantBody:   "options",
		},
		{
			name:       "non-2xx response flushes without ETag",
			config:     Config{},
			method:     "GET",
			path:       "/",
			handler:    notFoundHandler,
			wantStatus: 404,
			wantNoETag: true,
			wantBody:   "not found",
		},
		{
			name:       "empty body response flushes without ETag",
			config:     Config{},
			method:     "GET",
			path:       "/",
			handler:    emptyHandler,
			wantStatus: 204,
			wantNoETag: true,
			wantBody:   "",
		},
		{
			name:       "handler-set ETag is preserved",
			config:     Config{},
			method:     "GET",
			path:       "/",
			handler:    etagHandler(`"custom-tag"`),
			wantStatus: 200,
			wantETag:   `"custom-tag"`,
			wantBody:   "hello",
		},
		{
			name:       "If-None-Match wildcard returns 304",
			config:     Config{},
			method:     "GET",
			path:       "/",
			headers:    [][2]string{{"if-none-match", "*"}},
			handler:    bodyHandler("hello"),
			wantStatus: 304,
			wantETag:   expectedWeakTag("hello"),
			wantBody:   "",
		},
		{
			name:       "weak comparison W/abc matches abc",
			config:     Config{},
			method:     "GET",
			path:       "/",
			headers:    [][2]string{{"if-none-match", `"` + opaqueTag(expectedWeakTag("data")) + `"`}},
			handler:    bodyHandler("data"),
			wantStatus: 304,
			wantETag:   expectedWeakTag("data"),
			wantBody:   "",
		},
		{
			name:       "comma-separated If-None-Match list",
			config:     Config{},
			method:     "GET",
			path:       "/",
			headers:    [][2]string{{"if-none-match", `"aaa", ` + expectedWeakTag("hello") + `, "bbb"`}},
			handler:    bodyHandler("hello"),
			wantStatus: 304,
			wantETag:   expectedWeakTag("hello"),
			wantBody:   "",
		},
		{
			name:       "SkipPaths bypasses processing",
			config:     Config{SkipPaths: []string{"/health"}},
			method:     "GET",
			path:       "/health",
			handler:    bodyHandler("ok"),
			wantStatus: 200,
			wantNoETag: true,
			wantBody:   "ok",
		},
		{
			name: "Skip function bypasses processing",
			config: Config{
				Skip: func(c *celeris.Context) bool {
					return c.Path() == "/skip"
				},
			},
			method:     "GET",
			path:       "/skip",
			handler:    bodyHandler("skipped"),
			wantStatus: 200,
			wantNoETag: true,
			wantBody:   "skipped",
		},
		{
			name:    "handler error propagated alongside 304",
			config:  Config{},
			method:  "GET",
			path:    "/",
			headers: [][2]string{{"if-none-match", expectedWeakTag("hello")}},
			handler: func(c *celeris.Context) error {
				_ = c.Blob(200, "text/plain", []byte("hello"))
				return errors.New("etag: downstream error")
			},
			wantStatus: 304,
			wantETag:   expectedWeakTag("hello"),
			wantBody:   "",
			wantErr:    true,
		},
		{
			name:       "multiple ETags in If-None-Match one matches",
			config:     Config{},
			method:     "GET",
			path:       "/",
			headers:    [][2]string{{"if-none-match", `W/"00000001", W/"00000002", ` + expectedWeakTag("test")}},
			handler:    bodyHandler("test"),
			wantStatus: 304,
			wantETag:   expectedWeakTag("test"),
			wantBody:   "",
		},
		{
			name:       "If-None-Match with extra whitespace",
			config:     Config{},
			method:     "GET",
			path:       "/",
			headers:    [][2]string{{"if-none-match", `  ` + expectedWeakTag("hello") + `  `}},
			handler:    bodyHandler("hello"),
			wantStatus: 304,
			wantETag:   expectedWeakTag("hello"),
			wantBody:   "",
		},
		{
			name:       "strong ETag config with matching If-None-Match",
			config:     Config{Strong: true},
			method:     "GET",
			path:       "/",
			headers:    [][2]string{{"if-none-match", expectedStrongTag("hello")}},
			handler:    bodyHandler("hello"),
			wantStatus: 304,
			wantETag:   expectedStrongTag("hello"),
			wantBody:   "",
		},
		{
			name:       "handler-set weak ETag with matching If-None-Match",
			config:     Config{},
			method:     "GET",
			path:       "/",
			headers:    [][2]string{{"if-none-match", `W/"custom"`}},
			handler:    etagHandler(`W/"custom"`),
			wantStatus: 304,
			wantETag:   `W/"custom"`,
			wantBody:   "",
		},
		{
			name:       "New(Config{}) zero-value produces weak ETag",
			config:     Config{},
			method:     "GET",
			path:       "/",
			handler:    bodyHandler("hello"),
			wantStatus: 200,
			wantETag:   expectedWeakTag("hello"),
			wantBody:   "hello",
		},
		{
			name:       "large body 1KB+",
			config:     Config{},
			method:     "GET",
			path:       "/",
			handler:    bodyHandler(strings.Repeat("x", 2048)),
			wantStatus: 200,
			wantETag:   expectedWeakTag(strings.Repeat("x", 2048)),
			wantBody:   strings.Repeat("x", 2048),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := New(tt.config)
			chain := []celeris.HandlerFunc{mw, tt.handler}

			var opts []celeristest.Option
			for _, h := range tt.headers {
				opts = append(opts, celeristest.WithHeader(h[0], h[1]))
			}

			rec, err := testutil.RunChain(t, chain, tt.method, tt.path, opts...)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}

			testutil.AssertStatus(t, rec, tt.wantStatus)

			if tt.wantNoETag {
				testutil.AssertNoHeader(t, rec, "etag")
			} else if tt.wantETag != "" {
				gotETag := rec.Header("etag")
				if gotETag != tt.wantETag {
					t.Fatalf("etag: got %q, want %q", gotETag, tt.wantETag)
				}
			}

			gotBody := rec.BodyString()
			if gotBody != tt.wantBody {
				t.Fatalf("body: got %q, want %q", gotBody, tt.wantBody)
			}
		})
	}
}

func TestNewDefaultIsWeak(t *testing.T) {
	mw := New()
	chain := []celeris.HandlerFunc{mw, bodyHandler("hello")}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	tag := rec.Header("etag")
	if !strings.HasPrefix(tag, `W/"`) {
		t.Fatalf("New() should produce weak ETag, got %q", tag)
	}
}

func TestHandlerSetETagCheckedAgainstINM(t *testing.T) {
	mw := New()
	handler := etagHandler(`"my-custom-etag"`)
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithHeader("if-none-match", `"my-custom-etag"`))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 304)
}

func TestOpaqueTag(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`W/"abc"`, "abc"},
		{`"abc"`, "abc"},
		{`W/"abc`, `"abc`},
		{`abc`, "abc"},
		{`""`, ""},
	}
	for _, tt := range tests {
		got := opaqueTag(tt.input)
		if got != tt.want {
			t.Errorf("opaqueTag(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestETagMatch(t *testing.T) {
	tests := []struct {
		inm  string
		tag  string
		want bool
	}{
		{`*`, `W/"abc"`, true},
		{`W/"abc"`, `W/"abc"`, true},
		{`"abc"`, `W/"abc"`, true},
		{`W/"abc"`, `"abc"`, true},
		{`W/"abc"`, `W/"def"`, false},
		{`W/"abc", W/"def"`, `W/"def"`, true},
		{`  W/"abc"  `, `W/"abc"`, true},
		{`W/"abc", "def", W/"ghi"`, `"ghi"`, true},
		{``, `W/"abc"`, false},
		// Malformed inputs
		{`"abc`, `"abc"`, false},
		{`W/"abc`, `W/"abc"`, false},
		{`W/abc`, `W/"abc"`, false},
		{`   `, `W/"abc"`, false},
		{`abc`, `"abc"`, true},
	}
	for _, tt := range tests {
		got := etagMatch(tt.inm, tt.tag)
		if got != tt.want {
			t.Errorf("etagMatch(%q, %q) = %v, want %v", tt.inm, tt.tag, got, tt.want)
		}
	}
}

func TestFormatCRC32ETag(t *testing.T) {
	checksum := uint32(0xAABBCCDD)
	weak := formatCRC32ETag(checksum, true)
	if weak != `W/"aabbccdd"` {
		t.Fatalf("weak: got %q, want %q", weak, `W/"aabbccdd"`)
	}
	strong := formatCRC32ETag(checksum, false)
	if strong != `"aabbccdd"` {
		t.Fatalf("strong: got %q, want %q", strong, `"aabbccdd"`)
	}
}

func TestFormatCRC32ETagZero(t *testing.T) {
	weak := formatCRC32ETag(0, true)
	if weak != `W/"00000000"` {
		t.Fatalf("weak zero: got %q, want %q", weak, `W/"00000000"`)
	}
}

func TestHEADRequestAddsETag(t *testing.T) {
	mw := New()
	chain := []celeris.HandlerFunc{mw, bodyHandler("hello")}
	rec, err := testutil.RunChain(t, chain, "HEAD", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	tag := rec.Header("etag")
	if tag == "" {
		t.Fatal("expected etag header on HEAD request")
	}
	if !strings.HasPrefix(tag, `W/"`) {
		t.Fatalf("expected weak etag, got %q", tag)
	}
}

func TestHashFunc(t *testing.T) {
	sha := func(body []byte) string {
		h := sha256.Sum256(body)
		return hex.EncodeToString(h[:])
	}

	t.Run("weak with custom hash", func(t *testing.T) {
		mw := New(Config{HashFunc: sha})
		chain := []celeris.HandlerFunc{mw, bodyHandler("hello")}
		rec, err := testutil.RunChain(t, chain, "GET", "/")
		testutil.AssertNoError(t, err)
		testutil.AssertStatus(t, rec, 200)

		expected := sha256.Sum256([]byte("hello"))
		wantTag := `W/"` + hex.EncodeToString(expected[:]) + `"`
		gotTag := rec.Header("etag")
		if gotTag != wantTag {
			t.Fatalf("etag: got %q, want %q", gotTag, wantTag)
		}
	})

	t.Run("strong with custom hash", func(t *testing.T) {
		mw := New(Config{Strong: true, HashFunc: sha})
		chain := []celeris.HandlerFunc{mw, bodyHandler("hello")}
		rec, err := testutil.RunChain(t, chain, "GET", "/")
		testutil.AssertNoError(t, err)
		testutil.AssertStatus(t, rec, 200)

		expected := sha256.Sum256([]byte("hello"))
		wantTag := `"` + hex.EncodeToString(expected[:]) + `"`
		gotTag := rec.Header("etag")
		if gotTag != wantTag {
			t.Fatalf("etag: got %q, want %q", gotTag, wantTag)
		}
	})

	t.Run("custom hash 304", func(t *testing.T) {
		mw := New(Config{HashFunc: sha})
		chain := []celeris.HandlerFunc{mw, bodyHandler("hello")}

		expected := sha256.Sum256([]byte("hello"))
		inmTag := `W/"` + hex.EncodeToString(expected[:]) + `"`

		rec, err := testutil.RunChain(t, chain, "GET", "/",
			celeristest.WithHeader("if-none-match", inmTag))
		testutil.AssertNoError(t, err)
		testutil.AssertStatus(t, rec, 304)
	})

	t.Run("handler-set ETag takes precedence over HashFunc", func(t *testing.T) {
		mw := New(Config{HashFunc: sha})
		handler := etagHandler(`"handler-tag"`)
		chain := []celeris.HandlerFunc{mw, handler}
		rec, err := testutil.RunChain(t, chain, "GET", "/")
		testutil.AssertNoError(t, err)
		testutil.AssertStatus(t, rec, 200)

		gotTag := rec.Header("etag")
		if gotTag != `"handler-tag"` {
			t.Fatalf("etag: got %q, want %q", gotTag, `"handler-tag"`)
		}
	})
}

func TestZeroValueConfigProducesWeakETag(t *testing.T) {
	mw := New(Config{})
	chain := []celeris.HandlerFunc{mw, bodyHandler("test")}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	tag := rec.Header("etag")
	if !strings.HasPrefix(tag, `W/"`) {
		t.Fatalf("Config{} should produce weak ETag, got %q", tag)
	}
}

func TestZeroValueConfigWithHashFuncProducesWeakETag(t *testing.T) {
	sha := func(body []byte) string {
		h := sha256.Sum256(body)
		return hex.EncodeToString(h[:])
	}
	mw := New(Config{HashFunc: sha})
	chain := []celeris.HandlerFunc{mw, bodyHandler("test")}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	tag := rec.Header("etag")
	if !strings.HasPrefix(tag, `W/"`) {
		t.Fatalf("Config{HashFunc: sha} should produce weak ETag, got %q", tag)
	}
}

func TestConcurrentAccess(t *testing.T) {
	mw := New()
	handler := bodyHandler("concurrent-body")

	var wg sync.WaitGroup
	errs := make(chan error, 100)

	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			chain := []celeris.HandlerFunc{mw, handler}
			rec, err := testutil.RunChain(t, chain, "GET", "/")
			if err != nil {
				errs <- err
				return
			}
			if rec.StatusCode != 200 {
				errs <- errors.New("unexpected status")
				return
			}
			tag := rec.Header("etag")
			if tag == "" {
				errs <- errors.New("missing etag header")
				return
			}
			if !strings.HasPrefix(tag, `W/"`) {
				errs <- errors.New("expected weak etag")
				return
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent access error: %v", err)
	}
}
