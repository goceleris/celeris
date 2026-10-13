package static

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// Tests for celeris#888 item 35. A conditional request for a Root file used to
// be answered with a 304 from os.Stat of the path, which follows a symlink out
// of the root before Context.FileFromDir checks containment. A request whose
// If-None-Match equals the tag of an escaping symlink's target (or is "*"), or
// whose If-Modified-Since is at or after its mtime, got a 304 carrying that
// file's ETag and Last-Modified: an oracle on a file outside the root. A path
// that resolves outside the root is refused, as it is without a validator
// (FileFromDir's 400), and a symlink that stays inside is still served.

var (
	secretTime888  = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	secretBytes888 = []byte("outside-the-root-secret")
)

// root888 is <tmp>/root (the Root) next to <tmp>/outside/secret.txt.
type root888 struct {
	root, outside string
}

func newRoot888(t *testing.T) root888 {
	t.Helper()
	tmp := t.TempDir()
	r := root888{root: filepath.Join(tmp, "root"), outside: filepath.Join(tmp, "outside")}
	for _, d := range []string{r.root, r.outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.write(t, filepath.Join(r.outside, "secret.txt"), secretBytes888, secretTime888)
	return r
}

func (r root888) write(t *testing.T, p string, data []byte, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func (r root888) link(t *testing.T, target, name string) {
	t.Helper()
	p := filepath.Join(r.root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Skipf("symlinks are not available here: %v", err) // a skip is absent, not a pass
	}
}

func get888(t *testing.T, mw celeris.HandlerFunc, path string, hdr ...string) (*celeristest.ResponseRecorder, error) {
	t.Helper()
	opts := []celeristest.Option{celeristest.WithHandlers(mw)}
	for i := 0; i+1 < len(hdr); i += 2 {
		opts = append(opts, celeristest.WithHeader(hdr[i], hdr[i+1]))
	}
	ctx, rec := celeristest.NewContextT(t, "GET", path, opts...)
	return rec, ctx.Next()
}

func isHTTP400(err error) bool {
	var he *celeris.HTTPError
	return errors.As(err, &he) && he.Code == 400
}

// TestRoot304DoesNotFollowSymlinkOut888: a conditional request for a path that
// resolves outside the Root is refused (400), with no ETag and no
// Last-Modified on the response, for the leaf, a parent directory, a
// pre-compressed variant and the directory index.
func TestRoot304DoesNotFollowSymlinkOut888(t *testing.T) {
	secretTag := weakETag(secretTime888, int64(len(secretBytes888)))
	secretDate := secretTime888.UTC().Format(http.TimeFormat)
	cases := []struct {
		name  string
		setup func(t *testing.T, r root888)
		path  string
		ae    string
	}{
		{"leaf", func(t *testing.T, r root888) {
			r.link(t, filepath.Join(r.outside, "secret.txt"), "leak.txt")
		}, "/leak.txt", ""},
		{"leaf-relative-target", func(t *testing.T, r root888) {
			r.link(t, filepath.Join("..", "outside", "secret.txt"), "leak.txt")
		}, "/leak.txt", ""},
		{"sibling-sharing-the-root-prefix", func(t *testing.T, r root888) {
			sib := r.root + "-evil" // <tmp>/root-evil starts with "<tmp>/root"
			r.write(t, filepath.Join(sib, "secret.txt"), secretBytes888, secretTime888)
			r.link(t, filepath.Join(sib, "secret.txt"), "leak.txt")
		}, "/leak.txt", ""},
		{"parent-directory", func(t *testing.T, r root888) {
			r.link(t, r.outside, "linkdir")
		}, "/linkdir/secret.txt", ""},
		{"deep-parent-directory", func(t *testing.T, r root888) {
			r.link(t, r.outside, filepath.Join("a", "b", "linkdir"))
		}, "/a/b/linkdir/secret.txt", ""},
		{"gzip-variant", func(t *testing.T, r root888) {
			r.write(t, filepath.Join(r.root, "app.js"), []byte("plain"), secretTime888.Add(-time.Hour))
			r.link(t, filepath.Join(r.outside, "secret.txt"), "app.js.gz")
		}, "/app.js", "gzip"},
		{"directory-index", func(t *testing.T, r root888) {
			r.link(t, filepath.Join(r.outside, "secret.txt"), filepath.Join("sub", "index.html"))
		}, "/sub/", ""},
	}
	conds := []struct {
		name string
		hdr  []string
	}{
		{"if-none-match-tag", []string{"if-none-match", secretTag}},
		{"if-none-match-star", []string{"if-none-match", "*"}},
		{"if-modified-since", []string{"if-modified-since", secretDate}},
		{"unconditional", nil},
	}
	for _, tc := range cases {
		for _, cond := range conds {
			t.Run(tc.name+"/"+cond.name, func(t *testing.T) {
				r := newRoot888(t)
				tc.setup(t, r)
				mw := New(Config{Root: r.root, Compress: tc.ae != "", MaxAge: time.Hour})
				hdr := append([]string{}, cond.hdr...)
				if tc.ae != "" {
					hdr = append(hdr, "accept-encoding", tc.ae)
				}
				rec, err := get888(t, mw, tc.path, hdr...)
				if rec.StatusCode == 304 {
					t.Errorf("a path outside the Root answered 304 (etag %q, last-modified %q)",
						rec.Header("etag"), rec.Header("last-modified"))
				}
				if !isHTTP400(err) {
					t.Errorf("handler error %v, want a 400 (FileFromDir's refusal)", err)
				}
				for _, h := range []string{"etag", "last-modified", "cache-control", "content-encoding"} {
					if v := rec.Header(h); v != "" {
						t.Errorf("%s %q on a refused request, want none", h, v)
					}
				}
			})
		}
	}
}

// TestRoot304FollowsSymlinkInside888 is the control: a symlink that stays
// inside the Root (file, parent directory, relative and absolute targets, a
// pre-compressed variant) still gets its 304, with the target's validators.
func TestRoot304FollowsSymlinkInside888(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, r root888)
		path  string
		ae    string
	}{
		{"leaf-relative", func(t *testing.T, r root888) {
			r.write(t, filepath.Join(r.root, "real.txt"), secretBytes888, secretTime888)
			r.link(t, "real.txt", "alias.txt")
		}, "/alias.txt", ""},
		{"leaf-absolute", func(t *testing.T, r root888) {
			r.write(t, filepath.Join(r.root, "real.txt"), secretBytes888, secretTime888)
			r.link(t, filepath.Join(r.root, "real.txt"), "alias.txt")
		}, "/alias.txt", ""},
		{"parent-directory", func(t *testing.T, r root888) {
			r.write(t, filepath.Join(r.root, "real", "f.txt"), secretBytes888, secretTime888)
			r.link(t, "real", "alias")
		}, "/alias/f.txt", ""},
		{"gzip-variant", func(t *testing.T, r root888) {
			r.write(t, filepath.Join(r.root, "app.js"), []byte("plain"), secretTime888)
			r.write(t, filepath.Join(r.root, "real.gz"), secretBytes888, secretTime888)
			r.link(t, "real.gz", "app.js.gz")
		}, "/app.js", "gzip"},
		{"plain", func(t *testing.T, r root888) {
			r.write(t, filepath.Join(r.root, "d", "plain.txt"), secretBytes888, secretTime888)
		}, "/d/plain.txt", ""},
	}
	tag := weakETag(secretTime888, int64(len(secretBytes888)))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRoot888(t)
			tc.setup(t, r)
			mw := New(Config{Root: r.root, Compress: tc.ae != ""})
			hdr := []string{"if-none-match", tag}
			if tc.ae != "" {
				hdr = append(hdr, "accept-encoding", tc.ae)
			}
			rec, err := get888(t, mw, tc.path, hdr...)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rec.StatusCode != 304 {
				t.Fatalf("status %d, want 304 for a symlink inside the Root", rec.StatusCode)
			}
			if got := rec.Header("etag"); got != tag {
				t.Errorf("etag %q, want %q", got, tag)
			}
		})
	}
}

