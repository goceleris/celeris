package static

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// Round 2 of the security review of the dotfile rule.

// TestDotfilesDirFS888 (R2-F1): the rule follows symlinks under Config.FS too,
// for an FS that can resolve them (a real os.DirFS, an fs.ReadLinkFS): a visible
// name that links to a dotfile, a hidden directory, a chain of links, a link
// into .well-known, a link that goes through ".." are refused like the hidden
// name, a hidden name stays hidden whatever it links to, and a link to
// .well-known is served. The same table as the Root tests.
func TestDotfilesDirFS888(t *testing.T) {
	skipWindows888(t)
	root := dotTree(t)
	for _, pol := range []DotfilesPolicy{DotfilesIgnore, DotfilesDeny, DotfilesAllow} {
		mw := New(Config{FS: os.DirFS(root), Dotfiles: pol})
		absent := missing888(t, mw)
		for _, tc := range dotCases888 {
			if strings.Contains(tc.path, "\x00") || strings.Contains(tc.path, "%00") {
				continue
			}
			weird := strings.Contains(tc.path, "..") || strings.Contains(tc.path, "/./") || strings.Contains(tc.path, "//")
			t.Run(fmt.Sprintf("policy%d/%q", pol, tc.path), func(t *testing.T) {
				rec, err := get888(t, mw, tc.path)
				status := rec.StatusCode
				if he, ok := err.(*celeris.HTTPError); ok {
					status = he.Code
				}
				switch {
				case pol == DotfilesAllow || !tc.hidden:
					if tc.body != "" && !weird && (status != 200 || string(rec.Body) != tc.body) {
						t.Errorf("GET %q: status %d body %q, want 200 %q", tc.path, status, rec.Body, tc.body)
					}
				case pol == DotfilesIgnore:
					if status != absent.status || len(rec.Body) != 0 || err != nil {
						t.Errorf("GET %q hidden under Ignore: status %d body %q err %v, want passed on", tc.path, status, rec.Body, err)
					}
				default:
					if status != 403 || len(rec.Body) != 0 {
						t.Errorf("GET %q hidden under Deny: status %d body %q err %v, want 403", tc.path, status, rec.Body, err)
					}
				}
			})
		}
		// a listing does not enter a visible link to a hidden directory
		bw := New(Config{FS: os.DirFS(root), Dotfiles: pol, Browse: true, Index: "no-index.here"})
		if rec, _ := get888(t, bw, "/link-dir/"); (rec.StatusCode == 200) != (pol == DotfilesAllow) {
			t.Errorf("policy %d GET /link-dir/ (listing): status %d", pol, rec.StatusCode)
		}
		// and a 304 is no oracle either
		rec, _ := get888(t, mw, "/link-env", "if-none-match", "*")
		if (rec.StatusCode == 304 || rec.StatusCode == 200) != (pol == DotfilesAllow) {
			t.Errorf("policy %d GET /link-env with If-None-Match: status %d", pol, rec.StatusCode)
		}
	}
}

