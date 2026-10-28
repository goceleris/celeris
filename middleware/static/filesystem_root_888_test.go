package static

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/goceleris/celeris"
)

// A Root that resolves to the root of a filesystem is refused at construction
// unless Config.AllowFilesystemRoot is set (celeris#888 item 46, the maintainer's
// decision after the security review): "/" and "." in a working directory of "/"
// (the systemd default, a container without a WORKDIR) serve every file the
// process can read, and no released version served anything below such a root,
// so no working deployment depends on it. The check is on the RESOLVED root, so
// "." at "/", "../../.." reaching "/" and a symlink to "/" do not get through;
// it is made when New (or Server.Static) is called, and a chdir after that is
// not covered. Server.Static and Group.Static have no acknowledgement field and
// refuse such a root; middleware/static is the way to serve one on purpose.

func panicOf888(f func()) (v any) {
	defer func() { v = recover() }()
	f()
	return nil
}

func skipFSRoot888(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the filesystem root of the tests is \"/\"")
	}
}

// deepDir888 makes a working directory several levels below "/" and returns the
// number of levels from "/" (with symlinks resolved).
func deepDir888(t *testing.T) (dir string, depth int) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(base, "a", "b", "c")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return dir, len(strings.Split(strings.Trim(dir, "/"), "/"))
}

func TestFilesystemRootIsRefused888(t *testing.T) {
	skipFSRoot888(t)
	_, depth := deepDir888(t)
	link := filepath.Join(t.TempDir(), "to-root")
	if err := os.Symlink("/", link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	up := strings.TrimSuffix(strings.Repeat("../", depth), "/")
	for _, tc := range []struct {
		name, root, cwd string
	}{
		{"slash", "/", ""},
		{"slash-slash", "//", ""},
		{"slash-dot", "/.", ""},
		{"slash-dotdot", "/../..", ""},
		{"dot-at-slash", ".", "/"},
		{"dot-slash-at-slash", "./", "/"},
		{"dotdot-from-below-slash", "..", "/usr"},
		{"up-to-slash", up, ""},
		{"up-beyond-slash", up + "/../../..", ""},
		{"symlink-to-slash", link, ""},
		{"symlink-to-slash-slash", link + "/", ""},
		{"symlink-to-slash-dotdot", link + "/..", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.cwd != "" {
				if _, err := os.Stat(tc.cwd); err != nil {
					t.Skip("no " + tc.cwd)
				}
				t.Chdir(tc.cwd)
			}
			r := panicOf888(func() { New(Config{Root: tc.root}) })
			msg, _ := r.(string)
			if msg == "" {
				t.Fatalf("New(Root %q) did not panic (cwd %q)", tc.root, tc.cwd)
			}
			for _, want := range []string{`"/"`, "AllowFilesystemRoot", "static:"} {
				if !strings.Contains(msg, want) {
					t.Errorf("the panic %q lacks %q (the resolved root and the field must be named)", msg, want)
				}
			}
			// acknowledged, it is built
			if r := panicOf888(func() { New(Config{Root: tc.root, AllowFilesystemRoot: true}) }); r != nil {
				t.Errorf("New(Root %q, AllowFilesystemRoot) panicked: %v", tc.root, r)
			}
		})
	}
}

func TestOrdinaryRootsAreNotRefused888(t *testing.T) {
	skipFSRoot888(t)
	dir, _ := deepDir888(t)
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "to-dir")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	for _, root := range []string{".", "./", "sub", "./sub", "..", "../..", "../../..", dir, sub, dir + "/", link, "/tmp", "/etc", "/no/such/dir", "x/.."} {
		if r := panicOf888(func() { New(Config{Root: root}) }); r != nil {
			t.Errorf("New(Root %q) panicked: %v", root, r)
		}
	}
	// FS has no root: a Root next to it is ignored, as it always was
	if r := panicOf888(func() { New(Config{FS: os.DirFS("/")}) }); r != nil {
		t.Errorf("New(FS: os.DirFS(/)) panicked: %v", r)
	}
	if r := panicOf888(func() { New(Config{FS: os.DirFS(dir), Root: "/"}) }); r != nil {
		t.Errorf("New(FS and Root \"/\") panicked: %v (FS takes precedence)", r)
	}
}