// TestRoot304RelativeRoot888: the containment check of the 304 holds for a Root
// given as a relative path, an escape in an upper element included, and the
// symlink inside the root still gets its 304.
func TestRoot304RelativeRoot888(t *testing.T) {
	r := newRoot888(t)
	r.link(t, r.outside, filepath.Join("a", "linkdir"))
	r.write(t, filepath.Join(r.root, "a", "ok", "f.txt"), secretBytes888, secretTime888)
	t.Chdir(filepath.Dir(r.root))
	tag := weakETag(secretTime888, int64(len(secretBytes888)))
	for _, rel := range []string{"root", "./root", "root/a/.."} {
		mw := New(Config{Root: rel})
		rec, err := get888(t, mw, "/a/linkdir/secret.txt", "if-none-match", tag)
		if rec.StatusCode == 304 || !isHTTP400(err) {
			t.Errorf("Root %q: escape answered status %d, err %v; want a 400", rel, rec.StatusCode, err)
		}
		if got := rec.Header("etag"); got != "" {
			t.Errorf("Root %q: etag %q on a refused request, want none", rel, got)
		}
		rec, err = get888(t, mw, "/a/ok/f.txt", "if-none-match", tag)
		if err != nil || rec.StatusCode != 304 {
			t.Errorf("Root %q: file inside answered status %d, err %v; want a 304", rel, rec.StatusCode, err)
		}
	}
}

