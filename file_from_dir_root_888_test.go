package celeris

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/ctxkit"
)

// Context.FileFromDir with a baseDir of ".", "./" and "/" (celeris#888 item
// 46). The containment check compared the cleaned path with base + "/", which
// no path below "." or "/" starts with, so every file below these bases was
// refused with a 400 ("invalid file path") and middleware/static, which serves
// through it, served nothing below such a Root. Server.Static and Group.Static
// serve through it as well.

func fileFromDir888(t *testing.T, base, userPath string) (status int, body string, err error) {
	t.Helper()
	s, rw := newTestStream("GET", "/f")
	defer s.Release()
	c := acquireContext(s)
	defer releaseContext(c)
	err = c.FileFromDir(base, userPath)
	return rw.status, string(rw.body), err
}

func isInvalidPath888(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Code == 400
}

func TestFileFromDirDotAndSlashBases888(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	outside := filepath.Join(tmp, "outside")
	for p, body := range map[string]string{
		filepath.Join(root, "a.txt"):         "A",
		filepath.Join(root, "sub", "b.txt"):  "B",
		filepath.Join(outside, "secret.txt"): "SECRET",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		"in-abs":  filepath.Join(root, "a.txt"),
		"in-rel":  "a.txt",
		"esc-abs": filepath.Join(outside, "secret.txt"),
		"esc-rel": filepath.Join("..", "outside", "secret.txt"),
		"esc-dir": outside,
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Skipf("symlinks are not available here: %v", err)
		}
	}
	rootSlash := strings.TrimPrefix(filepath.ToSlash(root), "/") // below "/"

	type fcase struct {
		path string
		want string // the body, or "" for a 400
	}
	dotCases := []fcase{
		{"a.txt", "A"}, {"/a.txt", "A"}, {"sub/b.txt", "B"}, {"./sub/../a.txt", "A"},
		{"in-abs", "A"}, {"in-rel", "A"},
		{"esc-abs", ""}, {"esc-rel", ""}, {"esc-dir/secret.txt", ""},
		{"../outside/secret.txt", ""}, {"sub/../../outside/secret.txt", ""},
		{"../../../../../../etc/passwd", ""}, {"..", ""}, {"sub", ""},
		{"a.txt\x00", ""}, {`..\outside\secret.txt`, ""},
	}
	for _, base := range []string{".", "./"} {
		t.Run("base "+base, func(t *testing.T) {
			t.Chdir(root)
			for _, tc := range dotCases {
				status, body, err := fileFromDir888(t, base, tc.path)
				if strings.Contains(body, "SECRET") {
					t.Errorf("FileFromDir(%q, %q) sent the secret", base, tc.path)
				}
				if tc.want == "" {
					if err == nil || status == 200 {
						t.Errorf("FileFromDir(%q, %q): status %d err %v, want a refusal", base, tc.path, status, err)
					}
					continue
				}
				if err != nil || status != 200 || body != tc.want {
					t.Errorf("FileFromDir(%q, %q): status %d body %q err %v, want 200 %q", base, tc.path, status, body, err, tc.want)
				}
			}
			// The escapes are the 400 the check gives, not another error.
			for _, p := range []string{"../outside/secret.txt", "esc-abs", "esc-rel", "esc-dir/secret.txt"} {
				if _, _, err := fileFromDir888(t, base, p); !isInvalidPath888(err) {
					t.Errorf("FileFromDir(%q, %q) = %v, want an HTTPError 400", base, p, err)
				}
			}
		})
	}

	if runtime.GOOS == "windows" {
		return
	}
	t.Run("base /", func(t *testing.T) {
		// Everything is inside "/": the path below it is the absolute path
		// without its leading slash, ".." stops at the filesystem root, and a
		// symlink that leaves the tree still resolves inside "/".
		for _, tc := range []fcase{
			{rootSlash + "/a.txt", "A"}, {"/" + rootSlash + "/sub/b.txt", "B"}, {rootSlash + "/in-abs", "A"},
			{rootSlash + "/esc-abs", "SECRET"}, {rootSlash + "/../outside/secret.txt", "SECRET"},
			{strings.Repeat("../", 64) + rootSlash + "/a.txt", "A"},
			{rootSlash + "/sub", ""}, {rootSlash + "/missing.txt", ""},
		} {
			status, body, err := fileFromDir888(t, "/", tc.path)
			if tc.want == "" {
				if err == nil || status == 200 {
					t.Errorf("FileFromDir(%q, %q): status %d err %v, want a refusal", "/", tc.path, status, err)
				}
				continue
			}
			if err != nil || status != 200 || body != tc.want {
				t.Errorf("FileFromDir(%q, %q): status %d body %q err %v, want 200 %q", "/", tc.path, status, body, err, tc.want)
			}
		}
	})
}

// An empty baseDir is not the working directory: filepath.Clean("") is ".",
// and the check that now accepts everything below "." must not accept it.
// Server.Static and Group.Static pass their root on unchecked, so
// s.Static("/files", os.Getenv("STATIC_DIR")) with the variable unset was
// answered with a 400 for every file, and must still be.
func TestFileFromDirEmptyBaseIsRefused888(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	for _, p := range []string{"a.txt", "/a.txt", "./a.txt", ""} {
		status, body, err := fileFromDir888(t, "", p)
		if !isInvalidPath888(err) || status == 200 || body != "" {
			t.Errorf("FileFromDir(%q, %q): status %d body %q err %v, want an HTTPError 400", "", p, status, body, err)
		}
	}
}

// A relative baseDir that is not "." widens a little with this change: a
// symlink with an absolute target that stays inside it is served (it was a 400:
// the resolved path is absolute, the resolved base relative); one that leaves
// it is refused as before.
func TestFileFromDirRelativeBaseAbsoluteSymlink888(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for p, body := range map[string]string{
		filepath.Join(tmp, "public", "a.txt"):  "A",
		filepath.Join(tmp, "private", "s.txt"): "S",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range map[string]string{
		"in":  filepath.Join(tmp, "public", "a.txt"),
		"out": filepath.Join(tmp, "private", "s.txt"),
	} {
		if err := os.Symlink(target, filepath.Join(tmp, "public", name)); err != nil {
			t.Skipf("symlinks are not available here: %v", err)
		}
	}
	t.Chdir(tmp)
	for _, base := range []string{"public", "./public"} {
		if status, body, err := fileFromDir888(t, base, "in"); err != nil || status != 200 || body != "A" {
			t.Errorf("FileFromDir(%q, in): status %d body %q err %v, want 200 A", base, status, body, err)
		}
		if status, body, err := fileFromDir888(t, base, "out"); !isInvalidPath888(err) || status == 200 || body != "" {
			t.Errorf("FileFromDir(%q, out): status %d body %q err %v, want a 400", base, status, body, err)
		}
	}
}

func isNotRegular888(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Code == 404 && errors.Is(err, ctxkit.ErrNotRegular)
}
