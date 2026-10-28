package static

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// A Root of ".", "./" and "/" serves the tree below it (celeris#888 item 46;
// the guard that refused every path below these roots dates from v1.3.3, and
// the test that pinned it, TestRootDotAndSlashServeOnlyTheRootIndex888, is
// replaced by these). The same file served through an absolute Root is the
// control: every spelling must give the answer the control gives, with the
// traversal protections of the control.
//
// Context.FileFromDir has the same guard, so a path that static passes on to it
// is refused there with a 400 whatever it names; these tests therefore look at
// the bytes of the 200, not only at "not passed on".

const (
	indexBody888 = "<p>root index</p>"
	helloBody888 = "hello world"
	subBody888   = "<p>sub index</p>"
	pageBody888  = "page"
	envBody888   = "SECRET=1"
)

// tree888 writes the tree the tests serve, below r.root:
//
//	index.html, hello.txt, .env, .hidden/x.txt, sub/index.html, sub/page.txt
//	in-abs     -> <root>/hello.txt        (a symlink to a file inside, absolute)
//	in-rel     -> hello.txt               (relative)
//	in-up      -> ../root/hello.txt       (relative, climbs through the root's own name)
//	in-dir     -> <root>/sub              (a symlink to a directory inside, absolute)
//	esc-abs    -> <outside>/secret.txt    (a symlink out of the root)
//	esc-rel    -> ../outside/secret.txt
//	esc-dir    -> <outside>               (a directory symlink out of the root)
//	sub/esc-up -> ../../outside/secret.txt
func tree888(t *testing.T) root888 {
	t.Helper()
	r := newRoot888(t)
	for name, body := range map[string]string{
		"index.html":      indexBody888,
		"hello.txt":       helloBody888,
		".env":            envBody888,
		".hidden/x.txt":   envBody888,
		"sub/index.html":  subBody888,
		"sub/page.txt":    pageBody888,
		"sub/../real.txt": pageBody888,
	} {
		r.write(t, filepath.Join(r.root, filepath.FromSlash(name)), []byte(body), secretTime888)
	}
	r.link(t, filepath.Join(r.root, "hello.txt"), "in-abs")
	r.link(t, "hello.txt", "in-rel")
	r.link(t, filepath.Join("..", "root", "hello.txt"), "in-up") // climbs, through the root's own name
	r.link(t, filepath.Join(r.root, "sub"), "in-dir")
	r.link(t, filepath.Join(r.outside, "secret.txt"), "esc-abs")
	r.link(t, filepath.Join("..", "outside", "secret.txt"), "esc-rel")
	r.link(t, r.outside, "esc-dir")
	r.link(t, filepath.Join("..", "..", "outside", "secret.txt"), filepath.Join("sub", "esc-up"))
	return r
}

// spelling888 is one way to name the tree. url turns a path below the root
// ("/hello.txt") into the request path; chdir is whether the root is the
// working directory.
type spelling888 struct {
	name  string
	root  string
	chdir bool
	url   func(r root888, p string) string
}

func spellings888() []spelling888 {
	rel := func(r root888, p string) string { return p }
	out := []spelling888{
		{"dot", ".", true, rel},
		{"dot-slash", "./", true, rel},
	}
	if runtime.GOOS != "windows" {
		// The filesystem root: the tree is reached by its absolute path.
		out = append(out, spelling888{"slash", "/", false, func(r root888, p string) string {
			return strings.TrimSuffix(filepath.ToSlash(r.root), "/") + p
		}})
	}
	return out
}

// open888 makes the middleware for s, in the right working directory.
func (s spelling888) open888(t *testing.T, r root888, cfg Config) celeris.HandlerFunc {
	t.Helper()
	if s.chdir {
		t.Chdir(r.root)
	}
	cfg.Root = s.root
	return New(cfg)
}

type outcome888 struct {
	status int
	kind   string // "ok", "passed-on", "400", or the other error's text
	body   string
}

func (o outcome888) String() string {
	return fmt.Sprintf("{status %d, %s, body %q}", o.status, o.kind, o.body)
}

