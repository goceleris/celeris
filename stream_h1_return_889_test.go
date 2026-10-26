package celeris

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/conn"
)

// The router in front of the native HTTP/1 engines' conn layer (ProcessH1):
// a StreamWriter handler that returns without Close ends its chunked response
// with the last chunk; one that panicked or returned an error after its
// headers is not ended (the router cannot write a 500 into the body, and a last
// chunk would make the truncated body look whole): the connection is closed
// (celeris#889).
func TestRouterStreamWriterHandlerReturnH1889(t *testing.T) {
	srv := New(Config{})
	srv.GET("/noclose", func(c *Context) error {
		sw := c.StreamWriter()
		if err := sw.WriteHeader(200, nil); err != nil {
			return err
		}
		_, err := sw.Write([]byte("hello"))
		return err
	})
	srv.GET("/close", func(c *Context) error {
		sw := c.StreamWriter()
		_ = sw.WriteHeader(200, nil)
		_, _ = sw.Write([]byte("hello"))
		return sw.Close()
	})
	srv.GET("/panic", func(c *Context) error {
		sw := c.StreamWriter()
		_ = sw.WriteHeader(200, nil)
		_, _ = sw.Write([]byte("hello"))
		panic("boom889")
	})
	srv.GET("/err", func(c *Context) error {
		sw := c.StreamWriter()
		_ = sw.WriteHeader(200, nil)
		_, _ = sw.Write([]byte("hello"))
		return errors.New("boom889")
	})
	srv.GET("/ok", func(c *Context) error { return c.String(200, "ok") })
	srv.HEAD("/headnoclose", func(c *Context) error {
		sw := c.StreamWriter()
		_ = sw.WriteHeader(200, nil)
		_, _ = sw.Write([]byte("hello"))
		return nil
	})
	adapter := &routerAdapter{server: srv}

	do := func(in string) (string, error) {
		var out bytes.Buffer
		state := conn.NewH1State()
		err := conn.ProcessH1(context.Background(), []byte(in), state, adapter, func(b []byte) { out.Write(b) })
		conn.CloseH1(state)
		return out.String(), err
	}
	get := func(path string) string { return "GET " + path + " HTTP/1.1\r\nHost: x\r\n\r\n" }

	for _, path := range []string{"/noclose", "/close"} {
		out, err := do(get(path) + get("/ok"))
		if err != nil {
			t.Errorf("%s: %v", path, err)
		}
		if !strings.Contains(out, "5\r\nhello\r\n0\r\n\r\nHTTP/1.1 200 OK") || strings.Count(out, "0\r\n\r\n") != 1 || !strings.HasSuffix(out, "\r\n\r\nok") {
			t.Errorf("%s then /ok: want the chunked response ended once, then the next response: %q", path, out)
		}
	}
	for _, path := range []string{"/panic", "/err"} {
		out, err := do(get(path) + get("/ok"))
		if err == nil {
			t.Errorf("%s: ProcessH1 returned nil, the connection must close", path)
		}
		if strings.Contains(out, "0\r\n\r\n") || strings.Contains(out, "500") || strings.Contains(out, "ok") || !strings.HasSuffix(out, "5\r\nhello\r\n") {
			t.Errorf("%s: a broken-off response must be cut where it is, nothing after it: %q", path, out)
		}
	}
	out, err := do("HEAD /headnoclose HTTP/1.1\r\nHost: x\r\n\r\n" + get("/ok"))
	if err != nil || strings.Contains(out, "hello") || strings.Contains(out, "0\r\n\r\n") || !strings.HasSuffix(out, "\r\n\r\nok") {
		t.Errorf("HEAD then GET: err %v, %q", err, out)
	}
}

