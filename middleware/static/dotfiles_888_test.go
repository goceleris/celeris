package static

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// The default dotfile rule (celeris#888 item 46, the decision of the maintainer
// after the review of Root "."): a request whose path below the root has a
// segment that starts with a dot is answered as if the file did not exist
// (DotfilesIgnore, the default), refused with 403 (DotfilesDeny), or served
// (DotfilesAllow). ".well-known", exactly, is not a dot segment. Only the
// segments of the request count, never the name of the root.

const (
	tagIndex888 = "INDEX"
	tagWK888    = "ACME"
)

// dotTree writes the tree of these tests below a new temp dir and returns it.
//
//	index.html a.txt a.env .env .htaccess .git/config .ssh/id dir.with.dots/z.txt
//	sub/{index.html,b.txt,.hidden,.d/inner.txt}  d/.index.html  bare/{.a,b}
//	.well-known/{acme.txt,security.txt,.secret,.sub/x.txt,child/y.txt}
//	.well-known-x/z.txt  .well-knownx
//	link-env -> .env  link-dir -> .git  link-wk -> .well-known  .dot-link -> a.txt
var dotFiles888 = map[string]string{
	"index.html": tagIndex888, "a.txt": "A", "a.env": "AENV", ".env": "ENV", ".htaccess": "HTA",
	".git/config": "GITCFG", ".ssh/id": "SSH", "dir.with.dots/z.txt": "DOTS",
	"sub/index.html": "SUBIDX", "sub/b.txt": "B", "sub/.hidden": "SUBHID", "sub/.d/inner.txt": "INNER",
	"d/.index.html": "DOTINDEX", "bare/.a": "BA", "bare/b": "BB",
	".well-known/acme.txt": tagWK888, ".well-known/security.txt": "SEC", ".well-known/.secret": "WKSECRET",
	".well-known/.sub/x.txt": "WKSUB", ".well-known/child/y.txt": "WKCHILD",
	".well-known-x/z.txt": "WKX", ".well-knownx": "WKXFILE",
}

func dotTree(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range dotFiles888 {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, secretTime888, secretTime888); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{"link-env": ".env", "link-dir": ".git", "link-wk": ".well-known", ".dot-link": "a.txt"} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Skipf("symlinks are not available here: %v", err)
		}
	}
	return root
}

func skipWindows888(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("slash paths")
	}
}

type dotCase888 struct {
	path   string
	hidden bool   // answered as hidden under Ignore and Deny
	body   string // the body when the path is served ("" when no file is there to serve)
}