func outcomeOf888(t *testing.T, mw celeris.HandlerFunc, path string, hdr ...string) (outcome888, string) {
	t.Helper()
	rec, err := get888(t, mw, path, hdr...)
	o := outcome888{status: rec.StatusCode, body: string(rec.Body)}
	switch {
	case isHTTP400(err):
		o.kind = "400"
	case err != nil:
		o.kind = "error"
	case rec.StatusCode == 0:
		o.kind = "passed-on"
	default:
		o.kind = "ok"
	}
	return o, rec.Header("etag")
}

// TestRootSpellingsServeTheTree888: the files, directories and index files of
// the tree (and its dotfiles, with Dotfiles: Allow) are served under "." and "./"
// (relative to the working directory) and "/" (by absolute path), as under the
// absolute Root.
func TestRootSpellingsServeTheTree888(t *testing.T) {
	cases := []struct {
		path string
		want string // the body of the 200
		dot  bool   // a hidden file: served with Dotfiles: DotfilesAllow, passed on by default
	}{
		{"/", indexBody888, false},
		{"/index.html", indexBody888, false},
		{"/hello.txt", helloBody888, false},
		{"/sub/", subBody888, false},
		{"/sub", subBody888, false},
		{"/sub/page.txt", pageBody888, false},
		{"/./hello.txt", helloBody888, false},
		{"/sub/../hello.txt", helloBody888, false},
		// Hidden files are not served by default (Config.Dotfiles); allowed,
		// they are served under these roots as under any other.
		{"/.env", envBody888, true},
		{"/.hidden/x.txt", envBody888, true},
	}
	for _, sp := range append(spellings888(), spelling888{"absolute", "", false, nil}) {
		for _, tc := range cases {
			t.Run(sp.name+tc.path, func(t *testing.T) {
				r := tree888(t)
				cfg := Config{}
				if tc.dot {
					cfg.Dotfiles = DotfilesAllow
				}
				var mw, def celeris.HandlerFunc
				path := tc.path
				if sp.name == "absolute" {
					cfg.Root = r.root
					mw = New(cfg)
					def = New(Config{Root: r.root})
				} else {
					mw = sp.open888(t, r, cfg)
					def = sp.open888(t, r, Config{})
					path = sp.url(r, tc.path)
				}
				o, _ := outcomeOf888(t, mw, path)
				if o.kind != "ok" || o.status != 200 || o.body != tc.want {
					t.Errorf("GET %s: %v, want a 200 with %q", path, o, tc.want)
				}
				if tc.dot {
					if o, _ := outcomeOf888(t, def, path); o.kind != "passed-on" {
						t.Errorf("GET %s by default: %v, want it passed on (hidden)", path, o)
					}
				}
			})
		}
	}
}

// TestRootSpellingsConditionalRequests888: a file below the root answers a
// matching validator with a 304, an index file too (GET / was a 400 without a
// validator and a 304 with one).
func TestRootSpellingsConditionalRequests888(t *testing.T) {
	for _, sp := range spellings888() {
		for _, tc := range []struct {
			path string
			size int
		}{{"/", len(indexBody888)}, {"/hello.txt", len(helloBody888)}, {"/sub/", len(subBody888)}} {
			t.Run(sp.name+tc.path, func(t *testing.T) {
				r := tree888(t)
				mw := sp.open888(t, r, Config{})
				path := sp.url(r, tc.path)
				o, tag := outcomeOf888(t, mw, path)
				if o.status != 200 || tag != weakETag(secretTime888, int64(tc.size)) {
					t.Fatalf("GET %s: %v, etag %q, want a 200 with the file's tag", path, o, tag)
				}
				o, _ = outcomeOf888(t, mw, path, "if-none-match", tag)
				if o.status != 304 || o.kind != "ok" {
					t.Errorf("GET %s with its tag: %v, want a 304", path, o)
				}
			})
		}
	}
}

