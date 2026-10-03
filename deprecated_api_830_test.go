package celeris_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// deprecationMarker opens the paragraph that go doc, gopls and staticcheck's
// SA1019 read as a deprecation notice. It is matched with or without text after
// it on the line, as the plain-text search celeris#830 uses as its done
// condition (a git grep for the marker) matches it, and it is spelled in two
// pieces so that the search does not match this file.
const deprecationMarker = "Deprecated" + ":"

// deprecationsAllowedInV1 is the allowlist of deprecation notices outside
// internal/ trees, by file (slash-separated, from the repository root). It is
// empty at v1.6.0, which removed every deprecated public API (celeris#830).
// After the v1.6.0 tag, removing an exported identifier or moving it under
// internal/ is a breaking change that needs a /v2 import path, so a v1.x
// identifier is deprecated and kept until v2: its file goes here with the
// number of notices it carries and the issue that removes them in v2.
var deprecationsAllowedInV1 = map[string]allowedDeprecations{}

type allowedDeprecations struct {
	notices   int    // how many notices the file carries
	v2Removal string // the issue that removes them in v2, e.g. "celeris#1234"
}

// removal is one public identifier removed before the public v1.6.0 release
// (celeris#830, celeris#826), with the replacement its deprecation notice
// named. Dirs are relative to the repository root; a member is written
// "Type.Member".
type removal struct {
	dir, name     string // the removed identifier
	replDir, repl string // what its deprecation notice said to use instead
	issue         string
}

var removedBeforeV160 = []removal{
	{".", "Context.FormValueOk", ".", "Context.FormValueOK", "celeris#830"},
	{"driver/postgres", "ErrNoLastInsertId", "driver/postgres", "ErrNoLastInsertID", "celeris#830"},
	{"driver/postgres/internal/protocol", "SimpleQueryState.Tag", "driver/postgres/internal/protocol", "SimpleQueryState.TagBytes", "celeris#830"},
	{"internal/engine", "EngineMetrics.Throughput", "internal/engine", "EngineMetrics.RequestCount", "celeris#830"},
	{"middleware/csrf", "Storage", "middleware/store", "KV", "celeris#830"},
	{"middleware/csrf", "MemoryStorageConfig", "middleware/store", "MemoryKVConfig", "celeris#830"},
	{"middleware/session", "Store", "middleware/store", "KV", "celeris#830"},
	{"middleware/session", "MemoryStoreConfig", "middleware/store", "MemoryKVConfig", "celeris#830"},
	{"middleware/ratelimit", "Config.LimitReached", "middleware/ratelimit", "Config.ErrorHandler", "celeris#830"},
	{"middleware/basicauth", "HashPassword", "middleware/basicauth", "HashPasswordPBKDF2", "celeris#826"},
}

