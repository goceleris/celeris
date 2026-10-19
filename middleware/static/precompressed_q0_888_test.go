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

// Tests for the Accept-Encoding gate of the pre-compressed variants (celeris
// #888 item 33). servePreCompressed and servePreCompressedFS used to pick a
// variant with strings.Contains on the raw Accept-Encoding value, so a client
// that refuses an encoding with q=0 ("br;q=0, gzip;q=0") still got it, and
// the match was case-sensitive. The gate is Context.AcceptsEncodings, which
// parses the q-values (it is what compress negotiates with) and names
// Accept-Encoding in Vary, once.

const (
	plain888 = "plain-bytes"
	br888    = "br-bytes"
	gz888    = "gz-bytes"
)

// q0Fixture888 is a path with an identity file and a .br and a .gz sibling,
// served from the Root directory and from an fs.FS.
func q0Fixture888(t *testing.T) map[string]celeris.HandlerFunc {
	t.Helper()
	dir := t.TempDir()
	mod := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	for name, data := range map[string]string{"a.js": plain888, "a.js.br": br888, "a.js.gz": gz888} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	fsys := fstest.MapFS{
		"a.js":    {Data: []byte(plain888), ModTime: mod},
		"a.js.br": {Data: []byte(br888), ModTime: mod},
		"a.js.gz": {Data: []byte(gz888), ModTime: mod},
	}
	return map[string]celeris.HandlerFunc{
		"root": New(Config{Root: dir, Compress: true}),
		"fs":   New(Config{FS: fsys, Compress: true}),
	}
}

// varyCount888 counts how many times the response's Vary lines name
// Accept-Encoding.
func varyCount888(rec *celeristest.ResponseRecorder) (n int, lines []string) {
	for _, h := range rec.Headers {
		if h[0] != "vary" {
			continue
		}
		lines = append(lines, h[1])
		for tok := range strings.SplitSeq(h[1], ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "Accept-Encoding") {
				n++
			}
		}
	}
	return n, lines
}

// TestPreCompressedHonoursQ0And888: a variant is served only when the client
// accepts its encoding, with the q-values read (RFC 9110 12.5.3), on Root and
// on FS. "gzip, br" is the control: both are acceptable and br is preferred.
func TestPreCompressedHonoursQ0And888(t *testing.T) {
	rows := []struct {
		ae       string
		wantEnc  string // "" is the identity file
		wantBody string
	}{
		{"br;q=0, gzip;q=0", "", plain888},
		{"*;q=0", "", plain888},
		{"identity", "", plain888},
		{"br;q=0, gzip", "gzip", gz888},
		{"gzip, br;q=0", "gzip", gz888},
		{"*, br;q=0", "gzip", gz888},
		{"br;q=0.0, gzip;q=0.5", "gzip", gz888},
		{"gzip;q=0, br", "br", br888},
		{"gzip;q=1, br;q=0.1", "br", br888}, // the server's preference (br first) among acceptable codings, as documented on Config.Compress
		{"gzip, br", "br", br888},           // control: the client accepts both, br wins
		{"gzip", "gzip", gz888},
		{"br", "br", br888},
		{"*", "br", br888},
		// "*" matches only what is not listed (RFC 9110 12.5.3), so a coding
		// listed with a q-value above 0 is served although "*;q=0" refuses
		// the rest (celeris#888 item 44), and the name of the weight is
		// case-insensitive (item 45).
		{"gzip, *;q=0", "gzip", gz888},
		{"br, *;q=0", "br", br888},
		{"*;q=0, gzip", "gzip", gz888},
		{"gzip;q=1.0, identity; q=0.5, *;q=0", "gzip", gz888}, // RFC 9110 12.5.3's own example
		{"gzip;q=0, *", "br", br888},
		{"br;q=0, *", "gzip", gz888},
		{"br;Q=0, gzip", "gzip", gz888},
		{"br;Q=0, gzip;Q=0", "", plain888},
		{"x-gzip", "gzip", gz888}, // RFC 9110 8.4.1.3: x-gzip is gzip; the substring test matched it
		{"X-GZIP", "gzip", gz888},
		{"x-gzip, br;q=0", "gzip", gz888},
		{"x-gzip;q=0, br;q=0", "", plain888},
		{"GZIP", "gzip", gz888}, // codings are case-insensitive
		{"BR", "br", br888},
	}
	for name, mw := range q0Fixture888(t) {
		for _, row := range rows {
			t.Run(name+"/"+row.ae, func(t *testing.T) {
				ctx, rec := celeristest.NewContext("GET", "/a.js",
					celeristest.WithHeader("accept-encoding", row.ae),
					celeristest.WithHandlers(mw),
				)
				defer celeristest.ReleaseContext(ctx)
				if err := ctx.Next(); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if rec.StatusCode != 200 {
					t.Fatalf("status %d, want 200", rec.StatusCode)
				}
				if ce := rec.Header("content-encoding"); ce != row.wantEnc {
					t.Errorf("content-encoding %q, want %q", ce, row.wantEnc)
				}
				if got := rec.BodyString(); got != row.wantBody {
					t.Errorf("body %q, want %q", got, row.wantBody)
				}
				// The response depends on Accept-Encoding whichever
				// representation it carries: Vary names it once.
				if n, lines := varyCount888(rec); n != 1 {
					t.Errorf("Vary names Accept-Encoding %d times, want once: vary lines %q", n, lines)
				}
			})
		}
	}
}