// A matched handler that returns without answering is answered with an empty
// 200, as net/http does and as the 404, 405 and OPTIONS paths do, including one
// that took a StreamWriter and never used it ("Taking the StreamWriter commits
// nothing by itself"): with nothing sent, the response behind it on the
// connection would be read as its answer. A handler that detached or hijacked
// answers for itself.
func TestRouterHandlerThatDoesNotAnswerH1889(t *testing.T) {
	srv := New(Config{})
	srv.GET("/nothing", func(c *Context) error { return nil })
	srv.GET("/takenonly", func(c *Context) error {
		_ = c.StreamWriter()
		return nil
	})
	srv.GET("/aborted", func(c *Context) error { c.Abort(); return nil })
	srv.GET("/detaches", func(c *Context) error {
		done := c.Detach()
		done()
		return nil
	})
	srv.GET("/clset", func(c *Context) error { c.SetHeader("content-length", "1234"); return nil })
	srv.HEAD("/hx", func(c *Context) error { c.SetHeader("content-length", "1234"); return nil })
	srv.GET("/interim", func(c *Context) error {
		return c.StreamWriter().WriteHeader(103, [][2]string{{"link", "</a.css>; rel=preload"}})
	})
	srv.GET("/interimerr", func(c *Context) error {
		_ = c.StreamWriter().WriteHeader(103, nil)
		return errors.New("boom889")
	})
	srv.GET("/ok", func(c *Context) error { return c.String(200, "ok") })
	adapter := &routerAdapter{server: srv}
	do := func(in string, release bool) string {
		var out bytes.Buffer
		state := conn.NewH1State()
		_ = conn.ProcessH1(context.Background(), []byte(in), state, adapter, func(b []byte) { out.Write(b) })
		if release {
			conn.CloseH1(state)
		}
		return out.String()
	}
	get := func(path string) string { return "GET " + path + " HTTP/1.1\r\nHost: x\r\n\r\n" }
	for _, path := range []string{"/nothing", "/takenonly", "/aborted"} {
		out := do(get(path)+get("/ok"), true)
		if n := strings.Count(out, "HTTP/1.1 200 OK"); n != 2 || !strings.Contains(out, "content-length: 0\r\n\r\nHTTP/1.1 200 OK") || !strings.HasSuffix(out, "\r\n\r\nok") {
			t.Errorf("%s then /ok: want an empty 200 and then the 200 \"ok\": %q", path, out)
		}
	}
	// A content-length the handler set is not sent next to the fallback's: two
	// of them are a framing error (RFC 9112 section 6.3), and the body is empty.
	out := do(get("/clset")+get("/ok"), true)
	if n := strings.Count(strings.ToLower(out), "content-length"); n != 2 || strings.Contains(out, "1234") || !strings.Contains(out, "content-length: 0\r\n\r\nHTTP/1.1 200 OK") || !strings.HasSuffix(out, "\r\n\r\nok") {
		t.Errorf("/clset then /ok: want one content-length: 0 on the first response: %q", out)
	}
	// a HEAD keeps the size its handler declared, and no content-length: 0 is added
	out = do("HEAD /hx HTTP/1.1\r\nHost: x\r\n\r\n"+get("/ok"), true)
	head, rest, _ := strings.Cut(out, "\r\n\r\n")
	if strings.Count(strings.ToLower(head), "content-length") != 1 || !strings.Contains(head, "content-length: 1234") || !strings.HasPrefix(rest, "HTTP/1.1 200 OK") || !strings.HasSuffix(out, "\r\n\r\nok") {
		t.Errorf("HEAD /hx then /ok: want one content-length: 1234 and then the 200 \"ok\": %q", out)
	}
	// a handler that sent only an interim response (1xx) is answered, with an
	// empty 200, or with a 500 when it failed after it
	out = do(get("/interim")+get("/ok"), true)
	if !strings.HasPrefix(out, "HTTP/1.1 103") || strings.Count(out, "HTTP/1.1 200 OK") != 2 || !strings.Contains(out, "content-length: 0\r\n\r\nHTTP/1.1 200 OK") {
		t.Errorf("/interim then /ok: want the 103, an empty 200, then the 200 \"ok\": %q", out)
	}
	out = do(get("/interimerr")+get("/ok"), true)
	if !strings.HasPrefix(out, "HTTP/1.1 103") || !strings.Contains(out, "HTTP/1.1 500") {
		t.Errorf("/interimerr: want the 103 and then a final 500: %q", out)
	}
	// a detached request is the taker's to answer: nothing is added
	// (the Context of a detached request is released by its own waiter, not by CloseH1)
	if out := do(get("/detaches"), false); strings.Contains(out, "HTTP/1.1") {
		t.Errorf("a handler that detached got a response it did not write: %q", out)
	}
}