// TestNoDeprecatedPublicAPI830 pins celeris#830 and celeris#826. celeris goes
// public with v1.6.0, and after that removing an exported identifier needs a
// /v2 import path, so every deprecated public API went before it. Two things
// keep that true:
//
//   - no comment in a Go file outside an internal/ tree carries a deprecation
//     paragraph that deprecationsAllowedInV1 does not list, so nothing public
//     is on its way out unnoticed (internal packages are not public API:
//     driver/internal/async keeps one on purpose);
//   - none of the removed identifiers is declared again, even without a
//     notice: a re-added alias breaks no build and silently undoes the
//     removal.
//
// As positive controls, the walk must see a plausible number of files and
// find each removal's replacement declared where the old notice pointed, so a
// walk that missed a package cannot pass; TestDeprecationScanner830 checks the
// two detectors themselves on fixtures.
func TestNoDeprecatedPublicAPI830(t *testing.T) {
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil || !strings.HasPrefix(string(mod), "module github.com/goceleris/celeris\n") {
		t.Fatalf("expected the celeris module root at %s (go.mod: %v)", root, err)
	}

	fset := token.NewFileSet()
	notices := map[string][]int{}           // file -> lines of its notices
	decls := map[string]map[string]string{} // dir -> name -> position
	files, exempt := 0, 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			// Hidden directories hold no package of ours, and a checkout's
			// .claude/worktrees holds whole copies of the tree at other
			// commits.
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files++
		dir := filepath.ToSlash(filepath.Dir(rel))
		for _, line := range deprecationNotices(fset, f) {
			if isInternalDir(dir) {
				exempt++
				continue
			}
			notices[rel] = append(notices[rel], line)
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		for name, line := range declaredNames(fset, f) {
			if decls[dir] == nil {
				decls[dir] = map[string]string{}
			}
			decls[dir][name] = rel + ":" + strconv.Itoa(line)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("walked %d Go files; %d deprecation notice(s) inside internal packages (exempt)", files, exempt)
	if files < 500 {
		t.Fatalf("walked only %d Go files under %s: the walk is not seeing the repository", files, root)
	}
	for _, n := range unallowedDeprecations(notices, deprecationsAllowedInV1) {
		t.Errorf("%s: before the v1.6.0 tag, remove the identifier (celeris#830); after it, keep the identifier until v2 and list its file in deprecationsAllowedInV1 with the issue that removes it in v2", n)
	}
	for _, r := range removedBeforeV160 {
		if pos, ok := decls[r.dir][r.name]; ok {
			t.Errorf("%s.%s is declared again at %s; it was removed before v1.6.0 (%s), use %s.%s", pkgOf(r.dir), r.name, pos, r.issue, pkgOf(r.replDir), r.repl)
		}
		if _, ok := decls[r.replDir][r.repl]; !ok {
			t.Errorf("positive control: the walk did not find the replacement %s.%s, so the check above proves nothing for %s.%s", pkgOf(r.replDir), r.repl, pkgOf(r.dir), r.name)
		}
	}
}

// TestDeprecationScanner830 checks the two detectors TestNoDeprecatedPublicAPI830
// relies on, on fixtures whose answers are known: every declaration form that
// can carry a notice is found (and nothing else), and every form a removed
// identifier could come back as is reported by name.
func TestDeprecationScanner830(t *testing.T) {
	src := `package fixture

// A is fine.
func A() {}

// B is going away.
//
// ` + deprecationMarker + ` use A.
func B() {}

// C mentions the word mid-paragraph: see ` + deprecationMarker + ` this is no notice.
func C() {}

type T struct {
	// F is going away.
	//
	// ` + deprecationMarker + ` use G.
	F int
	G int
}

// M is going away.
//
// ` + deprecationMarker + ` gone.
func (t *T) M() {}

var (
	// V is going away.
	//
	// ` + deprecationMarker + ` gone.
	V = 1
	W = 2
)

// Alias is going away.
//
// ` + deprecationMarker + ` use T.
type Alias = T

type I interface {
	// Do is going away.
	//
	// ` + deprecationMarker + ` gone.
	Do()
}

// E is going away; the notice ends its line.
//
// ` + deprecationMarker + `
// use A.
func E() {}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	// Lines of the notice paragraphs above, in source order: B, F, M, V,
	// Alias, I.Do, E. C's mid-paragraph mention must not count.
	want := []int{8, 17, 24, 30, 37, 43, 49}
	if got := deprecationNotices(fset, f); !slices.Equal(got, want) {
		t.Fatalf("deprecationNotices = %v, want %v", got, want)
	}

	names := declaredNames(fset, f)
	for _, n := range []string{"A", "B", "C", "T", "T.F", "T.G", "T.M", "V", "W", "Alias", "I", "I.Do", "E"} {
		if _, ok := names[n]; !ok {
			t.Errorf("declaredNames missed %q; got %v", n, names)
		}
	}
	if len(names) != 13 {
		t.Errorf("declaredNames found %d names, want 13: %v", len(names), names)
	}

	// The allowlist covers a file only with its v2 removal issue and the
	// exact notice count, and an entry whose file no longer matches is
	// reported too.
	gotBad := unallowedDeprecations(map[string][]int{"a.go": {3, 9}, "b.go": {5}, "c.go": {7}, "d.go": {2}}, map[string]allowedDeprecations{
		"a.go":    {2, "celeris#1"},
		"b.go":    {2, "celeris#2"},
		"c.go":    {1, ""},
		"gone.go": {1, "celeris#3"},
	})
	wantBad := []string{
		"b.go: deprecationsAllowedInV1 allows 2 notice(s) (v2 removal \"celeris#2\"), the file has 1",
		"b.go:5: deprecation notice outside internal packages",
		"c.go: deprecationsAllowedInV1 allows 1 notice(s) (v2 removal \"\"), the file has 1",
		"c.go:7: deprecation notice outside internal packages",
		"d.go:2: deprecation notice outside internal packages",
		"gone.go: deprecationsAllowedInV1 allows 1 notice(s) (v2 removal \"celeris#3\"), the file has 0",
	}
	if !slices.Equal(gotBad, wantBad) {
		t.Errorf("unallowedDeprecations =\n%s\nwant\n%s", strings.Join(gotBad, "\n"), strings.Join(wantBad, "\n"))
	}

	for dir, want := range map[string]bool{
		"internal":               true,
		"driver/internal/async":  true,
		"middleware/internal":    true,
		"middleware/internalish": false,
		"driver/postgres":        false,
		".":                      false,
	} {
		if got := isInternalDir(dir); got != want {
			t.Errorf("isInternalDir(%q) = %v, want %v", dir, got, want)
		}
	}
}

// deprecationNotices returns the line of every comment paragraph in f that
// opens with the deprecation marker, the form go doc recognises. Every comment
// is read, not only doc comments, so a notice cannot hide in a floating
// comment.
func deprecationNotices(fset *token.FileSet, f *ast.File) []int {
	var lines []int
	for _, cg := range f.Comments {
		start := true // the first line of a comment group opens a paragraph
		for _, c := range cg.List {
			first := fset.Position(c.Pos()).Line
			text := strings.TrimSuffix(c.Text, "*/")
			for i, l := range strings.Split(text, "\n") {
				l = strings.TrimPrefix(l, "//")
				l = strings.TrimPrefix(l, "/*")
				l = strings.TrimSpace(l)
				if l == "" {
					start = true
					continue
				}
				if start && strings.HasPrefix(l, deprecationMarker) {
					lines = append(lines, first+i)
				}
				start = false
			}
		}
	}
	return lines
}

// unallowedDeprecations returns, sorted, each notice in found (file -> notice
// lines) that allowed does not cover, and each allowlist entry that does not
// match its file. An entry covers its file only if it names the v2 removal
// issue and the file carries exactly that many notices, so a new notice in a
// listed file is still reported.
func unallowedDeprecations(found map[string][]int, allowed map[string]allowedDeprecations) []string {
	var out []string
	for file, lines := range found {
		if a, ok := allowed[file]; ok && a.v2Removal != "" && a.notices == len(lines) {
			continue
		}
		for _, l := range lines {
			out = append(out, file+":"+strconv.Itoa(l)+": deprecation notice outside internal packages")
		}
	}
	for file, a := range allowed {
		if a.v2Removal == "" || a.notices != len(found[file]) {
			out = append(out, fmt.Sprintf("%s: deprecationsAllowedInV1 allows %d notice(s) (v2 removal %q), the file has %d", file, a.notices, a.v2Removal, len(found[file])))
		}
	}
	slices.Sort(out)
	return out
}

// declaredNames returns every top-level name f declares, with methods,
// struct fields and interface methods written "Type.Member", mapped to the
// line of the declaration.
func declaredNames(fset *token.FileSet, f *ast.File) map[string]int {
	names := map[string]int{}
	add := func(name string, pos token.Pos) { names[name] = fset.Position(pos).Line }
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil && len(d.Recv.List) > 0 {
				add(recvTypeName(d.Recv.List[0].Type)+"."+d.Name.Name, d.Name.Pos())
				continue
			}
			add(d.Name.Name, d.Name.Pos())
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.ValueSpec:
					for _, n := range s.Names {
						add(n.Name, n.Pos())
					}
				case *ast.TypeSpec:
					add(s.Name.Name, s.Name.Pos())
					var members *ast.FieldList
					switch typ := s.Type.(type) {
					case *ast.StructType:
						members = typ.Fields
					case *ast.InterfaceType:
						members = typ.Methods
					}
					if members == nil {
						continue
					}
					for _, m := range members.List {
						for _, n := range m.Names {
							add(s.Name.Name+"."+n.Name, n.Pos())
						}
					}
				}
			}
		}
	}
	return names
}

// recvTypeName returns the base type name of a method receiver: T for T,
// *T, T[K] and *T[K].
func recvTypeName(e ast.Expr) string {
	for {
		switch x := e.(type) {
		case *ast.StarExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		case *ast.Ident:
			return x.Name
		default:
			return ""
		}
	}
}

// isInternalDir reports whether the package in dir (slash-separated, relative
// to the repository root) sits in an internal/ tree, which the go command
// keeps out of the public API.
func isInternalDir(dir string) bool {
	return slices.Contains(strings.Split(dir, "/"), "internal")
}

// pkgOf names the package in dir for a message: "celeris" for the root,
// otherwise its import path below the module.
func pkgOf(dir string) string {
	if dir == "." {
		return "celeris"
	}
	return dir
}
