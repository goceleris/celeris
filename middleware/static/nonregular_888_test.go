package static

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// Non-regular files (celeris#888 item 46, round 2 of the security review). With
// Root "/" or "." in a working directory of "/" (the systemd default, a
// container without a WORKDIR) GET /dev/zero read without bound into memory (8
// GB/s; the process was OOM-killed on std and epoll): stat reports size 0 for a
// device, the size cap passes it, and io.ReadAll never ends. A FIFO anywhere in
// a served tree hung the handler in open(2). Devices, FIFOs and sockets are not
// served: they are answered as a missing file (404 under the middleware); the
// explicit APIs (Context.File, FileFromDir, FileFromFS) return a 404 HTTPError
// the caller sees. A symlink is followed to a regular file only.
//
// The device cells run only where a runaway read cannot hurt the machine they
// run on in a way the guard does not catch: guardMemory888 exits the test binary
// at 256 MiB of heap. They do not run on macOS, where a regression would be
// writing 8 GB/s into a developer's machine (set L21_DEVICE_CELLS=1 to force).

// guardMemory888 ends the test binary, with a message, when the heap passes
// limit: a read without bound must not take the machine with it.
func guardMemory888(t *testing.T, limit uint64) {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > limit {
				fmt.Fprintf(os.Stderr, "FAIL: %s: the heap is %d MiB: a non-regular file is read without bound\n", t.Name(), ms.HeapAlloc>>20)
				os.Exit(3)
			}
		}
	}()
	debug.SetMemoryLimit(int64(limit) * 2)
	t.Cleanup(func() { debug.SetMemoryLimit(-1) })
}

func deviceCellsOK888(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no devices, FIFOs or sockets as files")
	}
	if runtime.GOOS == "darwin" && os.Getenv("L21_DEVICE_CELLS") != "1" {
		t.Skip("the device cells read /dev/zero when the rule is missing: run them in a memory-capped container (L21_DEVICE_CELLS=1 forces them here)")
	}
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skip("no /dev/zero")
	}
}

// specialTree888 is a directory with a regular file, a FIFO, a unix socket and
// symlinks to each, and to devices.
type specialTree888 struct{ dir string }

func newSpecialTree888(t *testing.T) specialTree888 {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs or unix sockets as files")
	}
	// a short path: a unix socket's path is at most about 100 bytes
	dir, err := os.MkdirTemp("", "l21")
	if err != nil {
		t.Fatal(err)
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, "reg.txt"), []byte("REG"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644); err != nil {
		t.Skipf("no mkfifo here: %v", err)
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "sock"))
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	for link, target := range map[string]string{
		"lnk-fifo": "fifo", "lnk-sock": "sock", "lnk-reg": "reg.txt",
		"lnk-null": "/dev/null", "lnk-zero": "/dev/zero", "lnk-urandom": "/dev/urandom",
	} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Skipf("symlinks are not available here: %v", err)
		}
	}
	return specialTree888{dir}
}

// within888 runs fn and fails the test if it does not return in d: a handler
// stuck in open(2) on a FIFO is a hung request.
func within888[T any](t *testing.T, what string, d time.Duration, fn func() T) T {
	t.Helper()
	ch := make(chan T, 1)
	go func() { ch <- fn() }()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatalf("%s: no answer in %v (the handler is stuck)", what, d)
		var zero T
		return zero
	}
}

