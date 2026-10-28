package celeris

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
