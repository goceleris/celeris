package static

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// celeris#888 item 36: static marks a response whose strong tag compress would
// weaken, so that compress gives the 304 the tag the 200 carries. The mark is
// made only when it can matter, and the key is the one compress and etag use
// (compress cannot import it: a separate module).
func TestFSMarksTheRepresentationCompressCouldWeaken888(t *testing.T) {
	const key = "celeris.compressible-representation"
	content := []byte(strings.Repeat("0123456789", 100))
	mod := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	fsys := fstest.MapFS{
		"f.txt":    {Data: content, ModTime: mod},
		"f.txt.gz": {Data: []byte("gz"), ModTime: mod},
		"g.json":   {Data: []byte(`{"a":1}`), ModTime: mod},
		"zero.txt": {Data: content}, // no mtime: no validators at all
	}
	strong := ""
	noCompress := false
	// compress in front: it sets the key static looks for when it has
	// negotiated an encoding (this package cannot import compress).
	compressInFront := func(c *celeris.Context) error {
		c.Set("celeris.compress.active", true)
		return c.Next()
	}
	run := func(method, path string, c bool, hdrs ...string) (marked bool, size int, ct string, status int, tag string) {
		t.Helper()
		chain := []celeris.HandlerFunc{New(Config{FS: fsys, Compress: c})}
		if !noCompress {
			chain = append([]celeris.HandlerFunc{compressInFront}, chain...)
		}
		opts := []celeristest.Option{celeristest.WithHandlers(chain...)}
		for i := 0; i+1 < len(hdrs); i += 2 {
			opts = append(opts, celeristest.WithHeader(hdrs[i], hdrs[i+1]))
		}
		ctx, rec := celeristest.NewContextT(t, method, path, opts...)
		if err := ctx.Next(); err != nil {
			t.Fatalf("Next: %v", err)
		}
		v, ok := ctx.Get(key)
		if ok {
			m, isRep := v.(interface {
				CompressibleRepresentation() (int, string)
			})
			if !isRep {
				t.Fatalf("the value under %q is %T, which does not answer CompressibleRepresentation", key, v)
			}
			size, ct = m.CompressibleRepresentation()
		}
		return ok, size, ct, rec.StatusCode, rec.Header("etag")
	}

	ok, size, ct, status, tag := run("GET", "/f.txt", false, "accept-encoding", "gzip")
	if !ok || size != len(content) || !strings.HasPrefix(ct, "text/plain") || status != 200 || strings.HasPrefix(tag, "W/") {
		t.Fatalf("GET with Accept-Encoding: marked=%v size=%d type=%q status=%d tag=%q, want a mark of %d bytes of text/plain on a 200 with a strong tag", ok, size, ct, status, tag, len(content))
	}
	strong = tag
	if ok, _, _, status, _ := run("GET", "/f.txt", false, "accept-encoding", "gzip", "if-none-match", strong); !ok || status != 304 {
		t.Errorf("a 304: marked=%v status=%d, want a mark on a 304", ok, status)
	}
	// compress not in front (or it negotiated no encoding): nothing weakens the
	// tag, so no mark is made and the request pays nothing for it.
	noCompress = true
	if ok, _, _, status, _ := run("GET", "/f.txt", false, "accept-encoding", "gzip"); ok || status != 200 {
		t.Errorf("without compress in front: marked=%v status=%d, want no mark on a 200", ok, status)
	}
	if ok, _, _, status, _ := run("GET", "/f.txt", false, "accept-encoding", "gzip", "if-none-match", strong); ok || status != 304 {
		t.Errorf("without compress in front, a 304: marked=%v status=%d, want no mark on a 304", ok, status)
	}
	noCompress = false
	for _, tc := range []struct {
		name, method, path string
		compress           bool
		hdrs               []string
		wantMark           bool
		wantStatus         int
	}{
		{"a part of the file", "GET", "/f.txt", false, []string{"accept-encoding", "gzip", "range", "bytes=0-9"}, false, 206},
		{"a part, validated by the tag", "GET", "/f.txt", false, []string{"accept-encoding", "gzip", "range", "bytes=0-9", "if-range", strong}, false, 206},
		{"a range no byte of which exists", "GET", "/f.txt", false, []string{"accept-encoding", "gzip", "range", "bytes=99999-"}, false, 416},
		{"If-Range that does not hold: the whole file", "GET", "/f.txt", false, []string{"accept-encoding", "gzip", "range", "bytes=0-9", "if-range", `"other"`}, true, 200},
		{"a 304 of a ranged request", "GET", "/f.txt", false, []string{"accept-encoding", "gzip", "range", "bytes=0-9", "if-none-match", strong}, false, 304},
		{"a pre-compressed variant (weak tag, Content-Encoding)", "GET", "/f.txt", true, []string{"accept-encoding", "gzip"}, false, 200},
		{"no mtime, no validator", "GET", "/zero.txt", false, []string{"accept-encoding", "gzip"}, false, 200},
		{"another file", "GET", "/g.json", false, []string{"accept-encoding", "br"}, true, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, _, _, status, _ := run(tc.method, tc.path, tc.compress, tc.hdrs...)
			if ok != tc.wantMark || status != tc.wantStatus {
				t.Errorf("marked=%v status=%d, want marked=%v status=%d", ok, status, tc.wantMark, tc.wantStatus)
			}
		})
	}
}

