package static

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// A Root that is made of ".." segments names the directory it resolves to, and
// nothing outside that directory is served (celeris#888 item 46, review of the
// fix for the "." roots). PathWithin special-cased "." and not "..": for a base
// of "..", "../../etc/passwd" starts with ".." and with the separator after it,
// so it was "within", and a Root of ".." or "../.." served every file the
// process could read, by a request that climbs ("GET /../../TOPSECRET.txt").
// Server.Static and Group.Static serve through Context.FileFromDir, which has
// the same check. It is on main since v1.3.3.
//
// The tree (T is a temp dir; the working directory is T/p/c):
//
//	T/../ABOVE.txt  T/TOPSECRET.txt  T/p/PSECRET.txt  T/p/other/o.txt
//	T/p/c/{index.html,a.txt,.env}  T/p/c/sub/{index.html,b.txt}  T/p/c/x/
//
// Every file has its own marker as content. For each root spelling the
// directory R it resolves to is computed independently of the code under test
// (the working directory joined with the root); then, for every file F of the
// tree, the request "/<F relative to R>" must serve F when F is inside R, and
// must serve nothing when it is outside (the relative path then climbs: that is
// the request the exploit sends). Further requests that try to leave R (extra
// climbs, encodings, backslashes, doubled slashes, absolute paths, NUL) must
// return no marker of a file outside R.

type dotTree888 struct {
	top, cwd string            // T (physical) and the working directory T/p/c
	files    map[string]string // absolute path -> marker
}

