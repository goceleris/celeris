package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// TestThroughputIsDeprecatedAsAlwaysZero pins celeris#653.
//
// EngineMetrics.Throughput was exported and documented as "the recent
// requests-per-second rate", and no engine ever assigned it: std, epoll and
// io_uring never wrote it, and adaptive summed the two sub-engines' zeros. A
// field that is exported, documented as a measurement and always zero cannot
// be told apart from a real measurement of zero. Removing it is a breaking
// change, which belongs to v2.0.0 (celeris#651), so v1.6.0 deprecates it: the
// doc must say it always reads 0, why, and what to use instead.
func TestThroughputIsDeprecatedAsAlwaysZero(t *testing.T) {
	doc := fieldDoc(t, "engine.go", "EngineMetrics", "Throughput")
	var deprecated string
	for _, para := range strings.Split(doc, "\n\n") {
		if strings.HasPrefix(para, "Deprecated: ") {
			deprecated = para
		}
	}
	if deprecated == "" {
		t.Fatalf("EngineMetrics.Throughput has no \"Deprecated: \" paragraph; its doc reads:\n%s", doc)
	}
	for _, want := range []string{"always", "0", "RequestCount", "celeris#651"} {
		if !strings.Contains(deprecated, want) {
			t.Errorf("the Deprecated paragraph does not mention %q; it reads:\n%s", want, deprecated)
		}
	}
	if _, ok := reflect.TypeOf(EngineMetrics{}).FieldByName("Throughput"); !ok {
		t.Error("EngineMetrics.Throughput is gone: removing it is a breaking change for v2.0.0 (celeris#651), not a v1.6.0 change")
	}
}

// TestNothingSetsThroughput keeps the deprecation's claim true: the field reads
// 0 because nothing in the tree writes it. If something starts to (or to read
// it and pass it on, as adaptive's aggregator did), the doc comment and this
// test have to change together. It walks every non-test Go file of the
// repository, nested modules included, and reports any composite-literal key or
// selector named Throughput. The declaration itself is a struct field, neither.
func TestNothingSetsThroughput(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected the repository root at %s: %v", root, err)
	}
	fset := token.NewFileSet()
	var hits []string
	files := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Hidden directories hold no package of ours, and a checkout's
			// .claude/worktrees holds whole copies of the tree at other
			// commits, which would be reported as sites of this one.
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "vendor" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files++
		ast.Inspect(f, func(n ast.Node) bool {
			var id *ast.Ident
			switch x := n.(type) {
			case *ast.KeyValueExpr:
				id, _ = x.Key.(*ast.Ident)
			case *ast.SelectorExpr:
				id = x.Sel
			}
			if id != nil && id.Name == "Throughput" {
				rel, _ := filepath.Rel(root, fset.Position(id.Pos()).Filename)
				hits = append(hits, rel+":"+strconv.Itoa(fset.Position(id.Pos()).Line))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 100 {
		t.Fatalf("walked only %d non-test Go files under %s: the walk is not seeing the repository", files, root)
	}
	if len(hits) > 0 {
		t.Errorf("EngineMetrics.Throughput is deprecated as always 0, but %d site(s) touch it: %v", len(hits), hits)
	}
}

// fieldDoc returns the doc comment of field on struct typ declared in file.
func fieldDoc(t *testing.T, file, typ, field string) string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var doc string
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != typ {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return false
		}
		for _, fld := range st.Fields.List {
			for _, name := range fld.Names {
				if name.Name == field {
					found = true
					doc = fld.Doc.Text()
				}
			}
		}
		return false
	})
	if !found {
		t.Fatalf("%s.%s not found in %s", typ, field, file)
	}
	return strings.TrimSpace(doc)
}
