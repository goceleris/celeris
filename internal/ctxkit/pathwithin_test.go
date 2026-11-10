package ctxkit

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPathWithin(t *testing.T) {
	sep := string(filepath.Separator)
	cases := []struct {
		base, p string
		want    bool
	}{
		{"/srv/www", "/srv/www", true},
		{"/srv/www", "/srv/www/a", true},
		{"/srv/www", "/srv/www/a/b.txt", true},
		{"/srv/www", "/srv/wwwx", false},
		{"/srv/www", "/srv/ww", false},
		{"/srv/www", "/srv", false},
		{"/srv/www", "/etc/passwd", false},
		{"/srv/www", "/", false},
		{"public", "public/a", true},
		{"public", "publicity/a", false},
		{"public", "a", false},
		{".", ".", true},
		{".", "a", true},
		{".", "a/b/c.txt", true},
		{".", ".hidden", true},
		{".", "..a", true}, // a file named "..a" is not a climb
		{".", "..", false},
		{".", "../a", false},
		{".", "../../etc/passwd", false},
		{".", "/etc/passwd", false},
		{"/", "/", true},
		{"/", "/etc/passwd", true},
		{"/", "/a", true},
	}
	// A base of ".." segments names a directory above the working directory;
	// nothing above that one is within it. The working directory of these rows
	// is a tree of three levels, so the answers do not depend on where it is.
	dotdot := []struct {
		base, p string
		want    bool
	}{
		{"..", "..", true},
		{"..", "../a", true},
		{"..", "../c/a", true},
		{"..", "a", true}, // the working directory is inside its parent
		{"..", "../..", false},
		{"..", "../../etc/passwd", false},
		{"../..", "../../x", true},
		{"../..", "../x", true},
		{"../..", "../../..", false},
		{"../..", "../../../x", false},
		{"../..", "../../../../etc/passwd", false},
		{"../../..", "../../../x", true},
		{"../../..", "../../../../x", false},
		{"../x", "../x/y", true},
		{"../x", "../y", false},
		{"../x", "../../x", false},
		{"..", "/etc/passwd", false},
	}
	for _, tc := range cases {
		if runtime.GOOS == "windows" {
			break // the cases are slash paths; the volume root is checked below
		}
		if got := PathWithin(tc.base, tc.p); got != tc.want {
			t.Errorf("PathWithin(%q, %q) = %v, want %v", tc.base, tc.p, got, tc.want)
		}
	}
	if runtime.GOOS != "windows" {
		wd := filepath.Join(t.TempDir(), "a", "b", "c")
		if err := os.MkdirAll(wd, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(wd)
		for _, tc := range dotdot {
			if got := PathWithin(tc.base, tc.p); got != tc.want {
				t.Errorf("PathWithin(%q, %q) = %v, want %v", tc.base, tc.p, got, tc.want)
			}
		}
	}
	// The root of the volume, whatever the separator.
	root := filepath.VolumeName(os.TempDir()) + sep
	if !PathWithin(root, filepath.Join(os.TempDir(), "x")) || !PathWithin(root, root) {
		t.Errorf("PathWithin(%q, ...) refuses a path below the volume root", root)
	}
}

func TestResolvedWithin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("slash paths")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	// A relative base: "." and "sub", a resolved path that a symlink with an
	// absolute target made absolute.
	if err := os.Mkdir("sub", 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		base, resolved string
		want           bool
	}{
		{".", "a/b", true},
		{".", filepath.Join(dir, "sub", "f"), true},
		{".", dir, true},
		{".", filepath.Join(filepath.Dir(dir), "other"), false},
		{".", "/etc/passwd", false},
		{".", "../x", false},
		{"sub", filepath.Join(dir, "sub", "f"), true},
		{"sub", filepath.Join(dir, "other"), false},
		{dir, filepath.Join(dir, "a"), true},
		{dir, "/etc/passwd", false},
		{"/", "/etc/passwd", true},
		// A relative path that climbs back into the base (a symlink "../c/a"
		// from a working directory named c), and one that climbs out.
		{".", filepath.Join("..", filepath.Base(dir), "a"), true},
		{".", filepath.Join("..", "other"), false},
		{"..", filepath.Join("..", filepath.Base(dir), "a"), true},
		{"..", filepath.Join("..", "..", "x"), false},
	} {
		if got := ResolvedWithin(tc.base, tc.resolved); got != tc.want {
			t.Errorf("ResolvedWithin(%q, %q) = %v, want %v", tc.base, tc.resolved, got, tc.want)
		}
	}
}
