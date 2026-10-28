package ctxkit

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestHiddenSegment(t *testing.T) {
	for _, tc := range []struct {
		seg  string
		want bool
	}{
		{".env", true}, {".git", true}, {".ssh", true}, {".htaccess", true}, {"..", true}, {"...", true}, {".", true},
		{"", false}, {"a", false}, {"a.b", false}, {"env.", false}, {"a.env", false},
		{".well-known", false}, {".well-knownx", true}, {".well-known-x", true}, {".well-known.", true},
		{".Well-Known", true}, {".WELL-KNOWN", true}, {"well-known", false},
		{"%2eenv", true}, {"%2Eenv", true}, {"%2e%2e", true}, {"%2e", true}, {"%252eenv", false}, {"a%2eb", false},
		{"%2ewell-known", false}, {"%zz", false}, {"%", false}, {"%2", false},
	} {
		if got := HiddenSegment(tc.seg); got != tc.want {
			t.Errorf("HiddenSegment(%q) = %v, want %v", tc.seg, got, tc.want)
		}
	}
}

func TestHiddenPath(t *testing.T) {
	for _, tc := range []struct {
		p    string
		want bool
	}{
		{"", false}, {"/", false}, {"a", false}, {"/a/b/c.txt", false}, {"a.b/c.d", false},
		{".env", true}, {"/.env", true}, {"a/.env", true}, {"a/b/.c/d", true}, {"a/.b/c", true}, {"a/b/.c", true},
		{"/.git/config", true}, {".ssh/id_rsa", true},
		// ".well-known", exactly, and its children that are not hidden themselves
		{".well-known", false}, {"/.well-known/acme", false}, {".well-known/a/b", false},
		{".well-known/.x", true}, {".well-known/a/.x/b", true}, {".well-known-x/a", true}, {".well-knownx", true},
		{"a/.well-known/b", false}, {"a/.well-known/.b", true},
		// cleaned first: "." and empty segments drop, ".." removes the segment before it
		{"./.env", true}, {"//.env", true}, {"a//.env", true}, {"a/./.env", true}, {"a/../.env", true}, {"a/../b", false},
		{".git/../a", false}, {".well-known/../.env", true}, {"a/.b/..", false}, {"a/.b/../c", false}, {"../a", false}, {"/../.env", true},
		{"a/b/../../.env", true}, {"a/.env/../b", false},
		// a backslash is a name character (not a separator) except on Windows:
		// a piece that starts with a dot hides the segment, ".." is hidden, and a
		// ".." never pops the segment before it
		{`\.env`, true}, {`a\.env`, true}, {`a\b`, false}, {`.well-known\.x`, true},
		{`.git/..\x`, true}, {`.s\..\y`, true}, {`sub/..\.git/config`, true}, {`a\..\b`, true},
		// encoded: one decoding
		{"%2eenv", true}, {"a/%2Eenv", true}, {"%2e%2e/a", true}, {"a/%2e%2e/b", true}, {"%252eenv", false},
		{".env\x00", true}, {"a\x00/.b", true},
		// the name of a root is not part of the path
		{"sub/file", false},
	} {
		if got := HiddenPath(tc.p); got != tc.want {
			t.Errorf("HiddenPath(%q) = %v, want %v", tc.p, got, tc.want)
		}
	}
}

func TestHiddenPathAllocs(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	if n := testing.AllocsPerRun(100, func() { _ = HiddenPath("assets/css/site.min.css") }); n != 0 {
		t.Errorf("HiddenPath of a plain path allocates %v times, want 0", n)
	}
}

func TestRelWithin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("slash paths")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wd := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(wd, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(wd)
	for _, tc := range []struct {
		base, p, want string
		ok            bool
	}{
		{"/srv/www", "/srv/www", "", true},
		{"/srv/www", "/srv/www/a/.env", "a/.env", true},
		{"/", "/etc/passwd", "etc/passwd", true},
		{".", "x/.env", "x/.env", true},
		{".", ".", "", true},
		{"public", "public/x/y", "x/y", true},
		{"public", filepath.Join(wd, "public", "x"), "x", true}, // relative base, absolute path
		{".", filepath.Join(wd, "x", "y"), "x/y", true},
		{".", "../b/x", "x", true}, // climbs through the working directory's own name
		{"..", "../b/x", "b/x", true},
		{"..", filepath.Join(dir, "a", "q"), "q", true},
		{"/srv/www", "/srv/other", "", false},
		{".", "../other", "", false},
	} {
		got, ok := RelWithin(tc.base, tc.p)
		if ok != tc.ok || got != tc.want {
			t.Errorf("RelWithin(%q, %q) = %q, %v; want %q, %v", tc.base, tc.p, got, ok, tc.want, tc.ok)
		}
	}
}