// dotCases888 are the requests of the policy tests, with the Root of dotTree.
var dotCases888 = []dotCase888{
	// hidden
	{"/.env", true, "ENV"}, {"/.htaccess", true, "HTA"}, {"/.git/config", true, "GITCFG"}, {"/.ssh/id", true, "SSH"},
	{"/sub/.hidden", true, "SUBHID"}, {"/sub/.d/inner.txt", true, "INNER"}, {"/.git/", true, ""}, {"/.git", true, ""},
	{"/.well-known/.secret", true, "WKSECRET"}, {"/.well-known/.sub/x.txt", true, "WKSUB"},
	{"/.well-known-x/z.txt", true, "WKX"}, {"/.well-knownx", true, "WKXFILE"}, {"/.well-known.txt", true, ""},
	{"/.Well-Known/acme.txt", true, ""}, {"/.WELL-KNOWN/acme.txt", true, ""},
	{"/.nothing-here", true, ""}, {"/..env", true, ""}, {"/...", true, ""},
	// the same names reached through a path that cleans to them
	{"/./.env", true, "ENV"}, {"//.env", true, "ENV"}, {"/sub/../.env", true, "ENV"}, {"/x/../.env", true, "ENV"},
	{"/a.txt/../.env", true, "ENV"}, {"/.well-known/../.env", true, "ENV"}, {"/.env/", true, "ENV"},
	{"/sub/./.hidden", true, "SUBHID"}, {"/.well-known/./.secret", true, "WKSECRET"}, {"/sub/.d/../.hidden", true, "SUBHID"},
	// backslashes separate segments on Windows, so they do here
	{`/\.env`, true, ""}, {`/sub\.hidden`, true, ""}, {`/.well-known\.secret`, true, ""},
	// encoded: the path is the raw one, so "%2e" names no dotfile on its own; a
	// segment that decodes to a dot name is hidden all the same, and a double
	// encoding is decoded once only (it is a file named "%2eenv": none)
	{"/%2eenv", true, ""}, {"/%2Eenv", true, ""}, {"/%2e%2eenv", true, ""}, {"/sub/%2ehidden", true, ""},
	{"/.well-known/%2esecret", true, ""}, {"/%2e%2e/.env", true, ""}, {"/%2e", true, ""}, {"/%2e%2e", true, ""},
	{"/%252eenv", false, ""}, {"/%252e%252e/.env", true, ""},
	{"/.env\x00", true, ""}, {"/.env%00", true, ""},
	// symlinks: a visible name that leads to a dotfile or directory of the tree, and a hidden name
	{"/link-env", true, "ENV"}, {"/link-dir/config", true, "GITCFG"}, {"/.dot-link", true, "A"},
	// visible
	{"/", false, tagIndex888}, {"/index.html", false, tagIndex888}, {"/a.txt", false, "A"}, {"/a.env", false, "AENV"},
	{"/sub/", false, "SUBIDX"}, {"/sub", false, "SUBIDX"}, {"/sub/b.txt", false, "B"},
	{"/dir.with.dots/z.txt", false, "DOTS"}, {"/./a.txt", false, "A"}, {"//a.txt", false, "A"}, {"/sub/../a.txt", false, "A"},
	// ".well-known", exactly, and its children that are not themselves hidden
	{"/.well-known/acme.txt", false, tagWK888}, {"/.well-known/security.txt", false, "SEC"},
	{"/.well-known/child/y.txt", false, "WKCHILD"}, {"/.well-known/./acme.txt", false, tagWK888},
	{"/link-wk/acme.txt", false, tagWK888}, {"/link-wk/child/y.txt", false, "WKCHILD"},
	// ".git/../a.txt" cleans to "a.txt", which is what is opened
	{"/.git/../a.txt", false, "A"}, {"/.well-known/../a.txt", false, "A"},
}

// missing888 is the outcome of a path that does not exist: what a hidden path
// is answered with under DotfilesIgnore.
func missing888(t *testing.T, mw celeris.HandlerFunc) outcome888 {
	t.Helper()
	o, _ := outcomeOf888(t, mw, "/no-such-file-here.txt")
	if o.kind != "passed-on" {
		t.Fatalf("harness: a missing path gives %v, want it passed on", o)
	}
	return o
}

// TestDotfilesPolicies888: every request of the table under the three policies.
func TestDotfilesPolicies888(t *testing.T) {
	skipWindows888(t)
	root := dotTree(t)
	for _, pol := range []DotfilesPolicy{DotfilesIgnore, DotfilesDeny, DotfilesAllow} {
		mw := New(Config{Root: root, Dotfiles: pol})
		absent := missing888(t, mw)
		for _, tc := range dotCases888 {
			t.Run(fmt.Sprintf("policy%d/%q", pol, tc.path), func(t *testing.T) {
				rec, err := get888(t, mw, tc.path)
				o := outcome888{status: rec.StatusCode, body: string(rec.Body), kind: "ok"}
				switch {
				case err != nil:
					o.kind = "error"
				case rec.StatusCode == 0:
					o.kind = "passed-on"
				}
				if err != nil {
					if he, ok := err.(*celeris.HTTPError); ok {
						o.status = he.Code
					}
				}
				switch {
				case pol == DotfilesAllow || !tc.hidden:
					if tc.body == "" {
						// (a case-insensitive file system finds ".Well-Known" under Allow)
						if o.kind == "ok" && !strings.Contains(strings.ToLower(tc.path), ".well-known/acme") {
							t.Errorf("GET %q under policy %d answered %v for a file that is not there", tc.path, pol, o)
						}
					} else if o.status != 200 || o.body != tc.body {
						t.Errorf("GET %q under policy %d: %v, want 200 %q", tc.path, pol, o, tc.body)
					}
				case pol == DotfilesIgnore:
					if o != absent {
						t.Errorf("GET %q hidden under Ignore: %v, want what a missing file gets: %v", tc.path, o, absent)
					}
				default: // Deny
					if o.kind != "error" || o.status != 403 || o.body != "" {
						t.Errorf("GET %q hidden under Deny: %v, want a 403", tc.path, o)
					}
				}
				if tc.hidden && pol != DotfilesAllow {
					if tc.body != "" && strings.Contains(o.body, tc.body) {
						t.Errorf("GET %q leaked %q: %v", tc.path, tc.body, o)
					}
					for _, h := range []string{"etag", "last-modified", "content-type", "content-length"} {
						if v := rec.Header(h); v != "" {
							t.Errorf("GET %q: header %s %q on a hidden path", tc.path, h, v)
						}
					}
				}
			})
		}
	}
}

