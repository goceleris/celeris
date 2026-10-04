package static

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// TestPreCompressedVaryOnce912: a pre-compressed variant is served with Vary
// naming Accept-Encoding once, also when a middleware before static (compress,
// or anything that negotiates with Context.AcceptsEncodings, which names
// Accept-Encoding in Vary itself since celeris#912) has named it already.
func TestPreCompressedVaryOnce912(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log('hello')"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.js.gz"), []byte("gzip-compressed-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	mod := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	fsys := fstest.MapFS{
		"app.js":    {Data: []byte("console.log('hello')"), ModTime: mod},
		"app.js.gz": {Data: []byte("gzip-compressed-data"), ModTime: mod},
	}
	negotiate := func(c *celeris.Context) error {
		_ = c.AcceptsEncodings("br", "gzip")
		return c.Next()
	}
	for _, src := range []struct {
		name string
		cfg  Config
	}{
		{"os", Config{Root: dir, Compress: true}},
		{"fs", Config{FS: fsys, Compress: true}},
	} {
		for _, pre := range []struct {
			name string
			mw   []celeris.HandlerFunc
		}{
			{"alone", nil},
			{"after-AcceptsEncodings", []celeris.HandlerFunc{negotiate}},
		} {
			t.Run(src.name+"/"+pre.name, func(t *testing.T) {
				chain := append(append([]celeris.HandlerFunc{}, pre.mw...), New(src.cfg))
				ctx, rec := celeristest.NewContext("GET", "/app.js",
					celeristest.WithHeader("accept-encoding", "gzip"),
					celeristest.WithHandlers(chain...),
				)
				defer celeristest.ReleaseContext(ctx)
				if err := ctx.Next(); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if ce := rec.Header("content-encoding"); ce != "gzip" {
					t.Fatalf("content-encoding %q, want gzip (the pre-compressed variant)", ce)
				}
				n := 0
				var vary []string
				for _, h := range rec.Headers {
					if h[0] != "vary" {
						continue
					}
					vary = append(vary, h[1])
					for tok := range strings.SplitSeq(h[1], ",") {
						if strings.EqualFold(strings.TrimSpace(tok), "Accept-Encoding") {
							n++
						}
					}
				}
				if n != 1 {
					t.Errorf("Vary names Accept-Encoding %d times, want once: vary lines %q", n, vary)
				}
			})
		}
	}
}