func newDotTree888(t *testing.T) dotTree888 {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("slash paths and Root \"/\": not run on Windows")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	top := filepath.Join(base, "top")
	d := dotTree888{top: top, cwd: filepath.Join(top, "p", "c"), files: map[string]string{}}
	for _, f := range []string{
		"../ABOVE.txt", "TOPSECRET.txt", "p/PSECRET.txt", "p/other/o.txt",
		"p/c/index.html", "p/c/a.txt", "p/c/.env", "p/c/sub/index.html", "p/c/sub/b.txt",
	} {
		abs := filepath.Join(top, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		marker := "MARK<" + f + ">"
		if err := os.WriteFile(abs, []byte(marker), 0o644); err != nil {
			t.Fatal(err)
		}
		d.files[abs] = marker
	}
	if err := os.MkdirAll(filepath.Join(d.cwd, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(d.cwd)
	return d
}

// rootSpelling888 is a way to write a Root (or a FileFromDir base): the string,
// and the directory it resolves to.
type rootSpelling888 struct{ root, resolves string }

func (d dotTree888) spellings() []rootSpelling888 {
	abs := func(rel string) string { return filepath.Join(d.cwd, rel) }
	return []rootSpelling888{
		{".", d.cwd}, {"./", d.cwd}, {"x/..", d.cwd}, {"sub/..", d.cwd}, {"./sub/../", d.cwd},
		{"..", abs("..")}, {"../", abs("..")}, {"./..", abs("..")}, {"x/../..", abs("..")}, {"a/../..", abs("..")},
		{"../..", abs("../..")}, {"../../", abs("../..")}, {"../../..", abs("../../..")},
		{"../x/..", abs("..")}, {"../c", d.cwd}, {"../other", abs("../other")},
		{"sub", abs("sub")}, {"sub/", abs("sub")},
		{d.cwd, d.cwd}, {d.cwd + "/", d.cwd}, {filepath.Join(d.cwd, ".."), abs("..")},
		{"/", "/"},
	}
}

func isUnder888(root, p string) bool {
	return root == "/" || p == root || strings.HasPrefix(p, root+"/")
}

// requests888 are the requests for the spelling: the plain ones, with the
// expectation (a file inside R must be served), and the hostile ones.
func (d dotTree888) requests(sp rootSpelling888) (plain []req888, hostile []string) {
	paths := make([]string, 0, len(d.files))
	for f := range d.files {
		paths = append(paths, f)
	}
	sort.Strings(paths)
	for _, f := range paths {
		var u string
		if sp.root == "/" {
			u = f // by absolute path
		} else {
			rel, err := filepath.Rel(sp.resolves, f)
			if err != nil {
				continue
			}
			u = "/" + filepath.ToSlash(rel)
		}
		plain = append(plain, req888{path: u, file: f, inside: isUnder888(sp.resolves, f)})
		if sp.root != "/" {
			rel := strings.TrimPrefix(u, "/")
			hostile = append(hostile,
				"/./"+rel, "/"+rel+"/", "//"+rel, "/x/../"+rel, "/../../../../../../"+rel,
				"/%2e%2e/"+strings.TrimPrefix(rel, "../"), "/..%2f"+strings.TrimPrefix(rel, "../"),
				"/"+strings.ReplaceAll(rel, "/", `\`), "/"+rel+"\x00", "/"+rel+"%00")
		}
		hostile = append(hostile, "/"+filepath.ToSlash(f), f)
	}
	hostile = append(hostile, "/..", "/../", "/../..", "/../../..", "/sub/../..", "/.%00/a.txt", "/\x00", "//", "/../../../../etc/passwd")
	return plain, hostile
}

type req888 struct {
	path   string
	file   string
	inside bool
}

// verdict888 checks one answer. body is "" when nothing was written.
func (d dotTree888) verdict(t *testing.T, label string, sp rootSpelling888, r req888, hostile bool, status int, body string) {
	t.Helper()
	for f, m := range d.files {
		if !isUnder888(sp.resolves, f) && strings.Contains(body, m) {
			t.Errorf("%s: root %q (resolves to %s) GET %q returned %s's content %q: outside the root", label, sp.root, sp.resolves, r.path, f, m)
		}
	}
	if hostile {
		return
	}
	if r.inside {
		if status != 200 || body != d.files[r.file] {
			t.Errorf("%s: root %q (resolves to %s) GET %q: status %d body %q, want 200 %q", label, sp.root, sp.resolves, r.path, status, body, d.files[r.file])
		}
	} else if status == 200 {
		t.Errorf("%s: root %q (resolves to %s) GET %q answered 200 for %s, outside the root", label, sp.root, sp.resolves, r.path, r.file)
	}
}

func eachRequest888(d dotTree888, sp rootSpelling888, fn func(path string, r req888, hostile bool)) {
	plain, hostile := d.requests(sp)
	for _, r := range plain {
		fn(r.path, r, false)
	}
	for _, h := range hostile {
		fn(h, req888{path: h}, true)
	}
}

// TestDotDotRootsMiddleware888: the static middleware, every spelling.
func TestDotDotRootsMiddleware888(t *testing.T) {
	d := newDotTree888(t)
	for _, sp := range d.spellings() {
		mw := New(Config{Root: sp.root})
		eachRequest888(d, sp, func(path string, r req888, hostile bool) {
			rec, _ := get888(t, mw, path)
			d.verdict(t, "static", sp, r, hostile, rec.StatusCode, string(rec.Body))
		})
	}
}

// TestDotDotRootsFileFromDir888: Context.FileFromDir, which Server.Static and
// Group.Static call with the route's tail (no leading slash).
func TestDotDotRootsFileFromDir888(t *testing.T) {
	d := newDotTree888(t)
	for _, sp := range d.spellings() {
		eachRequest888(d, sp, func(path string, r req888, hostile bool) {
			c, rec := celeristest.NewContext("GET", "/f")
			defer celeristest.ReleaseContext(c)
			_ = c.FileFromDir(sp.root, strings.TrimPrefix(path, "/"))
			d.verdict(t, "FileFromDir", sp, r, hostile, rec.StatusCode, string(rec.Body))
		})
	}
}

// wire888 starts a server, registers with reg, and returns a function that
// sends a raw request line (nothing cleans the path) and returns the status
// and body. The server is stopped with the test.
func wire888(t *testing.T, eng celeris.EngineType, reg func(s *celeris.Server)) func(path string) (int, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := celeris.New(celeris.Config{Engine: eng})
	reg(s)
	done := make(chan error, 1)
	go func() { done <- s.StartWithListener(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		<-done
	})
	do := func(path string) (int, string) {
		var lastErr error
		for range 200 {
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
			resp := string(b)
			var status int
			_, _ = fmt.Sscanf(resp, "HTTP/1.1 %d", &status)
			body := ""
			if i := strings.Index(resp, "\r\n\r\n"); i >= 0 {
				body = resp[i+4:]
			}
			return status, body
		}
		t.Fatalf("cannot connect: %v", lastErr)
		return 0, ""
	}
	// The server is ready when something answers: a native engine closes the
	// listener it was given and rebinds it, so an early connection is dropped.
	for deadline := time.Now().Add(30 * time.Second); ; {
		if status, _ := do("/ready-probe"); status != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the server did not answer in 30 s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return do
}

// wireEngines888 is the engines the wire tests run on; an epoll engine is added
// on Linux (wire_engines_888_linux_test.go).
var wireEngines888 = []celeris.EngineType{celeris.Std}

// TestDotDotRootsOverTheWire888: the three entry points a user mounts, on the
// real engines: the static middleware, Server.Static and Group.Static.
func TestDotDotRootsOverTheWire888(t *testing.T) {
	d := newDotTree888(t)
	type entry struct {
		name   string
		prefix string
		reg    func(root string) func(s *celeris.Server)
	}
	entries := []entry{
		{"middleware", "", func(root string) func(s *celeris.Server) {
			return func(s *celeris.Server) { s.Use(New(Config{Root: root})) }
		}},
		{"Server.Static", "/files", func(root string) func(s *celeris.Server) {
			return func(s *celeris.Server) { s.Static("/files", root) }
		}},
		{"Group.Static", "/g/files", func(root string) func(s *celeris.Server) {
			return func(s *celeris.Server) { s.Group("/g").Static("/files", root) }
		}},
	}
	for _, eng := range wireEngines888 {
		for _, e := range entries {
			for _, sp := range d.spellings() {
				t.Run(fmt.Sprintf("%v/%s/%s", eng, e.name, sp.root), func(t *testing.T) {
					raw := wire888(t, eng, e.reg(sp.root))
					eachRequest888(d, sp, func(path string, r req888, hostile bool) {
						status, body := raw(e.prefix + path)
						d.verdict(t, e.name, sp, r, hostile, status, body)
					})
				})
			}
		}
	}
}