// TestRootSpellingsFollowSymlinksInside888: a symlink that stays inside the
// root is served (a file, a directory, a relative and an absolute target),
// with a 304 for its validator.
func TestRootSpellingsFollowSymlinksInside888(t *testing.T) {
	cases := []struct {
		path, want string
		size       int
	}{
		{"/in-abs", helloBody888, len(helloBody888)},
		{"/in-rel", helloBody888, len(helloBody888)},
		{"/in-up", helloBody888, len(helloBody888)},
		{"/in-dir/page.txt", pageBody888, len(pageBody888)},
		{"/in-dir/", subBody888, len(subBody888)},
	}
	for _, sp := range spellings888() {
		for _, tc := range cases {
			t.Run(sp.name+tc.path, func(t *testing.T) {
				r := tree888(t)
				mw := sp.open888(t, r, Config{})
				path := sp.url(r, tc.path)
				o, tag := outcomeOf888(t, mw, path)
				if o.kind != "ok" || o.status != 200 || o.body != tc.want {
					t.Fatalf("GET %s: %v, want a 200 with %q", path, o, tc.want)
				}
				o, _ = outcomeOf888(t, mw, path, "if-none-match", tag)
				if o.status != 304 {
					t.Errorf("GET %s with its tag: %v, want a 304", path, o)
				}
			})
		}
	}
}

