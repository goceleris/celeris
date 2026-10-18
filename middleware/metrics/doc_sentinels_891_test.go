package metrics

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestPackageDocNamesFullPathSentinels891 pins celeris#891 item 25: the
// package doc names every FullPath sentinel that the path label can carry
// (Context.FullPath), "<options>" included, which no test asserted before.
// The doc is read with go/parser, so the pin needs no request harness; a
// positive control checks that a package doc was found at all.
func TestPackageDocNamesFullPathSentinels891(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "doc.go", nil, parser.ParseComments|parser.PackageClauseOnly)
	if err != nil {
		t.Fatal(err)
	}
	if f.Doc == nil {
		t.Fatal("doc.go has no package doc comment")
	}
	doc := f.Doc.Text()
	if !strings.Contains(doc, "Registered Metrics") {
		t.Fatalf("positive control: the package doc was not read (got %d bytes)", len(doc))
	}
	for _, s := range []string{`"<unmatched>"`, `"<method-not-allowed>"`, `"<options>"`} {
		if !strings.Contains(doc, s) {
			t.Errorf("package doc does not name the FullPath sentinel %s (celeris#891 item 25)", s)
		}
	}
}
