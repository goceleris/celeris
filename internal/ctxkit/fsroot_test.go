package ctxkit

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// isRootOf is the OS-independent part of the filesystem-root test: what is left
// of a path once its volume name is split off. These rows include the Windows
// spellings, so they run everywhere.
func TestIsRootOf(t *testing.T) {
	for _, tc := range []struct {
		name       string
		vol, rest  string
		sep        byte
		wantIsRoot bool
	}{
		{"unix root", "", "/", '/', true},
		{"unix dir", "", "/usr", '/', false},
		{"unix empty", "", "", '/', false},
		{"unix two slashes", "", "//", '/', false}, // not a cleaned path
		{"drive root", `C:`, `\`, '\\', true},
		{"drive dir", `C:`, `\Users`, '\\', false},
		{"drive relative", `C:`, ``, '\\', false},
		{"drive relative dir", `C:`, `x`, '\\', false},
		{"unc root with slash", `\\host\share`, `\`, '\\', true},
		{"unc root", `\\host\share`, ``, '\\', true},
		{"unc dir", `\\host\share`, `\dir`, '\\', false},
		{"device root", `\\?\C:`, `\`, '\\', true},
		{"device dir", `\\?\C:`, `\x`, '\\', false},
	} {
		if got := isRootOf(tc.vol, tc.rest, tc.sep); got != tc.wantIsRoot {
			t.Errorf("%s: isRootOf(%q, %q) = %v, want %v", tc.name, tc.vol, tc.rest, got, tc.wantIsRoot)
		}
	}
}

func TestFilesystemRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("slash paths")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wd := filepath.Join(base, "a", "b")
	if err := os.MkdirAll(wd, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "to-root")
	if err := os.Symlink("/", link); err != nil {
		t.Skip(err)
	}
	t.Chdir(wd)
	up := "../../.."
	for range strings.Count(wd, "/") {
		up += "/.."
	}
	for _, tc := range []struct {
		root string
		want bool
	}{
		{"/", true}, {"/.", true}, {"//", true}, {"/..", true}, {link, true}, {up, true},
		{link + "/..", false}, // cleaned lexically, as the package cleans a root: the directory that holds the link
		{".", false}, {"..", false}, {"../..", false}, {wd, false}, {"/etc", false}, {"/no/such", false}, {"sub", false},
	} {
		got, ok := FilesystemRoot(filepath.Clean(tc.root))
		if ok != tc.want || (ok && got != "/") {
			t.Errorf("FilesystemRoot(%q) = %q, %v; want root %v", tc.root, got, ok, tc.want)
		}
	}
	// "." at "/"
	t.Chdir("/")
	if got, ok := FilesystemRoot("."); !ok || got != "/" {
		t.Errorf(`FilesystemRoot(".") at "/" = %q, %v`, got, ok)
	}
}
