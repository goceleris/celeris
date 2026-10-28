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
	for _, tc := range cases {
		if runtime.GOOS == "windows" {
			break // the cases are slash paths; the volume root is checked below
		}
		if got := PathWithin(tc.base, tc.p); got != tc.want {
			t.Errorf("PathWithin(%q, %q) = %v, want %v", tc.base, tc.p, got, tc.want)
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
	} {
		if got := ResolvedWithin(tc.base, tc.resolved); got != tc.want {
			t.Errorf("ResolvedWithin(%q, %q) = %v, want %v", tc.base, tc.resolved, got, tc.want)
		}
	}
}