// TestNonRegularFilesAreNotServed888: a FIFO, a socket and symlinks to them, in
// a tree served by an absolute Root, by ".", and by "/" (by absolute path), and
// with an os.DirFS: answered as missing, at once, and the regular file still
// served. Symlinks to devices leave the tree under the roots that have one,
// and are refused there.
func TestNonRegularFilesAreNotServed888(t *testing.T) {
	st := newSpecialTree888(t)
	type cfg struct {
		name string
		mw   func(t *testing.T) celeris.HandlerFunc
		url  func(p string) string
	}
	rel := func(p string) string { return p }
	cfgs := []cfg{
		{"abs-root", func(t *testing.T) celeris.HandlerFunc { return New(Config{Root: st.dir}) }, rel},
		{"dot-root", func(t *testing.T) celeris.HandlerFunc { t.Chdir(st.dir); return New(Config{Root: "."}) }, rel},
		{"slash-root", func(t *testing.T) celeris.HandlerFunc { return New(Config{Root: "/"}) }, func(p string) string { return st.dir + p }},
		{"dirfs", func(t *testing.T) celeris.HandlerFunc { return New(Config{FS: os.DirFS(st.dir)}) }, rel},
		{"spa", func(t *testing.T) celeris.HandlerFunc { return New(Config{Root: st.dir, SPA: true}) }, rel},
	}
	for _, c := range cfgs {
		for _, p := range []string{"/fifo", "/sock", "/lnk-fifo", "/lnk-sock", "/lnk-null"} {
			t.Run(c.name+p, func(t *testing.T) {
				mw := c.mw(t)
				path := c.url(p)
				rec := within888(t, "GET "+path, 5*time.Second, func() *celeristest.ResponseRecorder {
					rec, _ := get888(t, mw, path)
					return rec
				})
				body := string(rec.Body)
				switch {
				case c.name == "spa":
					// a missing path is the index file in SPA mode (none here): passed on
					if rec.StatusCode == 200 && body != "" {
						t.Errorf("GET %s: status %d body %q", path, rec.StatusCode, body)
					}
				case rec.StatusCode != 0 || body != "":
					t.Errorf("GET %s: status %d body %q, want it passed on as a missing file is", path, rec.StatusCode, body)
				}
			})
		}
		t.Run(c.name+"/reg.txt", func(t *testing.T) {
			mw := c.mw(t)
			rec, _ := get888(t, mw, c.url("/reg.txt"))
			if rec.StatusCode != 200 || string(rec.Body) != "REG" {
				t.Errorf("the regular file: status %d body %q, want 200 REG", rec.StatusCode, rec.Body)
			}
			rec, _ = get888(t, mw, c.url("/lnk-reg"))
			if rec.StatusCode != 200 || string(rec.Body) != "REG" {
				t.Errorf("a symlink to the regular file: status %d body %q, want 200 REG", rec.StatusCode, rec.Body)
			}
		})
	}
}

// TestNonRegularFilesExplicitAPIs888: Context.File, FileFromDir and FileFromFS
// return an error the caller can see (an HTTPError 404 "not a regular file")
// for a FIFO, a socket and a symlink to one, without blocking.
func TestNonRegularFilesExplicitAPIs888(t *testing.T) {
	st := newSpecialTree888(t)
	codeOf := func(err error) int {
		if he, ok := err.(*celeris.HTTPError); ok {
			return he.Code
		}
		return -1
	}
	for _, name := range []string{"fifo", "sock", "lnk-fifo", "lnk-sock"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(st.dir, name)
			calls := map[string]func(c *celeris.Context) error{
				"File":        func(c *celeris.Context) error { return c.File(path) },
				"FileFromDir": func(c *celeris.Context) error { return c.FileFromDir(st.dir, name) },
				"FileFromFS":  func(c *celeris.Context) error { return c.FileFromFS(name, os.DirFS(st.dir)) },
			}
			for what, call := range calls {
				err := within888(t, what+" "+name, 5*time.Second, func() error {
					c, rec := celeristest.NewContext("GET", "/f")
					defer celeristest.ReleaseContext(c)
					err := call(c)
					if rec.StatusCode == 200 {
						return fmt.Errorf("answered 200")
					}
					return err
				})
				if codeOf(err) != 404 {
					t.Errorf("%s(%s) = %v, want an HTTPError 404", what, name, err)
				}
			}
		})
	}
	t.Run("regular", func(t *testing.T) {
		c, rec := celeristest.NewContext("GET", "/f")
		defer celeristest.ReleaseContext(c)
		if err := c.FileFromDir(st.dir, "lnk-reg"); err != nil || string(rec.Body) != "REG" {
			t.Errorf("FileFromDir(lnk-reg): %v, body %q", err, rec.Body)
		}
	})
}

