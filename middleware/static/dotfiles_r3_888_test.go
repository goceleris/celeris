package static

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// Round 3 of the security review: under Config.FS the symlink rule stopped at
// the first link that leaves the FS: a hidden file reached through two planted
// links (one absolute, or one that climbs out of the FS and back) was served
// under Ignore and under Deny. A link that leaves the FS is not followed any
// more: the FS cannot vouch for what is beyond it, so under Ignore and Deny the
// path is refused (as a missing file / 403), and Allow serves it as before.

// linkTree888 is top/pub (the served directory) and top/outside, with a .env
// above pub, one in it, and the links of the review's probe.
type linkTree888 struct{ top, pub, outside string }

func newLinkTree888(t *testing.T) linkTree888 {
	t.Helper()
	skipWindows888(t)
	top, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lt := linkTree888{top: top, pub: filepath.Join(top, "pub"), outside: filepath.Join(top, "outside")}
	for p, body := range map[string]string{
		"pub/.env": "DOTENV", "pub/.git/config": "GITCFG", "pub/a.txt": "A", "pub/sub/b.txt": "B", "pub/d/x": "X",
		".env": "PARENTENV", "outside/vis.txt": "VIS",
	} {
		p = filepath.Join(top, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		// inside the FS
		"pub/alias": ".env", "pub/c2a": "c2b", "pub/c2b": ".env", "pub/c3a": "c3b", "pub/c3b": "c3c", "pub/c3c": ".git/config",
		"pub/v1": "v2", "pub/v2": "a.txt", "pub/in-visible": "sub/b.txt", "pub/sub/e": "../.env", "pub/sub/g": "../.git",
		// out of the FS, and back in
		"pub/parent-env": "../.env", "outside/pe": "../.env", "pub/out-parent-env": "../outside/pe",
		"outside/back": "../pub/.env", "pub/out-and-back": "../outside/back",
		"outside/absgit": filepath.Join(top, "pub", ".git"), "pub/out-absgit": "../outside/absgit",
		"pub/up": "..",
		// absolute
		"pub/abs-to-alias": filepath.Join(top, "pub", "alias"), "pub/abs-sub": filepath.Join(top, "pub", "sub"),
		"pub/abs-visible": filepath.Join(top, "outside", "vis.txt"),
		// loops
		"pub/loop1": "loop2", "pub/loop2": "loop1",
		// a long link that walks many segments per follow
		"pub/amp": strings.Repeat("d/../", 200) + "amp",
	} {
		if err := os.Symlink(target, filepath.Join(top, filepath.FromSlash(link))); err != nil {
			t.Skipf("symlinks are not available here: %v", err)
		}
	}
	return lt
}

// fsFlavours888 are the FS values the rule must hold for: a real os.DirFS and an
// fs.Sub of one (which hides the part of the tree above pub).
func (lt linkTree888) flavours(t *testing.T) map[string]fs.FS {
	t.Helper()
	sub, err := fs.Sub(os.DirFS(lt.top), "pub")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]fs.FS{"dirfs": os.DirFS(lt.pub), "sub": sub}
}

