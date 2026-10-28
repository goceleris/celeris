package static

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/celeristest"
)

// Round 4 of the security review: a FIFO planted in the served tree still hung the
// request in the places an FS opens a name before looking at it: the index probe
// of a directory (d/index.html is a FIFO), and a pre-compressed variant
// (x.js.br is a FIFO). Every Open of a name from the tree is now preceded by the
// same check, ctxkit.NotRegularFS, on the resolved name. Two cases cannot be
// checked and are documented instead: a link that leaves an FS which is no
// fs.StatFS (an fs.Sub of an os.DirFS) under DotfilesAllow, and FileFromFS of
// such a name, because the FS cannot say what is beyond the link.

func fifoTree888(t *testing.T) (top string, dirfs fs.FS, sub fs.FS) {
	t.Helper()
	st := newSpecialTree888(t) // reg.txt, fifo, sock, links
	for p, body := range map[string]string{"x.js": "XJS", "d/other.txt": "O"} {
		p = filepath.Join(st.dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := mkfifo888(filepath.Join(st.dir, "d", "index.html")); err != nil {
		t.Skip(err)
	}
	if err := mkfifo888(filepath.Join(st.dir, "x.js.br")); err != nil {
		t.Skip(err)
	}
	s, err := fs.Sub(os.DirFS(filepath.Dir(st.dir)), filepath.Base(st.dir))
	if err != nil {
		t.Fatal(err)
	}
	return st.dir, os.DirFS(st.dir), s
}

// TestFIFOIndexAndVariantUnderFS888: GET /d/ where d/index.html is a FIFO is
// answered as a directory with no index (passed on, or listed with Browse), and
// GET /x.js with Accept-Encoding: br where x.js.br is a FIFO gets the identity
// file; neither waits for a writer.
func TestFIFOIndexAndVariantUnderFS888(t *testing.T) {
	_, dirfs, sub := fifoTree888(t)
	for name, fsys := range map[string]fs.FS{"dirfs": dirfs, "sub": sub} {
		for _, pol := range []DotfilesPolicy{DotfilesIgnore, DotfilesDeny, DotfilesAllow} {
			t.Run(name+"/index", func(t *testing.T) {
				for _, browse := range []bool{false, true} {
					mw := New(Config{FS: fsys, Dotfiles: pol, Browse: browse})
					rec := within888(t, "GET /d/", 5*time.Second, func() *celeristest.ResponseRecorder {
						rec, _ := get888(t, mw, "/d/")
						return rec
					})
					if browse && (rec.StatusCode != 200 || !strings.Contains(string(rec.Body), "other.txt")) {
						t.Errorf("%s policy %d Browse: GET /d/ status %d body %q, want the listing", name, pol, rec.StatusCode, rec.Body)
					}
					if !browse && (rec.StatusCode != 0 || len(rec.Body) != 0) {
						t.Errorf("%s policy %d: GET /d/ status %d body %q, want it passed on", name, pol, rec.StatusCode, rec.Body)
					}
				}
			})
			t.Run(name+"/variant", func(t *testing.T) {
				mw := New(Config{FS: fsys, Dotfiles: pol, Compress: true})
				rec := within888(t, "GET /x.js", 5*time.Second, func() *celeristest.ResponseRecorder {
					rec, _ := get888(t, mw, "/x.js", "accept-encoding", "br")
					return rec
				})
				if rec.StatusCode != 200 || string(rec.Body) != "XJS" || rec.Header("content-encoding") != "" {
					t.Errorf("%s policy %d: GET /x.js with a FIFO .br: status %d body %q content-encoding %q, want the identity file", name, pol, rec.StatusCode, rec.Body, rec.Header("content-encoding"))
				}
			})
		}
	}
	// the SPA fallback does not open it either
	spa := New(Config{FS: sub, SPA: true})
	rec := within888(t, "SPA GET /d/", 5*time.Second, func() *celeristest.ResponseRecorder {
		rec, _ := get888(t, spa, "/d/")
		return rec
	})
	if rec.StatusCode == 200 && len(rec.Body) > 0 {
		t.Logf("SPA GET /d/: %q (the index of the root, if any)", rec.Body)
	}
}

// TestFIFOLeavingLinksAreRefusedUnderIgnore888 pins the cases around the two that
// cannot be checked: under Ignore and Deny a link that leaves the FS is refused
// before anything is opened, and an os.DirFS (a StatFS) follows the link with
// stat, so a link to a FIFO is found even under Allow.
func TestFIFOLeavingLinksAreRefusedUnderIgnore888(t *testing.T) {
	st := newSpecialTree888(t)
	parent := filepath.Dir(st.dir)
	outFifo := filepath.Join(parent, "outfifo")
	if err := mkfifo888(outFifo); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = os.Remove(outFifo) })
	for link, target := range map[string]string{"absfifo": outFifo, "relfifo": "../outfifo"} {
		if err := os.Symlink(target, filepath.Join(st.dir, link)); err != nil {
			t.Skipf("symlinks are not available here: %v", err)
		}
	}
	sub, err := fs.Sub(os.DirFS(parent), filepath.Base(st.dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/absfifo", "/relfifo"} {
		for name, cfg := range map[string]Config{
			"sub-ignore": {FS: sub}, "sub-deny": {FS: sub, Dotfiles: DotfilesDeny},
			"dirfs-ignore": {FS: os.DirFS(st.dir)}, "dirfs-allow": {FS: os.DirFS(st.dir), Dotfiles: DotfilesAllow},
		} {
			mw := New(cfg)
			rec := within888(t, name+" GET "+p, 5*time.Second, func() *celeristest.ResponseRecorder {
				rec, _ := get888(t, mw, p)
				return rec
			})
			if rec.StatusCode == 200 || len(rec.Body) != 0 {
				t.Errorf("%s GET %s: status %d body %q", name, p, rec.StatusCode, rec.Body)
			}
		}
	}
}