// TestRootMarksNothing888: Root's tag is weak (mtime and size), which compress
// leaves as it is, so the 200 and the 304 agree already and no mark is made.
func TestRootMarksNothing888(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(strings.Repeat("a", 1000)), 0o644); err != nil {
		t.Fatal(err)
	}
	compressInFront := func(c *celeris.Context) error { c.Set("celeris.compress.active", true); return c.Next() }
	ctx, rec := celeristest.NewContextT(t, "GET", "/f.txt",
		celeristest.WithHandlers(compressInFront, New(Config{Root: dir})), celeristest.WithHeader("accept-encoding", "gzip"))
	if err := ctx.Next(); err != nil {
		t.Fatal(err)
	}
	if _, ok := ctx.Get("celeris.compressible-representation"); ok || rec.StatusCode != 200 || !strings.HasPrefix(rec.Header("etag"), "W/") {
		t.Errorf("Root: marked=%v status=%d tag=%q, want no mark on a 200 with a weak tag", ok, rec.StatusCode, rec.Header("etag"))
	}
}

// The mark is stored in the context's keys, which Context.Keys hands to user
// code (a logging middleware may print every value): it must not reach the
// file's bytes, which may be up to 100 MB.
func TestFSMarkDoesNotExposeTheFileBytes888(t *testing.T) {
	const marker = "SECRET-FILE-BYTES-0123456789"
	content := []byte(strings.Repeat(marker, 400))
	fsys := fstest.MapFS{"f.txt": {Data: content, ModTime: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)}}
	compressInFront := func(c *celeris.Context) error { c.Set("celeris.compress.active", true); return c.Next() }
	ctx, _ := celeristest.NewContextT(t, "GET", "/f.txt",
		celeristest.WithHandlers(compressInFront, New(Config{FS: fsys})), celeristest.WithHeader("accept-encoding", "gzip"))
	if err := ctx.Next(); err != nil {
		t.Fatal(err)
	}
	keys := ctx.Keys()
	v, ok := keys["celeris.compressible-representation"]
	if !ok {
		t.Fatalf("no mark in Keys(): %v", keys)
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		for _, out := range []string{fmt.Sprintf(format, v), fmt.Sprintf(format, keys)} {
			if strings.Contains(out, marker) || strings.Contains(out, "83, 69, 67") || len(out) > 4096 {
				t.Errorf("%s of the mark exposes the file (len %d): %.200s", format, len(out), out)
			}
		}
	}
}