func TestDotfilesFSLinksLeavingTheFS888(t *testing.T) {
	lt := newLinkTree888(t)
	refused := []string{
		// a single link to a hidden file, in the FS and out of it
		"/alias", "/parent-env",
		// chains of 2 and 3 links in the FS
		"/c2a", "/c3a", "/sub/e", "/sub/g/config",
		// out of the FS and back: the hidden file is in pub, or above it
		"/out-and-back", "/out-parent-env", "/out-absgit/config",
		// behind an absolute link
		"/abs-to-alias", "/abs-sub/e", "/abs-sub/g/config",
		// the FS root's parent, and an absolute link to a visible file outside
		"/up/pub/a.txt", "/up/.env", "/abs-visible",
		// loops
		"/loop1", "/loop2",
	}
	served := map[string]string{"/a.txt": "A", "/sub/b.txt": "B", "/v1": "A", "/v2": "A", "/in-visible": "B"}
	for name, fsys := range lt.flavours(t) {
		for _, pol := range []DotfilesPolicy{DotfilesIgnore, DotfilesDeny, DotfilesAllow} {
			mw := New(Config{FS: fsys, Dotfiles: pol})
			absent := missing888(t, mw)
			for _, p := range refused {
				t.Run(fmt.Sprintf("%s/policy%d%s", name, pol, p), func(t *testing.T) {
					rec, err := get888(t, mw, p)
					status := rec.StatusCode
					if he, ok := err.(*celeris.HTTPError); ok {
						status = he.Code
					}
					switch pol {
					case DotfilesIgnore:
						if status != absent.status || len(rec.Body) != 0 || err != nil {
							t.Errorf("GET %s under Ignore: status %d body %q err %v, want what a missing file gets", p, status, rec.Body, err)
						}
					case DotfilesDeny:
						if status != 403 || len(rec.Body) != 0 {
							t.Errorf("GET %s under Deny: status %d body %q err %v, want 403", p, status, rec.Body, err)
						}
					default: // Allow: the OS follows every link, as before the rule
						if p == "/out-parent-env" && string(rec.Body) != "PARENTENV" && name == "dirfs" {
							t.Errorf("GET %s under Allow: status %d body %q, want it served as before", p, status, rec.Body)
						}
					}
					if pol != DotfilesAllow {
						for _, secret := range []string{"ENV", "GITCFG", "VIS"} {
							if strings.Contains(string(rec.Body), secret) {
								t.Errorf("GET %s leaked %q", p, secret)
							}
						}
					}
				})
			}
			for p, want := range served {
				t.Run(fmt.Sprintf("%s/policy%d-served%s", name, pol, p), func(t *testing.T) {
					if o, _ := outcomeOf888(t, mw, p); o.status != 200 || o.body != want {
						t.Errorf("GET %s: %v, want 200 %q (a link that stays inside the FS is judged by where it ends)", p, o, want)
					}
				})
			}
		}
	}
}

// TestFSLinkWalkIsBounded888: a planted link that walks many segments per follow
// costs a request about what a plain one does, not 24,000 allocations; it is
// refused.
func TestFSLinkWalkIsBounded888(t *testing.T) {
	lt := newLinkTree888(t)
	mw := New(Config{FS: os.DirFS(lt.pub)})
	allocs := testing.AllocsPerRun(5, func() {
		ctx, _ := celeristest.NewContext("GET", "/amp", celeristest.WithHandlers(mw))
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	})
	if allocs > 1500 {
		t.Errorf("GET of a planted amplifying link: %.0f allocations, want it cut off early", allocs)
	}
	if o, _ := outcomeOf888(t, mw, "/amp"); o.kind != "passed-on" {
		t.Errorf("GET /amp: %v, want it refused as missing", o)
	}
}

// TestFIFOUnderSubFS888 (R3-F3): an fs.Sub is no fs.StatFS, so the pre-open stat
// was skipped and a FIFO in a Sub of an os.DirFS blocked the handler. It is
// found through the links the Sub can read.
func TestFIFOUnderSubFS888(t *testing.T) {
	st := newSpecialTree888(t)
	parent := filepath.Dir(st.dir)
	sub, err := fs.Sub(os.DirFS(parent), filepath.Base(st.dir))
	if err != nil {
		t.Fatal(err)
	}
	mw := New(Config{FS: sub})
	for _, p := range []string{"/fifo", "/lnk-fifo", "/sock", "/lnk-sock"} {
		rec := within888(t, "GET "+p, 5*time.Second, func() *celeristest.ResponseRecorder {
			rec, _ := get888(t, mw, p)
			return rec
		})
		if rec.StatusCode != 0 || len(rec.Body) != 0 {
			t.Errorf("fs.Sub GET %s: status %d body %q, want it passed on as a missing file", p, rec.StatusCode, rec.Body)
		}
	}
	for _, name := range []string{"fifo", "lnk-fifo"} {
		err := within888(t, "FileFromFS "+name, 5*time.Second, func() error {
			c, _ := celeristest.NewContext("GET", "/f")
			defer celeristest.ReleaseContext(c)
			return c.FileFromFS(name, sub)
		})
		if he, ok := err.(*celeris.HTTPError); !ok || he.Code != 404 {
			t.Errorf("FileFromFS(%s, fs.Sub) = %v, want an HTTPError 404", name, err)
		}
	}
	if o, _ := outcomeOf888(t, mw, "/reg.txt"); o.status != 200 || o.body != "REG" {
		t.Errorf("fs.Sub GET /reg.txt: %v, want 200 REG", o)
	}
}