// TestRoot304SymlinkedRoot888: the Root itself is a symlink to the directory
// that holds the files (a "current -> release-N" deploy). Its own path is not
// part of the check, so a symlink inside still gets its 304 and one that leaves
// the real directory is still refused.
func TestRoot304SymlinkedRoot888(t *testing.T) {
	r := newRoot888(t)
	r.write(t, filepath.Join(r.root, "real.txt"), secretBytes888, secretTime888)
	r.link(t, "real.txt", "alias.txt")
	r.link(t, filepath.Join(r.outside, "secret.txt"), "leak.txt")
	rootLink := filepath.Join(filepath.Dir(r.root), "current")
	if err := os.Symlink(r.root, rootLink); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	tag := weakETag(secretTime888, int64(len(secretBytes888)))
	mw := New(Config{Root: rootLink})
	rec, err := get888(t, mw, "/alias.txt", "if-none-match", tag)
	if err != nil || rec.StatusCode != 304 {
		t.Errorf("symlink inside a symlinked Root: status %d, err %v; want a 304", rec.StatusCode, err)
	}
	rec, err = get888(t, mw, "/leak.txt", "if-none-match", tag)
	if rec.StatusCode == 304 || !isHTTP400(err) || rec.Header("etag") != "" {
		t.Errorf("escape from a symlinked Root: status %d, err %v, etag %q; want a 400 and no etag",
			rec.StatusCode, err, rec.Header("etag"))
	}
}

// TestRoot304IndexOutsideRoot888: Config.Index is joined to the requested
// directory without a lexical check, so an Index that climbs out of the root
// names a file outside it without any symlink. Its 304 is refused as well.
func TestRoot304IndexOutsideRoot888(t *testing.T) {
	r := newRoot888(t)
	r.write(t, filepath.Join(r.root+"-evil", "secret.txt"), secretBytes888, secretTime888) // shares the root's name prefix
	tag := weakETag(secretTime888, int64(len(secretBytes888)))
	for _, index := range []string{
		filepath.Join("..", "outside", "secret.txt"),
		filepath.Join("..", "root-evil", "secret.txt"),
	} {
		mw := New(Config{Root: r.root, Index: index})
		for _, hdr := range [][]string{{"if-none-match", tag}, {"if-none-match", "*"}, nil} {
			rec, err := get888(t, mw, "/", hdr...)
			if rec.StatusCode == 304 || !isHTTP400(err) || rec.Header("etag") != "" || rec.Header("last-modified") != "" {
				t.Errorf("Index %q %v: status %d, err %v, etag %q, last-modified %q; want a 400 and no validators",
					index, hdr, rec.StatusCode, err, rec.Header("etag"), rec.Header("last-modified"))
			}
		}
	}
}

// TestRoot304IndexOutsideDotRoot888: the same climbing Index with a Root of "."
// (the working directory). The lexical test must hold when the root has no
// prefix to compare with: a 304 for a file outside it is refused here too.
func TestRoot304IndexOutsideDotRoot888(t *testing.T) {
	r := newRoot888(t)
	t.Chdir(r.root)
	tag := weakETag(secretTime888, int64(len(secretBytes888)))
	for _, root := range []string{".", "./"} {
		mw := New(Config{Root: root, Index: filepath.Join("..", "outside", "secret.txt")})
		for _, hdr := range [][]string{{"if-none-match", tag}, {"if-none-match", "*"}, nil} {
			rec, err := get888(t, mw, "/", hdr...)
			if rec.StatusCode == 304 || !isHTTP400(err) || rec.Header("etag") != "" || rec.Header("last-modified") != "" {
				t.Errorf("Root %q %v: status %d, err %v, etag %q, last-modified %q; want a 400 and no validators",
					root, hdr, rec.StatusCode, err, rec.Header("etag"), rec.Header("last-modified"))
			}
		}
	}
}
