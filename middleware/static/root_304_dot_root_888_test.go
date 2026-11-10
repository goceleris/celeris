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
// Every case reaches the containment check, the paths below the root as well
// as the root's own index (TestRootSpellingsServeTheTree888 and the other
// root_spellings_888 tests cover what these roots serve). Until celeris#888
// item 46 was fixed serveOS compared the joined path with cleanRoot + "/",
// which no path below "." or "/" starts with, so only the root's own index got
// here and the others were passed on unlooked at. The assertions: no 304, a
// 400 as for any escape, and no validators of the target on the response.

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
					if !isHTTP400(err) {
						t.Errorf("handler error %v, want a 400 (FileFromDir's refusal)", err)
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