// TestPreCompressedNoSiblingVary888 pins the behaviour the AcceptsEncodings
// gate brings: a file without any .br/.gz sibling, requested with an
// Accept-Encoding header, now carries Vary: Accept-Encoding (once), because
// the gate is evaluated before the siblings are looked up. A request without
// Accept-Encoding does not negotiate and carries none.
func TestPreCompressedNoSiblingVary888(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lone.txt"), []byte("lone"), 0o600); err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{"lone.txt": {Data: []byte("lone"), ModTime: time.Unix(1_700_000_000, 0)}}
	for name, mw := range map[string]celeris.HandlerFunc{
		"root": New(Config{Root: dir, Compress: true}),
		"fs":   New(Config{FS: fsys, Compress: true}),
	} {
		for _, ae := range []string{"gzip", "br;q=0, gzip;q=0"} {
			t.Run(name+"/"+ae, func(t *testing.T) {
				ctx, rec := celeristest.NewContext("GET", "/lone.txt",
					celeristest.WithHeader("accept-encoding", ae), celeristest.WithHandlers(mw))
				defer celeristest.ReleaseContext(ctx)
				if err := ctx.Next(); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if ce := rec.Header("content-encoding"); ce != "" {
					t.Errorf("content-encoding %q, want none", ce)
				}
				if n, lines := varyCount888(rec); n != 1 {
					t.Errorf("Vary names Accept-Encoding %d times, want once: vary lines %q", n, lines)
				}
			})
		}
		t.Run(name+"/no-accept-encoding", func(t *testing.T) {
			ctx, rec := celeristest.NewContext("GET", "/lone.txt", celeristest.WithHandlers(mw))
			defer celeristest.ReleaseContext(ctx)
			if err := ctx.Next(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if n, lines := varyCount888(rec); n != 0 {
				t.Errorf("Vary names Accept-Encoding %d times, want none: vary lines %q", n, lines)
			}
		})
	}
}

// TestPreCompressedVaryAlreadyListed888: Vary is named once also when a
// middleware before static set a Vary line by hand (not through
// AcceptsEncodings), with Accept-Encoding listed among other fields or as
// "*"; a Vary line without it gets Accept-Encoding added.
func TestPreCompressedVaryAlreadyListed888(t *testing.T) {
	fx := q0Fixture888(t)
	for _, pre := range []struct {
		vary string
		want int
	}{
		{"Accept-Encoding", 1},
		{"Origin, accept-encoding", 1},
		{"*", 0}, // "*" already covers it: nothing is added, and it is not named
		{"Origin", 1},
	} {
		for name, mw := range fx {
			t.Run(name+"/"+pre.vary, func(t *testing.T) {
				set := func(c *celeris.Context) error {
					c.AddHeader("vary", pre.vary)
					return c.Next()
				}
				ctx, rec := celeristest.NewContext("GET", "/a.js",
					celeristest.WithHeader("accept-encoding", "gzip"),
					celeristest.WithHandlers(set, mw))
				defer celeristest.ReleaseContext(ctx)
				if err := ctx.Next(); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if n, lines := varyCount888(rec); n != pre.want {
					t.Errorf("Vary names Accept-Encoding %d times, want %d: vary lines %q", n, pre.want, lines)
				}
			})
		}
	}
}
