package celeris_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// The limits and caveats of the public godoc that celeris#891 items 15, 16,
// 19, 24, 30 and 31 name. Each check reads the doc comment of one declaration
// with go/parser and matches the text after joining its lines with single
// spaces, so a sentence may wrap anywhere. Each check has a positive control
// (the declaration must be found and carry text), and each lists the stale
// wording it retires, so a restored claim fails as well as a missing one.

// docText returns the doc comment of the func or method name (receiver type
// recv, "" for a plain func), with its whitespace normalised.
func docText(t *testing.T, file, recv, name string) string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != name {
			continue
		}
		got := ""
		if fd.Recv != nil && len(fd.Recv.List) == 1 {
			got = docRecvName(fd.Recv.List[0].Type)
		}
		if got == recv && fd.Doc != nil {
			return normaliseDoc(fd.Doc.Text())
		}
	}
	t.Fatalf("positive control: %s has no documented %s.%s", file, recv, name)
	return ""
}

// typeDoc returns the doc comment of the type name, or of its field, with
// whitespace normalised. A field doc is what the field's own comment says.
func typeDoc(t *testing.T, file, typ, field string) string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, sp := range gd.Specs {
			ts := sp.(*ast.TypeSpec)
			if ts.Name.Name != typ {
				continue
			}
			if field == "" {
				doc := ts.Doc
				if doc == nil {
					doc = gd.Doc
				}
				if doc == nil {
					t.Fatalf("positive control: %s type %s has no doc", file, typ)
				}
				return normaliseDoc(doc.Text())
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				t.Fatalf("positive control: %s.%s is not a struct", file, typ)
			}
			for _, fl := range st.Fields.List {
				for _, n := range fl.Names {
					if n.Name == field && fl.Doc != nil {
						return normaliseDoc(fl.Doc.Text())
					}
				}
			}
		}
	}
	t.Fatalf("positive control: %s has no documented %s.%s", file, typ, field)
	return ""
}

func docRecvName(e ast.Expr) string {
	if s, ok := e.(*ast.StarExpr); ok {
		e = s.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func normaliseDoc(s string) string { return strings.Join(strings.Fields(s), " ") }

// requireDoc fails for each phrase that is missing from doc, and for each
// stale phrase that is still in it.
func requireDoc(t *testing.T, what, doc string, want, stale []string) {
	t.Helper()
	if len(doc) < 200 {
		t.Fatalf("positive control: %s doc is only %d bytes", what, len(doc))
	}
	for _, s := range want {
		if !strings.Contains(doc, s) {
			t.Errorf("%s: godoc lacks %q (celeris#891)", what, s)
		}
	}
	for _, s := range stale {
		if strings.Contains(doc, s) {
			t.Errorf("%s: godoc still says %q (celeris#891)", what, s)
		}
	}
}

// TestShutdownDocBounds891 pins celeris#891 items 16, 19 and 30 on the
// Server.Shutdown godoc: the WriteTimeout cap holds while it is set, the 250 ms
// floor is a minimum and the io_uring window a maximum, the std 503 and the
// per-engine budget rules are stated.
func TestShutdownDocBounds891(t *testing.T) {
	doc := docText(t, "server.go", "Server", "Shutdown")
	requireDoc(t, "Server.Shutdown", doc,
		[]string{
			"but no longer than [Config.WriteTimeout] while that is set",
			"for at least 250 ms, ending as soon as nothing is left to send",
			"for at most those 250 ms,",
			"answers 503 (Service Unavailable)",
			"On std the shortest budget governs",
			"the drain's budget is the ctx of the last Shutdown call",
			"Adaptive hands its ctx to both of its engines",
		},
		[]string{
			"never for less than 250 ms",
			"for those 250 ms",
			"The shortest governs",
			"capped by [Config.WriteTimeout], from the start",
		})
}

// TestHEADDocDetachCaveat891 pins celeris#891 item 24: the Server.HEAD godoc
// says what a streaming GET handler must check so that a HEAD request does not
// leave its goroutine writing into a finished response.
func TestHEADDocDetachCaveat891(t *testing.T) {
	doc := docText(t, "server.go", "Server", "HEAD")
	requireDoc(t, "Server.HEAD", doc,
		[]string{
			`c.Method() == "HEAD"`,
			"return after WriteHeader",
			"[Context.Detach]",
		},
		nil)
}

// TestWriteTimeoutDoc891 pins celeris#891 item 31 on Config.WriteTimeout: the
// closing-connection bound is 5 s on epoll and io_uring when the timeout is
// disabled, max(5s, WriteTimeout) on io_uring, restarts on each byte taken,
// and has no absolute limit.
func TestWriteTimeoutDoc891(t *testing.T) {
	doc := typeDoc(t, "config.go", "Config", "WriteTimeout")
	requireDoc(t, "Config.WriteTimeout", doc,
		[]string{
			"max(5s, WriteTimeout) (io_uring)",
			"5 s on either when WriteTimeout is -1",
			"there is no absolute limit on such a connection",
		},
		[]string{
			"epoll; 5s when it is 0",
			"the max duration for writing the response.",
		})
}

// TestResponseMemoryDoc891 pins celeris#891 item 15 on Config's response memory
// section, Context.File and Context.FileFromFS: the H2 and H1 bounds, the
// detached and StreamWriter caps, that a single Blob or File response is staged
// whole, and the conditions under which a File response is sent with sendfile(2)
// rather than read into memory (epoll, HTTP/1.1, at least 16 KiB, no async
// handlers, no buffering or capture), with the cases that read the file whole.
func TestResponseMemoryDoc891(t *testing.T) {
	cfg := typeDoc(t, "config.go", "Config", "")
	requireDoc(t, "Config (Response memory)", cfg,
		[]string{
			"# Response memory",
			"a single handler can stage a large one",
			"a [Context.Blob] of any size",
			"up to 64 MiB of unsent response data",
			"more than 4 MiB of responses on its connection are unsent",
			"A detached connection (WebSocket, SSE) may queue 64 MiB",
			"A [Context.File] response is limited to 100 MB",
			"sendfile(2), and not read into memory, only on epoll (not io_uring, std or HTTP/2)",
			"an HTTP/1.1 response of at least 16 KiB",
			"neither [Config.AsyncHandlers] nor a route marked [Route.Async]",
			"[Context.BufferResponse], [Context.CaptureResponse]",
			"Any other File response is read whole into memory",
		},
		[]string{
			"Response memory section of",
			"not [Config.MaxRequestBodySize]",
			"cap what a slow client can make the server hold",
			"on epoll it is sent with sendfile(2), but",
		})
	file := docText(t, "context_response.go", "Context", "File")
	requireDoc(t, "Context.File", file,
		[]string{
			"capped at 100 MB",
			"sendfile(2), not read into memory, only on epoll",
			"at least 16 KiB",
			"no async handlers",
			"buffers or captures the response",
			"Response memory section of [Config]",
			"io_uring, std, HTTP/2, an async route, a buffered or captured response",
		},
		[]string{
			"The entire file is loaded into memory",
			"On epoll the file is sent with sendfile(2)",
		})
	fromFS := docText(t, "context_response.go", "Context", "FileFromFS")
	requireDoc(t, "Context.FileFromFS", fromFS,
		[]string{
			"capped at 100 MB",
			"sendfile(2) only where that applies",
			"an *os.File",
			"otherwise the whole file is read into memory",
		},
		[]string{"sendfile(2) on epoll for a file backed"})
}
