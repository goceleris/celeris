package main

import (
	"cmp"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/want from the fixture")

// copyFixture copies testdata/fixture to a temporary directory, naming each
// go.mod.txt go.mod (the fixture keeps them renamed so the tools module and
// `find . -name go.mod` do not see extra modules).
func copyFixture(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	src := filepath.Join("testdata", "fixture")
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if info.Name() == "go.mod.txt" {
			target = filepath.Join(filepath.Dir(target), "go.mod")
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func gen(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files, err := generate(root)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestFixture compares the golden files of the fixture with testdata/want.
func TestFixture(t *testing.T) {
	files := gen(t, copyFixture(t))
	want := filepath.Join("testdata", "want")
	if *update {
		if err := os.RemoveAll(want); err != nil {
			t.Fatal(err)
		}
		if _, err := writeFiles(want, files); err != nil {
			t.Fatal(err)
		}
		return
	}
	stale, err := compare(want, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stale {
		t.Errorf("%s differs from the fixture's output (go test -update rewrites it):\n%s", s.name, unifiedDiff(s.name, s.committed, s.generated))
	}
	var names []string
	for n := range files {
		names = append(names, n)
	}
	// The root package, sub and the nested module; not internal/, test/
	// (package or module), package main, or a package's _test.go exports.
	for _, n := range []string{"api/fix.txt", "api/sub.txt", "api/nested.txt"} {
		if files[n] == nil {
			t.Errorf("no %s among %v", n, names)
		}
	}
	if len(files) != 3 {
		t.Errorf("got %d files %v, want 3", len(files), names)
	}
}

// TestCheck runs the controls of celeris#443's golden-file check on the
// fixture: the check passes on the tree it was written from, fails with the
// diff on an exported change, and passes on an unexported one.
func TestCheck(t *testing.T) {
	root := copyFixture(t)
	if _, err := writeFiles(root, gen(t, root)); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		src      string   // the file to edit, relative to the fixture root; default fix.go
		with     []string // other files that get the same edit
		old, new string   // a replacement in it; old "" appends new
		file     string   // the stale file, "" for none
		diff     []string
	}{
		{name: "unchanged"},
		{name: "exported func added", new: "\nfunc Added(int) {}\n", file: "api/fix.txt", diff: []string{"+func Added(int)"}},
		// The methods an unexported interface of another package supplies
		// are listed: nothing else names them (celeris#890).
		{name: "unexported embedded interface changed", src: "fix_hiddeniface.go", old: "type hiddenReader interface{ io.Reader }", new: "type hiddenReader interface{ io.Writer }",
			file: "api/fix.txt", diff: []string{"-method (HidIface) Read([]byte) (int, error)", "+method (HidIface) Write([]byte) (int, error)", "-method (HidNested) Read([]byte) (int, error)", "+method (HidNested) Write([]byte) (int, error)"}},
		{name: "exported signature changed",
			old: "func Variadic(prefix string, rest ...int) (n int, err error)", new: "func Variadic(prefix string, rest ...int64) (n int, err error)",
			file: "api/fix.txt", diff: []string{"-func Variadic(string, ...int) (int, error)", "+func Variadic(string, ...int64) (int, error)"}},
		// Exported fields reordered: the field lines are sorted, so only the
		// order line shows it, and unkeyed literals break (celeris#890).
		{name: "exported fields reordered", src: "fix_order.go", old: "\tB string\n\tC bool\n", new: "\tC bool\n\tB string\n",
			file: "api/fix.txt", diff: []string{"-type Ordered struct, field order A, B, C", "+type Ordered struct, field order A, C, B"}},
		{name: "embedded field moved", src: "fix_order.go", old: "\tBase\n\tZ []byte\n", new: "\tZ []byte\n\tBase\n",
			file: "api/fix.txt", diff: []string{"-type Grouped struct, field order X, Y, Base, Z", "+type Grouped struct, field order X, Y, Z, Base"}},
		{name: "fields reordered in a struct with an unexported field", src: "fix_order.go", old: "\tA int\n\tB string\n\tc bool\n", new: "\tB string\n\tA int\n\tc bool\n"},
		{name: "exported method removed", old: "func (Kind) String() string { return \"\" }", new: "",
			file: "api/fix.txt", diff: []string{"-method (Kind) String() string"}},
		// The first unexported field added to an all-exported struct changes
		// no exported field line, and breaks unkeyed literals (celeris#890).
		{name: "first unexported field added", src: "fix_unexported.go", old: "\tB string\n}", new: "\tB string\n\tc int\n}",
			file: "api/fix.txt", diff: []string{"+type AllExp struct, unexported fields"}},
		{name: "last unexported field removed", src: "fix_unexported.go", old: "\tA int\n\tb int\n", new: "\tA int\n",
			file: "api/fix.txt", diff: []string{"-type OneUnexp struct, unexported fields"}},
		{name: "unexported field added to a struct that has one", src: "fix_unexported.go", old: "\tA int\n\tb int\n", new: "\tA int\n\tb int\n\tc int\n"},
		{name: "unexported identifier changed", old: "unexported = 7", new: "unexported = 8\n\tunexported2 = 9"},
		// A struct that stops being comparable (or becomes so) changes no
		// exported field line: the blank field is the whole change (celeris#890).
		{name: "struct stops being comparable", src: "fix_comparable.go", old: "\tE any\n", new: "\tE any\n\t_ [0]func()\n",
			file: "api/fix.txt", diff: []string{"+type Cmp struct, not comparable"}},
		{name: "struct becomes comparable", src: "fix_comparable.go", old: "_ [0]func()", new: "_ [0]int",
			file: "api/fix.txt", diff: []string{"-type NotCmpBlank struct, not comparable"}},
		{name: "function body changed", old: "func (b *Box[T]) Get() T                { return b.Val }", new: "func (b *Box[T]) Get() T { var z T; _ = z; return b.Val }"},
		{name: "parameter renamed", old: "func Variadic(prefix string,", new: "func Variadic(p string,"},
		// The package clause is not in the import path, and a rename breaks
		// importers that use the default name (celeris#890). go list wants one
		// name per directory, so the _test.go file is renamed with it.
		{name: "package clause renamed", src: "sub/sub.go", with: []string{"sub/only_test.go"}, old: "package sub\n", new: "package subs\n",
			file: "api/sub.txt", diff: []string{"-# Package clause: sub.", "+# Package clause: subs."}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The edit applies to each of the case's files, and each is
			// restored afterwards.
			for _, rel := range append([]string{cmp.Or(c.src, "fix.go")}, c.with...) {
				src := filepath.Join(root, filepath.FromSlash(rel))
				orig, err := os.ReadFile(src)
				if err != nil {
					t.Fatal(err)
				}
				text := string(orig)
				switch {
				case c.old != "":
					if !strings.Contains(text, c.old) {
						t.Fatalf("%s has no %q", rel, c.old)
					}
					text = strings.Replace(text, c.old, c.new, 1)
				default:
					text += c.new
				}
				if err := os.WriteFile(src, []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := os.WriteFile(src, orig, 0o644); err != nil {
						t.Error(err)
					}
				}()
			}
			stale, err := compare(root, gen(t, root))
			if err != nil {
				t.Fatal(err)
			}
			if c.file == "" {
				for _, s := range stale {
					t.Errorf("%s is stale, want it to pass:\n%s", s.name, unifiedDiff(s.name, s.committed, s.generated))
				}
				return
			}
			if len(stale) != 1 || stale[0].name != c.file {
				t.Fatalf("stale = %v, want only %s", stale, c.file)
			}
			d := unifiedDiff(stale[0].name, stale[0].committed, stale[0].generated)
			for _, l := range c.diff {
				if !strings.Contains(d, "\n"+l+"\n") {
					t.Errorf("diff has no line %q:\n%s", l, d)
				}
			}
		})
	}
}

// TestPlatformLine checks that an identifier of one platform, or of one tag
// set, is caught and says where it holds.
func TestPlatformLine(t *testing.T) {
	got := string(gen(t, copyFixture(t))["api/fix.txt"])
	for _, l := range []string{
		"func LinuxOnly() // only: linux/amd64 linux/arm64\n",
		`const PerOS untyped string = "linux" // only: linux/amd64 linux/arm64` + "\n",
		`const PerOS untyped string = "other" // only: darwin/arm64 windows/amd64` + "\n",
		"const Tagged untyped bool = true // only: (-tags=validation)\n",
	} {
		if !strings.Contains(got, l) {
			t.Errorf("api/fix.txt has no line %q", strings.TrimSuffix(l, "\n"))
		}
	}
}

// TestForeignMethods pins which methods of another module or the standard
// library are listed: those of an interface reached through an unexported
// type, and not those of a struct (time.Time's), whether it is embedded
// directly or through an unexported struct, since a Go release that adds a
// method to time.Time must change nothing here (celeris#890).
func TestForeignMethods(t *testing.T) {
	got := string(gen(t, copyFixture(t))["api/fix.txt"])
	for _, want := range []string{
		"method (HidIface) Read([]byte) (int, error)\n",
		"method (HidNested) Read([]byte) (int, error)\n",
		"method (HidPtr) Close() error\n",
		"type ExpIface struct, embedded io.Reader\n",
		"type ExpStruct struct, embedded time.Time\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("api/fix.txt has no line %q", strings.TrimSuffix(want, "\n"))
		}
	}
	for _, recv := range []string{"(HidStruct)", "(ExpStruct)", "(ExpIface)"} {
		for _, line := range strings.Split(got, "\n") {
			if strings.HasPrefix(line, "method "+recv) {
				t.Errorf("api/fix.txt lists a foreign method of %s: %q", recv, line)
			}
		}
	}
}

func TestDescribe(t *testing.T) {
	bit := func(platform, tags int) uint32 { return 1 << (platform*len(tagSets) + tags) }
	linux := bit(0, 0) | bit(0, 1) | bit(0, 2) | bit(1, 0) | bit(1, 1) | bit(1, 2)
	for _, c := range []struct {
		set  uint32
		want string
	}{
		{linux, "linux/amd64 linux/arm64"},
		{bit(0, 1) | bit(1, 1) | bit(2, 1) | bit(3, 1), "(-tags=validation)"},
		{bit(0, 0) | bit(0, 2), "linux/amd64 (no tags, -tags=celeris_closeprobe)"},
		{bit(0, 1) | bit(3, 0) | bit(3, 1) | bit(3, 2), "linux/amd64 (-tags=validation); windows/amd64"},
	} {
		if got := describe(c.set); got != c.want {
			t.Errorf("describe(%b) = %q, want %q", c.set, got, c.want)
		}
	}
}

func TestDiff(t *testing.T) {
	committed := []byte("a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\n")
	generated := []byte("a\nB\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\n")
	want := "--- a/x\n+++ b/x\n@@ -1,5 +1,5 @@\n a\n-b\n+B\n c\n d\n e\n@@ -9,3 +9,4 @@\n i\n j\n k\n+l\n"
	if got := unifiedDiff("x", committed, generated); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if got := unifiedDiff("x", nil, []byte("a\n")); got != "--- /dev/null\n+++ b/x\n@@ -0,0 +1,1 @@\n+a\n" {
		t.Errorf("new file: got\n%s", got)
	}
}