// TestDotfilesHeadRangeConditional888: HEAD, Range and a matching validator on a
// hidden file behave as on a path that is not there (Ignore: passed on, Deny:
// 403): no 304, no 206, no validators.
func TestDotfilesHeadRangeConditional888(t *testing.T) {
	skipWindows888(t)
	root := dotTree(t)
	tag := weakETag(secretTime888, int64(len("ENV")))
	for _, pol := range []DotfilesPolicy{DotfilesIgnore, DotfilesDeny} {
		mw := New(Config{Root: root, Dotfiles: pol})
		for _, p := range []string{"/.env", "/link-env", "/.git/config"} {
			for _, hdr := range [][]string{{"range", "bytes=0-1"}, {"if-none-match", tag}, {"if-none-match", "*"},
				{"if-modified-since", secretTime888.UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")}, {"if-range", tag, "range", "bytes=0-1"}} {
				rec, err := get888(t, mw, p, hdr...)
				if rec.StatusCode == 206 || rec.StatusCode == 304 || rec.StatusCode == 200 || len(rec.Body) != 0 {
					t.Errorf("policy %d GET %s %v: status %d body %q err %v", pol, p, hdr, rec.StatusCode, rec.Body, err)
				}
				if pol == DotfilesDeny && !(err != nil && err.(*celeris.HTTPError).Code == 403) {
					t.Errorf("policy %d GET %s %v: err %v, want a 403", pol, p, hdr, err)
				}
				if pol == DotfilesIgnore && err != nil {
					t.Errorf("policy %d GET %s %v: err %v, want it passed on", pol, p, hdr, err)
				}
				for _, h := range []string{"etag", "last-modified", "content-range", "accept-ranges"} {
					if v := rec.Header(h); v != "" {
						t.Errorf("policy %d GET %s %v: header %s %q", pol, p, hdr, h, v)
					}
				}
			}
			ctx, rec := celeristest.NewContextT(t, "HEAD", p, celeristest.WithHandlers(mw))
			err := ctx.Next()
			if rec.StatusCode == 200 || rec.Header("content-length") != "" || rec.Header("etag") != "" {
				t.Errorf("policy %d HEAD %s: status %d content-length %q etag %q err %v", pol, p, rec.StatusCode, rec.Header("content-length"), rec.Header("etag"), err)
			}
		}
	}
}

// TestDotfilesRootNameIsNotHidden888: only the segments of the request count.
// A root that is, or sits in, a directory whose name starts with a dot serves
// its files: the working directory of a ".config" directory under ".", "./" and
// "..", an absolute root with a dot in every element, a Prefix that starts with
// a dot.
func TestDotfilesRootNameIsNotHidden888(t *testing.T) {
	skipWindows888(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hiddenRoot := filepath.Join(base, ".hidden-parent", ".config")
	for _, f := range []string{"a.txt", "sub/b.txt", ".env"} {
		p := filepath.Join(hiddenRoot, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("body:"+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	inner := filepath.Join(hiddenRoot, "sub")
	for _, tc := range []struct {
		name, root, cwd string
	}{
		{"absolute", hiddenRoot, ""},
		{"absolute-trailing-slash", hiddenRoot + "/", ""},
		{"dot", ".", hiddenRoot},
		{"dot-slash", "./", hiddenRoot},
		{"dotdot", "..", inner},
		{"relative-named", ".config", filepath.Dir(hiddenRoot)},
		{"relative-dotted-path", "./.hidden-parent/.config", base},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.cwd != "" {
				t.Chdir(tc.cwd)
			}
			mw := New(Config{Root: tc.root})
			for _, p := range []string{"/a.txt", "/sub/b.txt"} {
				o, _ := outcomeOf888(t, mw, p)
				want := "body:" + strings.TrimPrefix(p, "/")
				if o.status != 200 || o.body != want {
					t.Errorf("Root %q GET %s: %v, want 200 %q (the root's own name is not hidden)", tc.root, p, o, want)
				}
			}
			if o, _ := outcomeOf888(t, mw, "/.env"); o.status == 200 {
				t.Errorf("Root %q GET /.env: %v, want the dotfile below the root hidden", tc.root, o)
			}
		})
	}
	t.Run("prefix", func(t *testing.T) {
		mw := New(Config{Root: hiddenRoot, Prefix: "/.assets"})
		if o, _ := outcomeOf888(t, mw, "/.assets/a.txt"); o.status != 200 || o.body != "body:a.txt" {
			t.Errorf("Prefix /.assets: %v, want 200", o)
		}
		if o, _ := outcomeOf888(t, mw, "/.assets/.env"); o.status == 200 {
			t.Errorf("Prefix /.assets, /.env below: %v, want hidden", o)
		}
	})
}

// TestDotfilesIndex888: Index is chosen by the operator and is never hidden by
// the rule, so ".index.html" is served for a directory request; the same name
// asked for by its path is a dot segment and is hidden.
func TestDotfilesIndex888(t *testing.T) {
	skipWindows888(t)
	root := dotTree(t)
	for _, pol := range []DotfilesPolicy{DotfilesIgnore, DotfilesDeny} {
		mw := New(Config{Root: root, Index: ".index.html", Dotfiles: pol})
		if o, _ := outcomeOf888(t, mw, "/d/"); o.status != 200 || o.body != "DOTINDEX" {
			t.Errorf("policy %d GET /d/ with Index .index.html: %v, want 200 DOTINDEX", pol, o)
		}
		rec, err := get888(t, mw, "/d/.index.html")
		if rec.StatusCode == 200 || strings.Contains(string(rec.Body), "DOTINDEX") {
			t.Errorf("policy %d GET /d/.index.html: status %d body %q err %v, want hidden", pol, rec.StatusCode, rec.Body, err)
		}
	}
	// SPA fallback with the dot-named Index.
	spa := New(Config{Root: root, Index: "d/.index.html", SPA: true})
	if o, _ := outcomeOf888(t, spa, "/nothing/here"); o.status != 200 || o.body != "DOTINDEX" {
		t.Errorf("SPA with a dot-named Index: %v, want 200 DOTINDEX", o)
	}
}

// TestDotfilesBrowse888: a listing leaves out the entries that start with a dot
// (but not ".well-known") under Ignore and Deny, and lists them under Allow; a
// hidden directory is not listed at all.
func TestDotfilesBrowse888(t *testing.T) {
	skipWindows888(t)
	root := dotTree(t)
	for _, pol := range []DotfilesPolicy{DotfilesIgnore, DotfilesDeny, DotfilesAllow} {
		mw := New(Config{Root: root, Dotfiles: pol, Browse: true, Index: "no-index.here"})
		o, _ := outcomeOf888(t, mw, "/")
		for _, name := range []string{".env", ".git", ".ssh", ".htaccess", ".well-knownx", ".dot-link"} {
			has := strings.Contains(o.body, ">"+name)
			if has != (pol == DotfilesAllow) {
				t.Errorf("policy %d: listing of / has %q = %v, want %v", pol, name, has, pol == DotfilesAllow)
			}
		}
		for _, name := range []string{"a.txt", ".well-known/", "dir.with.dots/", "sub/"} {
			if !strings.Contains(o.body, ">"+name) {
				t.Errorf("policy %d: listing of / lacks %q", pol, name)
			}
		}
		o, _ = outcomeOf888(t, mw, "/bare/")
		if has := strings.Contains(o.body, ">.a"); has != (pol == DotfilesAllow) || !strings.Contains(o.body, ">b") {
			t.Errorf("policy %d: listing of /bare/ = %q", pol, o.body)
		}
		for _, p := range []string{"/.git/", "/sub/.d/", "/.well-known/.sub/"} {
			rec, err := get888(t, mw, p)
			listed := rec.StatusCode == 200
			if listed != (pol == DotfilesAllow) {
				t.Errorf("policy %d GET %s: status %d err %v, listed %v", pol, p, rec.StatusCode, err, listed)
			}
		}
		// a visible name that leads to a hidden directory is not listed either
		if rec, _ := get888(t, mw, "/link-dir/"); (rec.StatusCode == 200) != (pol == DotfilesAllow) {
			t.Errorf("policy %d GET /link-dir/: status %d", pol, rec.StatusCode)
		}
		if rec, _ := get888(t, mw, "/.well-known/"); rec.StatusCode != 200 || !strings.Contains(string(rec.Body), ">acme.txt") || strings.Contains(string(rec.Body), ">.secret") != (pol == DotfilesAllow) {
			t.Errorf("policy %d GET /.well-known/: status %d body %q", pol, rec.StatusCode, rec.Body)
		}
	}
}

// TestDotfilesSPA888: under Ignore a hidden path is answered as a missing one
// is, which in SPA mode is the index file; Deny refuses it.
func TestDotfilesSPA888(t *testing.T) {
	skipWindows888(t)
	root := dotTree(t)
	ignore := New(Config{Root: root, SPA: true})
	for _, p := range []string{"/.env", "/.git/config", "/sub/.hidden", "/link-env", "/missing"} {
		if o, _ := outcomeOf888(t, ignore, p); o.status != 200 || o.body != tagIndex888 {
			t.Errorf("SPA Ignore GET %s: %v, want the index (as for a missing path)", p, o)
		}
	}
	deny := New(Config{Root: root, SPA: true, Dotfiles: DotfilesDeny})
	rec, err := get888(t, deny, "/.env")
	if !(err != nil && err.(*celeris.HTTPError).Code == 403) || len(rec.Body) != 0 {
		t.Errorf("SPA Deny GET /.env: status %d body %q err %v, want 403", rec.StatusCode, rec.Body, err)
	}
	allow := New(Config{Root: root, SPA: true, Dotfiles: DotfilesAllow})
	if o, _ := outcomeOf888(t, allow, "/.env"); o.status != 200 || o.body != "ENV" {
		t.Errorf("SPA Allow GET /.env: %v, want ENV", o)
	}
}

// TestDotfilesSymlinks888: the rule looks at the request path and at where the
// path leads once its symlinks are resolved: a visible name that links to a
// dotfile, or to a hidden directory, of the tree is refused like the dotfile; a
// link to ".well-known" is served; a hidden name is hidden whatever it links to.
// Under Allow they are all served.
func TestDotfilesSymlinks888(t *testing.T) {
	skipWindows888(t)
	root := dotTree(t)
	tag := weakETag(secretTime888, int64(len("ENV")))
	for _, pol := range []DotfilesPolicy{DotfilesIgnore, DotfilesDeny, DotfilesAllow} {
		mw := New(Config{Root: root, Dotfiles: pol})
		for _, p := range []string{"/link-env", "/link-dir/config", "/.dot-link"} {
			rec, err := get888(t, mw, p)
			served := rec.StatusCode == 200
			if served != (pol == DotfilesAllow) {
				t.Errorf("policy %d GET %s: status %d body %q err %v, served %v", pol, p, rec.StatusCode, rec.Body, err, served)
			}
			// a conditional request is no oracle either
			rec, _ = get888(t, mw, p, "if-none-match", tag)
			if (rec.StatusCode == 304 || rec.StatusCode == 200) != (pol == DotfilesAllow) {
				t.Errorf("policy %d GET %s with If-None-Match: status %d", pol, p, rec.StatusCode)
			}
			if pol != DotfilesAllow && (rec.Header("etag") != "" || rec.Header("last-modified") != "") {
				t.Errorf("policy %d GET %s with If-None-Match: validators on a refused request", pol, p)
			}
		}
		if o, _ := outcomeOf888(t, mw, "/link-wk/acme.txt"); o.status != 200 || o.body != tagWK888 {
			t.Errorf("policy %d GET /link-wk/acme.txt: %v, want 200", pol, o)
		}
	}
}

// TestDotfilesFS888: the same rule for Config.FS.
func TestDotfilesFS888(t *testing.T) {
	m := fstest.MapFS{}
	for name, body := range dotFiles888 {
		m[name] = &fstest.MapFile{Data: []byte(body), ModTime: secretTime888}
	}
	for _, pol := range []DotfilesPolicy{DotfilesIgnore, DotfilesDeny, DotfilesAllow} {
		mw := New(Config{FS: m, Dotfiles: pol})
		absent := missing888(t, mw)
		for _, tc := range dotCases888 {
			if strings.Contains(tc.path, "link") || strings.Contains(tc.path, `\`) || strings.Contains(tc.path, "\x00") || strings.Contains(tc.path, "%00") || strings.HasPrefix(tc.path, "/.dot") {
				continue // symlinks and the OS-only rows
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
					// fs.FS names hold no "..", "." or empty element: those rows only check the rule
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
		// listing
		bw := New(Config{FS: m, Dotfiles: pol, Browse: true, Index: "no-index.here"})
		o, _ := outcomeOf888(t, bw, "/")
		if has := strings.Contains(o.body, ">.env"); has != (pol == DotfilesAllow) {
			t.Errorf("FS policy %d: listing has .env = %v", pol, has)
		}
		if !strings.Contains(o.body, ">.well-known/") || !strings.Contains(o.body, ">a.txt") {
			t.Errorf("FS policy %d: listing of / = %q", pol, o.body)
		}
	}
}

// TestDotfilesOverTheWire888: the default rule on the real engines, for the
// middleware and for Server.Static and Group.Static, which serve a directory
// too: a hidden path is a 404 with nothing of the file in it; ".well-known" and
// the visible files are served.
func TestDotfilesOverTheWire888(t *testing.T) {
	skipWindows888(t)
	root := dotTree(t)
	hidden := []string{"/.env", "/.git/config", "/sub/.hidden", "/.well-known/.secret", "/sub/../.env", "/%2eenv", "/%2E%2e/.env",
		"/link-env", "/link-dir/config", "/.ssh/id", "/.htaccess", "//.env", "/./.env", `/\.env`}
	visible := map[string]string{"/a.txt": "A", "/sub/b.txt": "B", "/.well-known/acme.txt": tagWK888, "/.well-known/child/y.txt": "WKCHILD", "/dir.with.dots/z.txt": "DOTS"}
	type entry struct {
		name, prefix string
		reg          func(s *celeris.Server)
	}
	for _, eng := range wireEngines888 {
		for _, e := range []entry{
			{"middleware", "", func(s *celeris.Server) { s.Use(New(Config{Root: root})) }},
			{"Server.Static", "/files", func(s *celeris.Server) { s.Static("/files", root) }},
			{"Group.Static", "/g/files", func(s *celeris.Server) { s.Group("/g").Static("/files", root) }},
		} {
			t.Run(fmt.Sprintf("%v/%s", eng, e.name), func(t *testing.T) {
				raw := wire888(t, eng, e.reg)
				for _, p := range hidden {
					status, body := raw(e.prefix + p)
					if status != 404 {
						t.Errorf("GET %s%s: status %d body %q, want 404", e.prefix, p, status, body)
					}
					for _, secret := range []string{"ENV", "GITCFG", "SSH", "HTA", "SUBHID", "WKSECRET"} {
						if strings.Contains(body, secret) {
							t.Errorf("GET %s%s leaked %q", e.prefix, p, secret)
						}
					}
				}
				for p, want := range visible {
					if status, body := raw(e.prefix + p); status != 200 || body != want {
						t.Errorf("GET %s%s: status %d body %q, want 200 %q", e.prefix, p, status, body, want)
					}
				}
			})
		}
		t.Run(fmt.Sprintf("%v/middleware-deny-and-allow", eng), func(t *testing.T) {
			deny := wire888(t, eng, func(s *celeris.Server) { s.Use(New(Config{Root: root, Dotfiles: DotfilesDeny})) })
			if status, body := deny("/.env"); status != 403 || strings.Contains(body, "ENV") {
				t.Errorf("Deny GET /.env: status %d body %q, want 403", status, body)
			}
			if status, body := deny("/.well-known/acme.txt"); status != 200 || body != tagWK888 {
				t.Errorf("Deny GET /.well-known/acme.txt: status %d body %q", status, body)
			}
			allow := wire888(t, eng, func(s *celeris.Server) { s.Use(New(Config{Root: root, Dotfiles: DotfilesAllow})) })
			if status, body := allow("/.env"); status != 200 || body != "ENV" {
				t.Errorf("Allow GET /.env: status %d body %q, want 200 ENV", status, body)
			}
		})
	}
}

// TestFileFromDirServesDotfiles888 pins what this change leaves alone:
// Context.FileFromDir is an explicit API whose caller chooses the path, and
// serves a dotfile as before.
func TestFileFromDirServesDotfiles888(t *testing.T) {
	skipWindows888(t)
	root := dotTree(t)
	c, rec := celeristest.NewContext("GET", "/f")
	defer celeristest.ReleaseContext(c)
	if err := c.FileFromDir(root, ".env"); err != nil || rec.StatusCode != 200 || string(rec.Body) != "ENV" {
		t.Errorf("FileFromDir(.env): status %d body %q err %v, want 200 ENV", rec.StatusCode, rec.Body, err)
	}
}