// TestFilesystemRootAcknowledged888: with the acknowledgement the root serves
// (a regular file), and the other rules stay: hidden files are not served, nor a
// FIFO, nor a symlink that leads to a hidden file.
func TestFilesystemRootAcknowledged888(t *testing.T) {
	skipFSRoot888(t)
	st := newSpecialTree888(t)
	if err := os.WriteFile(filepath.Join(st.dir, ".env"), []byte("ENV"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".env", filepath.Join(st.dir, "lnk-env")); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	for _, root := range []string{"/", "."} {
		t.Run(root, func(t *testing.T) {
			t.Chdir("/")
			mw := New(Config{Root: root, AllowFilesystemRoot: true})
			if o, _ := outcomeOf888(t, mw, st.dir+"/reg.txt"); o.status != 200 || o.body != "REG" {
				t.Errorf("GET reg.txt: %v, want 200 REG", o)
			}
			for _, p := range []string{"/.env", "/lnk-env", "/fifo", "/lnk-fifo", "/sock"} {
				if o, _ := outcomeOf888(t, mw, st.dir+p); o.kind != "passed-on" {
					t.Errorf("GET %s under an acknowledged filesystem root: %v, want it passed on", p, o)
				}
			}
			deny := New(Config{Root: root, AllowFilesystemRoot: true, Dotfiles: DotfilesDeny})
			if _, err := get888(t, deny, st.dir+"/.env"); err == nil {
				t.Errorf("Deny: GET .env returned no error")
			}
		})
	}
}

// TestStaticRoutesRefuseAFilesystemRoot888: Server.Static and Group.Static have no
// acknowledgement field: they panic at registration, and name middleware/static.
func TestStaticRoutesRefuseAFilesystemRoot888(t *testing.T) {
	skipFSRoot888(t)
	dir, depth := deepDir888(t)
	link := filepath.Join(t.TempDir(), "to-root")
	if err := os.Symlink("/", link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	up := strings.TrimSuffix(strings.Repeat("../", depth), "/")
	for _, root := range []string{"/", "//", up, link} {
		for name, reg := range map[string]func(root string){
			"Server.Static": func(root string) { celeris.New(celeris.Config{}).Static("/files", root) },
			"Group.Static":  func(root string) { celeris.New(celeris.Config{}).Group("/g").Static("/files", root) },
		} {
			r := panicOf888(func() { reg(root) })
			msg, _ := r.(string)
			if msg == "" || !strings.Contains(msg, "middleware/static") || !strings.Contains(msg, "AllowFilesystemRoot") || !strings.Contains(msg, `"/"`) {
				t.Errorf("%s(root %q) panicked with %v, want a message that names the resolved root, middleware/static and AllowFilesystemRoot", name, root, r)
			}
		}
	}
	t.Chdir("/")
	if r := panicOf888(func() { celeris.New(celeris.Config{}).Static("/files", ".") }); r == nil {
		t.Errorf("Server.Static(., ) at a working directory of / did not panic")
	}
	for _, root := range []string{dir, ".", "..", "sub"} {
		if root != dir {
			continue // from "/" the others are the filesystem root or relative to it
		}
		if r := panicOf888(func() { celeris.New(celeris.Config{}).Static("/files", root) }); r != nil {
			t.Errorf("Server.Static(%q) panicked: %v", root, r)
		}
	}
	if r := panicOf888(func() { celeris.New(celeris.Config{}).Static("/files", "") }); r != nil {
		t.Errorf("Server.Static with an empty root panicked: %v (it serves nothing)", r)
	}
}

func ExampleConfig_allowFilesystemRoot() {
	// A filesystem root served on purpose, acknowledged.
	_ = New(Config{Root: "/", AllowFilesystemRoot: true})
	fmt.Println("ok")
	// Output: ok
}