// TestDeviceFilesStatic888: the middleware under the roots that reach /dev:
// "/" and "." in a working directory of "/". The guard exits the binary if a
// read runs away.
func TestDeviceFilesStatic888(t *testing.T) {
	deviceCellsOK888(t)
	guardMemory888(t, 256<<20)
	for _, root := range []string{"/", "."} {
		t.Run(root, func(t *testing.T) {
			t.Chdir("/")
			mw := New(Config{Root: root})
			for _, p := range []string{"/dev/zero", "/dev/null", "/dev/urandom", "/dev/random", "/dev/full"} {
				rec := within888(t, "GET "+p, 10*time.Second, func() *celeristest.ResponseRecorder {
					rec, _ := get888(t, mw, p)
					return rec
				})
				if rec.StatusCode != 0 || len(rec.Body) != 0 {
					t.Errorf("Root %q GET %s: status %d, %d body bytes, want it passed on as a missing file", root, p, rec.StatusCode, len(rec.Body))
				}
			}
			// a symlink to a device, inside a served tree under Root "/"
			st := newSpecialTree888(t)
			for _, l := range []string{"lnk-zero", "lnk-null", "lnk-urandom"} {
				p := st.dir + "/" + l
				if root == "." {
					continue
				}
				rec := within888(t, "GET "+p, 10*time.Second, func() *celeristest.ResponseRecorder {
					rec, _ := get888(t, mw, p)
					return rec
				})
				if rec.StatusCode != 0 || len(rec.Body) != 0 {
					t.Errorf("Root %q GET %s: status %d, %d body bytes, want it passed on", root, p, rec.StatusCode, len(rec.Body))
				}
			}
		})
	}
	t.Run("dirfs", func(t *testing.T) {
		mw := New(Config{FS: os.DirFS("/")})
		for _, p := range []string{"/dev/zero", "/dev/urandom"} {
			rec := within888(t, "GET "+p, 10*time.Second, func() *celeristest.ResponseRecorder {
				rec, _ := get888(t, mw, p)
				return rec
			})
			if rec.StatusCode != 0 || len(rec.Body) != 0 {
				t.Errorf("os.DirFS(/) GET %s: status %d, %d body bytes, want it passed on", p, rec.StatusCode, len(rec.Body))
			}
		}
	})
}

// TestDeviceFilesExplicitAPIs888: c.File, FileFromDir("/", ...) and FileFromFS
// refuse a device with an HTTPError 404 and read nothing.
func TestDeviceFilesExplicitAPIs888(t *testing.T) {
	deviceCellsOK888(t)
	guardMemory888(t, 256<<20)
	for what, call := range map[string]func(c *celeris.Context) error{
		"File":        func(c *celeris.Context) error { return c.File("/dev/zero") },
		"FileFromDir": func(c *celeris.Context) error { return c.FileFromDir("/", "dev/zero") },
		"FileFromFS":  func(c *celeris.Context) error { return c.FileFromFS("dev/zero", os.DirFS("/")) },
	} {
		err := within888(t, what, 10*time.Second, func() error {
			c, _ := celeristest.NewContext("GET", "/f")
			defer celeristest.ReleaseContext(c)
			return call(c)
		})
		if he, ok := err.(*celeris.HTTPError); !ok || he.Code != 404 {
			t.Errorf("%s of /dev/zero = %v, want an HTTPError 404", what, err)
		}
	}
}

// TestDeviceFilesOverTheWire888: the exact request of the review, over the
// engines: Root "." with a working directory of "/", Root "/", Server.Static
// and Group.Static over "." — GET /dev/zero is a 404 and the server stays up.
func TestDeviceFilesOverTheWire888(t *testing.T) {
	deviceCellsOK888(t)
	guardMemory888(t, 256<<20)
	t.Chdir("/")
	type entry struct {
		name, prefix string
		reg          func(s *celeris.Server)
	}
	for _, eng := range wireEngines888 {
		for _, e := range []entry{
			{"middleware-dot", "", func(s *celeris.Server) { s.Use(New(Config{Root: "."})) }},
			{"middleware-slash", "", func(s *celeris.Server) { s.Use(New(Config{Root: "/"})) }},
			{"Server.Static-dot", "/files", func(s *celeris.Server) { s.Static("/files", ".") }},
			{"Group.Static-slash", "/g/files", func(s *celeris.Server) { s.Group("/g").Static("/files", "/") }},
		} {
			t.Run(fmt.Sprintf("%v/%s", eng, e.name), func(t *testing.T) {
				raw := wire888(t, eng, e.reg)
				for _, p := range []string{"/dev/zero", "/dev/urandom", "/dev/null"} {
					status, body := raw(e.prefix + p)
					if status != 404 || len(body) > 64 {
						t.Errorf("GET %s%s: status %d, %d body bytes, want 404", e.prefix, p, status, len(body))
					}
				}
				if status, _ := raw(e.prefix + "/etc/hostname"); status != 200 && !strings.Contains(e.name, "Static") {
					t.Logf("GET /etc/hostname: %d (the server is up)", status)
				}
			})
		}
	}
}