// TestServerStaticNotRegularIsNotFound888 (R3-F6): Server.Static answers a FIFO,
// a socket and a link to one with the body of a missing file, not a message that
// tells them apart.
func TestServerStaticNotRegularIsNotFound888(t *testing.T) {
	st := newSpecialTree888(t)
	for _, eng := range wireEngines888 {
		raw := wire888(t, eng, func(s *celeris.Server) { s.Static("/files", st.dir) })
		_, missing := raw("/files/no-such-file")
		for _, p := range []string{"/files/fifo", "/files/lnk-fifo", "/files/sock"} {
			status, body := raw(p)
			if status != 404 || body != missing {
				t.Errorf("%v GET %s: status %d body %q, want 404 %q (as a missing file)", eng, p, status, body, missing)
			}
		}
	}
}

// TestZeroSizePseudoFileIsServedEmpty888 (R3-F4): a file whose size is 0 but
// that has content (/proc/self/pagemap reads 100 MB) is read for as many bytes
// as it says it has, not up to the 100 MB cap: served empty, as under epoll's
// sendfile and under an FS. Linux only.
func TestZeroSizePseudoFileIsServedEmpty888(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs")
	}
	if _, err := os.Stat("/proc/self/pagemap"); err != nil {
		t.Skip("no /proc/self/pagemap")
	}
	guardMemory888(t, 256<<20)
	c, rec := celeristest.NewContext("GET", "/f")
	defer celeristest.ReleaseContext(c)
	began := time.Now()
	err := c.File("/proc/self/pagemap")
	if err != nil || len(rec.Body) != 0 {
		t.Errorf("File(/proc/self/pagemap): %d body bytes, err %v, want an empty body", len(rec.Body), err)
	}
	if d := time.Since(began); d > 2*time.Second {
		t.Errorf("File(/proc/self/pagemap) took %v", d)
	}
}

// BenchmarkDirFSRequest888 is the cost of the symlink walk: a deep path under an
// os.DirFS with the rule (the default) and without it (Allow).
func BenchmarkDirFSRequest888(b *testing.B) {
	root, err := filepath.EvalSymlinks(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b", "c", "d", "e")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		b.Fatal(err)
	}
	for _, p := range []string{filepath.Join(deep, "app.js"), filepath.Join(root, "app.js")} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		pol  DotfilesPolicy
		path string
	}{{"rule/shallow", DotfilesIgnore, "/app.js"}, {"rule/deep", DotfilesIgnore, "/a/b/c/d/e/app.js"},
		{"allow/shallow", DotfilesAllow, "/app.js"}, {"allow/deep", DotfilesAllow, "/a/b/c/d/e/app.js"}} {
		b.Run(tc.name, func(b *testing.B) {
			mw := New(Config{FS: os.DirFS(root), Dotfiles: tc.pol})
			b.ReportAllocs()
			for b.Loop() {
				ctx, _ := celeristest.NewContext("GET", tc.path, celeristest.WithHandlers(mw))
				_ = ctx.Next()
				celeristest.ReleaseContext(ctx)
			}
		})
	}
}
