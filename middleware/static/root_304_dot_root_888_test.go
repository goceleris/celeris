package static

import (
	"net/http"
	"path/filepath"
	"testing"
)

// Item 35 of celeris#888 (a 304 is answered only for a path that resolves
// inside the Root) for a Root of "." and "./", the working directory:
// an absolute and a relative symlink to a file outside, a symlink to a
// directory outside, an escaping index symlink, each with If-None-Match (the
// target's tag and "*"), If-Modified-Since and no validator. celeris#888 item
// 46: no test pinned these; the 304 tests for symlinks ran with an absolute
// Root only.
//
// What these roots do is narrower than the doc suggested (found while writing
// this, see TestRootDotAndSlashServeOnlyTheRootIndex888): serveOS compares the
// joined path with cleanRoot + "/", which for "." and "/" no path below the
// root starts with, so every path but the root's own is passed to the next
// handler. The cases below that name a path below the root therefore never
// reach the containment check; the root's own index does, and that is where an
// escape can happen. The assertions hold for both: no 304, and no validators of
// the target on the response.

func TestRoot304DotRootDoesNotFollowSymlinkOut888(t *testing.T) {
	secretTag := weakETag(secretTime888, int64(len(secretBytes888)))
	secretDate := secretTime888.UTC().Format(http.TimeFormat)
	cases := []struct {
		name  string
		setup func(t *testing.T, r root888)
		path  string
	}{
		{"leaf-absolute", func(t *testing.T, r root888) {
			r.link(t, filepath.Join(r.outside, "secret.txt"), "leak.txt")
		}, "/leak.txt"},
		{"leaf-relative", func(t *testing.T, r root888) {
			r.link(t, filepath.Join("..", "outside", "secret.txt"), "leak.txt")
		}, "/leak.txt"},
		{"parent-directory", func(t *testing.T, r root888) {
			r.link(t, r.outside, "linkdir")
		}, "/linkdir/secret.txt"},
		{"index-absolute", func(t *testing.T, r root888) {
			r.link(t, filepath.Join(r.outside, "secret.txt"), filepath.Join("sub", "index.html"))
		}, "/sub/"},
		{"index-relative", func(t *testing.T, r root888) {
			r.link(t, filepath.Join("..", "..", "outside", "secret.txt"), filepath.Join("sub", "index.html"))
		}, "/sub/"},
		{"root-index-absolute", func(t *testing.T, r root888) {
			r.link(t, filepath.Join(r.outside, "secret.txt"), "index.html")
		}, "/"},
		{"root-index-relative", func(t *testing.T, r root888) {
			r.link(t, filepath.Join("..", "outside", "secret.txt"), "index.html")
		}, "/"},
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
	for _, root := range []string{".", "./"} {
		for _, tc := range cases {
			for _, cond := range conds {
				t.Run(root+"/"+tc.name+"/"+cond.name, func(t *testing.T) {
					r := newRoot888(t)
					tc.setup(t, r)
					t.Chdir(r.root)
					mw := New(Config{Root: root})
					rec, err := get888(t, mw, tc.path, cond.hdr...)
					if rec.StatusCode == 304 {
						t.Errorf("a path outside the Root answered 304 (etag %q, last-modified %q)", rec.Header("etag"), rec.Header("last-modified"))
					}
					// Only the Root's own index is looked at under these roots
					// (see TestRootDotAndSlashServeOnlyTheRootIndex888): there
					// FileFromDir refuses with a 400; every other path is passed
					// to the next handler, with nothing on the response.
					if tc.path == "/" && !isHTTP400(err) {
						t.Errorf("handler error %v, want a 400 (FileFromDir's refusal)", err)
					}
					if tc.path != "/" && (err != nil || rec.StatusCode != 0) {
						t.Errorf("status %d err %v for a path below the Root, want it passed on (nothing written)", rec.StatusCode, err)
					}
					for _, h := range []string{"etag", "last-modified"} {
						if v := rec.Header(h); v != "" {
							t.Errorf("%s %q on a refused request, want none", h, v)
						}
					}
				})
			}
		}
	}
}

// TestRootDotAndSlashServeOnlyTheRootIndex888 pins what the package doc says of
// a Root of ".", "./" and "/" (celeris#888 item 46). A request for the root
// directory itself is answered by its index file through two checks that
// disagree: the static middleware's own (resolvesInside), which accepts a file
// in the working directory, and Context.FileFromDir's, which requires the "./"
// prefix a Root of "." does not have. So GET / is a 400 without a validator and
// a 304 when one matches; the file is inside the root either way. A request for
// anything below the root is passed to the next handler, whatever it names.
// If a change makes these roots serve files, this test and the doc change with it.
func TestRootDotAndSlashServeOnlyTheRootIndex888(t *testing.T) {
	r := newRoot888(t)
	r.write(t, filepath.Join(r.root, "index.html"), []byte("<p>hi</p>"), secretTime888)
	r.write(t, filepath.Join(r.root, "d", "plain.txt"), secretBytes888, secretTime888)
	t.Chdir(r.root)
	indexTag := weakETag(secretTime888, int64(len("<p>hi</p>")))
	fileTag := weakETag(secretTime888, int64(len(secretBytes888)))
	for _, root := range []string{".", "./"} {
		mw := New(Config{Root: root})
		rec, err := get888(t, mw, "/")
		if !isHTTP400(err) {
			t.Errorf("Root %q: GET / -> status %d err %v, want a 400 from FileFromDir", root, rec.StatusCode, err)
		}
		rec, err = get888(t, mw, "/", "if-none-match", indexTag)
		if err != nil || rec.StatusCode != 304 {
			t.Errorf("Root %q: GET / with the index's tag -> status %d err %v, want a 304", root, rec.StatusCode, err)
		}
		for _, hdr := range [][]string{nil, {"if-none-match", fileTag}} {
			rec, err = get888(t, mw, "/d/plain.txt", hdr...)
			if err != nil || rec.StatusCode != 0 {
				t.Errorf("Root %q: GET /d/plain.txt %v -> status %d err %v, want it passed on (nothing written)", root, hdr, rec.StatusCode, err)
			}
		}
	}
	// "/" is the same through the other end: the root is the filesystem's.
	abs := filepath.Join(r.root, "d", "plain.txt")
	rec, err := get888(t, New(Config{Root: "/"}), abs)
	if err != nil || rec.StatusCode != 0 {
		t.Errorf("Root %q: GET %s -> status %d err %v, want it passed on (nothing written)", "/", abs, rec.StatusCode, err)
	}
}