// TestDotfilesCompressVariantLink888 (R2-F3): a visible file whose ".gz" variant
// is a symlink to a hidden file is served as the identity file (no variant),
// not 404.
func TestDotfilesCompressVariantLink888(t *testing.T) {
	skipWindows888(t)
	root := dotTree(t)
	if err := os.WriteFile(filepath.Join(root, "b.js"), []byte("BJS"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".env", filepath.Join(root, "b.js.gz")); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	for _, mw := range []celeris.HandlerFunc{
		New(Config{Root: root, Compress: true}),
		New(Config{FS: os.DirFS(root), Compress: true}),
	} {
		rec, err := get888(t, mw, "/b.js", "accept-encoding", "gzip")
		if err != nil || rec.StatusCode != 200 || string(rec.Body) != "BJS" || rec.Header("content-encoding") != "" {
			t.Errorf("GET /b.js with a hidden .gz link: status %d body %q content-encoding %q err %v, want the identity file", rec.StatusCode, rec.Body, rec.Header("content-encoding"), err)
		}
	}
}

// TestDotfilesPolicyIsValidated888: an unknown DotfilesPolicy panics at New, as
// the other invalid Config values do.
func TestDotfilesPolicyIsValidated888(t *testing.T) {
	for _, pol := range []DotfilesPolicy{3, 7, 255} {
		func() {
			defer func() {
				r := recover()
				if r == nil {
					t.Errorf("New with Dotfiles %d did not panic", pol)
					return
				}
				if msg, _ := r.(string); !strings.Contains(msg, "Dotfiles") {
					t.Errorf("New with Dotfiles %d panicked with %v, want a message about Dotfiles", pol, r)
				}
			}()
			New(Config{Root: ".", Dotfiles: pol})
		}()
	}
	for _, pol := range []DotfilesPolicy{DotfilesIgnore, DotfilesAllow, DotfilesDeny} {
		New(Config{Root: ".", Dotfiles: pol}) // must not panic
	}
}

// TestServerStaticMissingIs404_888: Server.Static and Group.Static answer a file
// that is not there (and a dangling symlink) with 404, as they answer a hidden
// path, not 500 with the raw lstat error.
func TestServerStaticMissingIs404_888(t *testing.T) {
	skipWindows888(t)
	root := dotTree(t)
	if err := os.Symlink("no-such-target", filepath.Join(root, "dangling")); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	for _, eng := range wireEngines888 {
		raw := wire888(t, eng, func(s *celeris.Server) {
			s.Static("/files", root)
			s.Group("/g").Static("/files", root)
		})
		for _, p := range []string{"/files/nope.txt", "/files/dangling", "/files/sub/nope.txt", "/g/files/nope.txt", "/files/%252eenv", "/files/.env", "/files/.missing"} {
			if status, body := raw(p); status != 404 {
				t.Errorf("%v GET %s: status %d body %q, want 404", eng, p, status, body)
			}
		}
		if status, _ := raw("/files/a.txt"); status != 200 {
			t.Errorf("%v GET /files/a.txt: status %d", eng, status)
		}
	}
}

// TestDotDotRootCostsAboutADotRoot888 (R2-F6): a Root of ".." segments is
// resolved once, when the middleware is made, so a request costs what it costs
// under an absolute Root (the same work), not four times that (Getwd and
// EvalSymlinks of the working directory in every containment check: 388
// allocations against 98 on the review's measurement).
func TestDotDotRootCostsAboutADotRoot888(t *testing.T) {
	skipWindows888(t)
	d := newDotTree888(t) // the working directory is T/p/c
	if err := os.MkdirAll(filepath.Join(d.cwd, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	cost := func(root, path string) float64 {
		mw := New(Config{Root: root})
		return testing.AllocsPerRun(20, func() {
			ctx, _ := celeristest.NewContext("GET", path, celeristest.WithHandlers(mw))
			_ = ctx.Next()
			celeristest.ReleaseContext(ctx)
		})
	}
	dot := cost(d.cwd, "/a.txt") // an absolute Root: what a resolved ".." root is
	for _, tc := range []struct{ root, path string }{{"..", "/c/a.txt"}, {"../c", "/a.txt"}, {"x/..", "/a.txt"}, {"../..", "/p/c/a.txt"}} {
		if got := cost(tc.root, tc.path); got > dot+8 {
			t.Errorf("Root %q: %.0f allocations per request, an absolute Root costs %.0f", tc.root, got, dot)
		}
	}
	// a working directory that changes after New is not followed by a resolved root
	mw := New(Config{Root: ".."})
	t.Chdir(d.cwd + "/sub")
	rec, _ := get888(t, mw, "/PSECRET.txt")
	if rec.StatusCode != 200 || string(rec.Body) != d.files[filepath.Join(d.top, "p", "PSECRET.txt")] {
		t.Errorf("Root \"..\" after a chdir: status %d body %q, want the file of the directory it resolved to at New", rec.StatusCode, rec.Body)
	}
}
