package compress

import (
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// varyAcceptEncodingCount counts how many times the Vary lines of headers
// name Accept-Encoding.
func varyAcceptEncodingCount(headers [][2]string) int {
	n := 0
	for _, h := range headers {
		if !strings.EqualFold(h[0], "vary") {
			continue
		}
		for tok := range strings.SplitSeq(h[1], ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "Accept-Encoding") {
				n++
			}
		}
	}
	return n
}

// TestVaryNamesAcceptEncodingOnce912: since celeris#912,
// Context.AcceptsEncodings adds Accept-Encoding to the response's Vary header
// itself. compress calls it on every GET, so compress must not add a second
// Vary: Accept-Encoding line of its own, on any path: the compressed body,
// no matching encoding, a body below MinLength, HEAD, a handler that called
// AcceptsEncodings too or replaced the Vary header with one that names it.
// A handler that replaces the Vary header (SetHeader) with one that does not
// still gets Accept-Encoding added back, next to its own.
func TestVaryNamesAcceptEncodingOnce912(t *testing.T) {
	body := testBody()
	blob := func(c *celeris.Context) error { return c.Blob(200, "application/json", body) }
	cases := []struct {
		name       string
		method     string
		ae         string
		cfg        Config
		h          celeris.HandlerFunc
		wantCE     string
		wantOrigin bool
	}{
		{"gzip/compressed", "GET", "gzip", Config{}, blob, "gzip", false},
		{"identity", "GET", "identity", Config{}, blob, "", false},
		{"no-accept-encoding", "GET", "", Config{}, blob, "", false},
		{"no-matching-encoding", "GET", "gzip", Config{Encodings: []string{"zstd"}}, blob, "", false},
		{"below-minlength", "GET", "gzip", Config{}, func(c *celeris.Context) error {
			return c.Blob(200, "application/json", []byte("short"))
		}, "", false},
		{"head", "HEAD", "gzip", Config{}, blob, "", false},
		{"handler-replaces-vary-naming-it", "GET", "gzip", Config{}, func(c *celeris.Context) error {
			c.SetHeader("vary", "Origin, accept-encoding")
			return blob(c)
		}, "gzip", true},
		{"handler-negotiates-too", "GET", "gzip", Config{}, func(c *celeris.Context) error {
			_ = c.AcceptsEncodings("gzip")
			return blob(c)
		}, "gzip", false},
		{"handler-replaces-vary", "GET", "gzip", Config{}, func(c *celeris.Context) error {
			c.SetHeader("vary", "Origin")
			return blob(c)
		}, "gzip", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := []celeristest.Option{celeristest.WithHandlers(New(tc.cfg), tc.h)}
			if tc.ae != "" {
				opts = append(opts, celeristest.WithHeader("accept-encoding", tc.ae))
			}
			ctx, rec := celeristest.NewContext(tc.method, "/data", opts...)
			defer celeristest.ReleaseContext(ctx)
			if err := ctx.Next(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var vary []string
			origin := false
			for _, h := range rec.Headers {
				if h[0] == "vary" {
					vary = append(vary, h[1])
					origin = origin || strings.HasPrefix(h[1], "Origin")
				}
			}
			if got := rec.Header("content-encoding"); got != tc.wantCE {
				t.Errorf("content-encoding %q, want %q", got, tc.wantCE)
			}
			if n := varyAcceptEncodingCount(rec.Headers); n != 1 {
				t.Errorf("Vary names Accept-Encoding %d times, want once: vary lines %q", n, vary)
			}
			if origin != tc.wantOrigin {
				t.Errorf("Vary: Origin present=%v, want %v: vary lines %q", origin, tc.wantOrigin, vary)
			}
		})
	}
}

// TestCompressedStreamVaryOnce912: a CompressedStream adds Vary:
// Accept-Encoding to the headers it sends, unless the caller's headers name it
// already: they can be the Context's response headers, where
// AcceptsEncodings has put it (celeris#912).
func TestCompressedStreamVaryOnce912(t *testing.T) {
	cases := []struct {
		name    string
		headers [][2]string
		want    int // Vary lines that name Accept-Encoding
	}{
		{"none", nil, 1},
		{"other-vary", [][2]string{{"vary", "Origin"}}, 1},
		{"named", [][2]string{{"vary", "Accept-Encoding"}, {"content-length", "5"}}, 1},
		{"named-in-a-list", [][2]string{{"Vary", "origin, accept-encoding"}}, 1},
		{"star", [][2]string{{"vary", "*"}}, 0}, // Vary: * already covers it
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := streamHeaders("gzip", tc.headers)
			if n := varyAcceptEncodingCount(out); n != tc.want {
				t.Errorf("Vary names Accept-Encoding %d times, want %d: %q", n, tc.want, out)
			}
			for _, h := range out {
				if h[0] == "content-length" {
					t.Errorf("Content-Length kept: %q", out)
				}
			}
			if out[0] != [2]string{"content-encoding", "gzip"} {
				t.Errorf("first header %q, want content-encoding gzip", out[0])
			}
		})
	}
}

// TestVaryStarIsLeftAlone912: a handler that answers Vary: * (the response
// varies on more than request headers) gets no Accept-Encoding line next to
// it: "*" already covers every header.
func TestVaryStarIsLeftAlone912(t *testing.T) {
	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip"),
		celeristest.WithHandlers(New(), func(c *celeris.Context) error {
			c.SetHeader("vary", "*")
			return c.Blob(200, "application/json", testBody())
		}),
	)
	defer celeristest.ReleaseContext(ctx)
	if err := ctx.Next(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var vary []string
	for _, h := range rec.Headers {
		if h[0] == "vary" {
			vary = append(vary, h[1])
		}
	}
	if len(vary) != 1 || vary[0] != "*" {
		t.Errorf("vary lines %q, want only \"*\"", vary)
	}
}