// TestRootSpellingsRefuseSymlinksOut888: under "." and "./" a symlink that
// leaves the working directory is refused (400), with and without a validator,
// and the secret never reaches the response. (Under "/" nothing is outside:
// TestRootSlashHasNoOutside888.)
func TestRootSpellingsRefuseSymlinksOut888(t *testing.T) {
	paths := []string{"/esc-abs", "/esc-rel", "/esc-dir/secret.txt", "/sub/esc-up"}
	tag := weakETag(secretTime888, int64(len(secretBytes888)))
	hdrs := [][]string{nil, {"if-none-match", tag}, {"if-none-match", "*"},
		{"if-modified-since", secretTime888.UTC().Format(http.TimeFormat)}}
	for _, sp := range spellings888() {
		if !sp.chdir {
			continue
		}
		for _, p := range paths {
			for _, hdr := range hdrs {
				t.Run(fmt.Sprint(sp.name, p, hdr), func(t *testing.T) {
					r := tree888(t)
					mw := sp.open888(t, r, Config{})
					rec, err := get888(t, mw, p, hdr...)
					if !isHTTP400(err) || rec.StatusCode == 304 || bytes.Contains(rec.Body, secretBytes888) {
						t.Errorf("GET %s %v: status %d err %v body %q, want a 400", p, hdr, rec.StatusCode, err, rec.Body)
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

// TestRootSlashHasNoOutside888 pins what Root "/" is: every path is inside it,
// so a traversal ("..") stops at the filesystem root and a symlink that leaves
// the tree still resolves inside "/". Nothing here is a bypass: the operator
// asked for the whole filesystem (the Config.Root doc says so).
func TestRootSlashHasNoOutside888(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Root \"/\" is the current volume's root on Windows")
	}
	r := tree888(t)
	mw := New(Config{Root: "/"})
	base := strings.TrimSuffix(filepath.ToSlash(r.root), "/")
	for _, p := range []string{
		base + "/esc-abs",
		base + "/esc-dir/secret.txt",
		base + "/../outside/secret.txt",
		strings.Repeat("/..", 64) + base + "/../outside/secret.txt",
	} {
		o, _ := outcomeOf888(t, mw, p)
		if o.kind != "ok" || o.body != string(secretBytes888) {
			t.Errorf("GET %s: %v, want the file (everything is inside \"/\")", p, o)
		}
	}
}

// TestRootSpellingsTraversalMatchesTheAbsoluteRoot888: the paths that try to
// leave the root (".." in the clear and encoded, backslashes, an encoded
// slash, a NUL byte, a doubled slash) get under "." and "./" the answer the
// absolute Root gives, and none of them returns the secret. c.Path() is the
// raw path, so an encoded dot or slash names a file that does not exist.
func TestRootSpellingsTraversalMatchesTheAbsoluteRoot888(t *testing.T) {
	paths := []string{
		"/../outside/secret.txt",
		"/../../outside/secret.txt",
		"/sub/../../outside/secret.txt",
		"/sub/../../../../../../etc/passwd",
		"/..",
		"/../",
		"/%2e%2e/outside/secret.txt",
		"/..%2foutside%2fsecret.txt",
		"/%2e%2e%2foutside%2fsecret.txt",
		"/..%5coutside%5csecret.txt",
		`/..\outside\secret.txt`,
		`/sub\..\..\outside\secret.txt`,
		"//outside/secret.txt",
		"/hello.txt\x00",
		"/\x00/../outside/secret.txt",
		"/hello.txt%00",
		"/sub/%2e%2e/hello.txt",
		"/../etc/passwd",
		"/./../outside/secret.txt",
	}
	for _, sp := range spellings888() {
		if !sp.chdir {
			continue
		}
		for _, p := range paths {
			t.Run(fmt.Sprintf("%s%q", sp.name, p), func(t *testing.T) {
				r := tree888(t)
				want, _ := outcomeOf888(t, New(Config{Root: r.root}), p)
				mw := sp.open888(t, r, Config{})
				got, _ := outcomeOf888(t, mw, p)
				if got != want {
					t.Errorf("GET %q: %v, the absolute Root gives %v", p, got, want)
				}
				if strings.Contains(got.body, string(secretBytes888)) || strings.Contains(got.body, "root:") {
					t.Errorf("GET %q returned content from outside the root: %v", p, got)
				}
				// The same with Browse and SPA, which read the tree too.
				for _, cfg := range []Config{{Browse: true}, {SPA: true}} {
					wantC, _ := outcomeOf888(t, New(Config{Root: r.root, Browse: cfg.Browse, SPA: cfg.SPA}), p)
					gotC, _ := outcomeOf888(t, sp.open888(t, r, cfg), p)
					if gotC != wantC || strings.Contains(gotC.body, string(secretBytes888)) {
						t.Errorf("GET %q (Browse %v, SPA %v): %v, the absolute Root gives %v", p, cfg.Browse, cfg.SPA, gotC, wantC)
					}
				}
			})
		}
	}
}

// TestRootSpellingsBrowseAndSPA888: a directory listing and the SPA fallback
// work below these roots too.
func TestRootSpellingsBrowseAndSPA888(t *testing.T) {
	for _, sp := range spellings888() {
		t.Run(sp.name, func(t *testing.T) {
			r := tree888(t)
			r.write(t, filepath.Join(r.root, "bare", "f.txt"), []byte("f"), secretTime888)
			mw := sp.open888(t, r, Config{Browse: true})
			o, _ := outcomeOf888(t, mw, sp.url(r, "/bare/"))
			if o.status != 200 || !strings.Contains(o.body, "f.txt") {
				t.Errorf("listing of /bare/: %v", o)
			}
			spa := sp.open888(t, r, Config{SPA: true})
			o, _ = outcomeOf888(t, spa, sp.url(r, "/no/such/route"))
			if o.status != 200 && sp.chdir {
				t.Errorf("SPA fallback: %v, want the index", o)
			}
		})
	}
}

// TestRootSpellingsOverTheWire888: the same requests through a server, raw, so
// that nothing cleans the path first.
func TestRootSpellingsOverTheWire888(t *testing.T) {
	for _, sp := range spellings888() {
		t.Run(sp.name, func(t *testing.T) {
			r := tree888(t)
			mw := sp.open888(t, r, Config{})
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			s := celeris.New(celeris.Config{Engine: celeris.Std})
			s.Use(mw)
			done := make(chan error, 1)
			go func() { done <- s.StartWithListener(ln) }()
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = s.Shutdown(ctx)
				if err := <-done; err != nil {
					t.Logf("Start returned %v", err)
				}
			})
			raw := func(path string) string {
				var lastErr error
				for range 100 {
					c, err := net.Dial("tcp", ln.Addr().String())
					if err != nil {
						lastErr = err
						time.Sleep(20 * time.Millisecond)
						continue
					}
					defer func() { _ = c.Close() }()
					_ = c.SetDeadline(time.Now().Add(10 * time.Second))
					_, _ = fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n", path)
					b, _ := io.ReadAll(c)
					return string(b)
				}
				t.Fatalf("cannot connect: %v", lastErr)
				return ""
			}
			if got := raw(sp.url(r, "/hello.txt")); !strings.HasPrefix(got, "HTTP/1.1 200") || !strings.HasSuffix(got, helloBody888) {
				t.Errorf("GET %s: %q, want a 200 with %q", sp.url(r, "/hello.txt"), got, helloBody888)
			}
			if got := raw(sp.url(r, "/sub/page.txt")); !strings.HasPrefix(got, "HTTP/1.1 200") || !strings.HasSuffix(got, pageBody888) {
				t.Errorf("GET /sub/page.txt: %q", got)
			}
			if !sp.chdir {
				return
			}
			for _, p := range []string{"/../outside/secret.txt", "/%2e%2e/outside/secret.txt", "/..%2foutside%2fsecret.txt",
				"/sub/%2e%2e/%2e%2e/outside/secret.txt", `/..\outside\secret.txt`, "/esc-abs", "/esc-dir/secret.txt"} {
				got := raw(p)
				if strings.Contains(got, string(secretBytes888)) || strings.HasPrefix(got, "HTTP/1.1 200") {
					t.Errorf("GET %s answered %q: content from outside the root", p, got)
				}
			}
		})
	}
}

// TestRootSpellingsBrowseDoesNotListASymlinkedDirOut888: a directory listing
// is refused for a symlink to a directory outside the root (the recheck in
// serveOS), whichever way the root is spelled, and a symlink to a directory
// inside is listed.
func TestRootSpellingsBrowseDoesNotListASymlinkedDirOut888(t *testing.T) {
	for _, sp := range spellings888() {
		if !sp.chdir {
			continue
		}
		t.Run(sp.name, func(t *testing.T) {
			r := tree888(t)
			r.write(t, filepath.Join(r.root, "plain", "listed.txt"), []byte("x"), secretTime888)
			r.link(t, "plain", "in-plain")
			mw := sp.open888(t, r, Config{Browse: true})
			o, _ := outcomeOf888(t, mw, "/esc-dir/")
			if o.status == 200 || strings.Contains(o.body, "secret.txt") {
				t.Errorf("GET /esc-dir/: %v, want the listing of a directory outside the root refused", o)
			}
			o, _ = outcomeOf888(t, mw, "/in-plain/")
			if o.status != 200 || !strings.Contains(o.body, "listed.txt") {
				t.Errorf("GET /in-plain/: %v, want its listing", o)
			}
		})
	}
}

// TestRootSpellingsHeadAndRange888: HEAD and a byte range below "." , "./" and
// "/" are answered as under any Root (the recorder keeps a HEAD's body; the
// engine drops it on the wire).
func TestRootSpellingsHeadAndRange888(t *testing.T) {
	for _, sp := range spellings888() {
		t.Run(sp.name, func(t *testing.T) {
			r := tree888(t)
			mw := sp.open888(t, r, Config{})
			path := sp.url(r, "/hello.txt")
			ctx, rec := celeristest.NewContextT(t, "HEAD", path, celeristest.WithHandlers(mw))
			if err := ctx.Next(); err != nil || rec.StatusCode != 200 || rec.Header("content-length") != fmt.Sprint(len(helloBody888)) {
				t.Errorf("HEAD %s: status %d, content-length %q, err %v, want 200 and %d", path, rec.StatusCode, rec.Header("content-length"), err, len(helloBody888))
			}
			rg, err := get888(t, mw, path, "range", "bytes=0-4")
			if err != nil || rg.StatusCode != 206 || string(rg.Body) != "hello" {
				t.Errorf("GET %s bytes=0-4: status %d body %q err %v, want 206 %q", path, rg.StatusCode, rg.Body, err, "hello")
			}
			ri, err := get888(t, mw, sp.url(r, "/"), "range", "bytes=0-2")
			if err != nil || ri.StatusCode != 206 || string(ri.Body) != indexBody888[:3] {
				t.Errorf("GET / bytes=0-2: status %d body %q err %v, want 206 %q", ri.StatusCode, ri.Body, err, indexBody888[:3])
			}
		})
	}
}
