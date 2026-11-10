package celeris

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// celeris#889 item 24 for ToHandler: the stream's context is bound to the
// request's (celeris#949) only when the handler asks for c.Context(), and the
// binding is removed when the request ends.

// parentCtx889 is a request context that counts registrations made through
// context.AfterFunc, which is what binding a stream's context to it is.
type parentCtx889 struct {
	context.Context
	registered atomic.Int32
	stopped    atomic.Int32
}

// Value hides the embedded context's cancelCtx from the context package, which
// would otherwise register a child with it directly and never call AfterFunc.
func (p *parentCtx889) Value(any) any { return nil }

func (p *parentCtx889) AfterFunc(f func()) (stop func() bool) {
	p.registered.Add(1)
	inner := context.AfterFunc(p.Context, f)
	return func() bool {
		p.stopped.Add(1)
		return inner()
	}
}

func newParent889() (*parentCtx889, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	return &parentCtx889{Context: ctx}, cancel
}

func TestToHandlerBindsContextOnlyWhenAsked889(t *testing.T) {
	for _, major := range []int{1, 2} {
		for _, asks := range []bool{false, true} {
			name := map[int]string{1: "h1-", 2: "h2-"}[major] + map[bool]string{false: "never-asks", true: "asks"}[asks]
			t.Run(name, func(t *testing.T) {
				h := ToHandler(func(c *Context) error {
					if asks {
						_ = c.Context().Err()
					}
					return c.String(200, "ok")
				})
				parent, cancel := newParent889()
				defer cancel()
				req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(parent)
				req.ProtoMajor = major
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				want := int32(0)
				if asks {
					want = 1 // ToHandler binds on HTTP/1 as well
				}
				if got := parent.registered.Load(); got != want {
					t.Fatalf("registrations on the request's context = %d, want %d", got, want)
				}
				if got := parent.stopped.Load(); got != want {
					t.Fatalf("removals at the end of the request = %d, want %d", got, want)
				}
			})
		}
	}
}

// The lazy binding still ends c.Context() with the request's context.
func TestToHandlerContextEndsWithTheRequest889(t *testing.T) {
	parent, cancel := newParent889()
	defer cancel()
	got := make(chan error, 1)
	h := ToHandler(func(c *Context) error {
		ctx := c.Context()
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()
		select {
		case <-ctx.Done():
			got <- ctx.Err()
		case <-time.After(5 * time.Second):
			got <- errors.New("c.Context() did not end with the request's context")
		}
		return c.String(200, "ok")
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(parent)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if err := <-got; !errors.Is(err, context.Canceled) {
		t.Fatalf("Err = %v, want context.Canceled", err)
	}
	// And one asked for after the request's context ended.
	parent2, cancel2 := newParent889()
	cancel2()
	var err2 error
	h = ToHandler(func(c *Context) error { err2 = c.Context().Err(); return c.String(200, "ok") })
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil).WithContext(parent2))
	if !errors.Is(err2, context.Canceled) {
		t.Fatalf("context asked for after the request's ended: Err = %v, want context.Canceled", err2)
	}
}

// celeris#889 item 36, premise for ToHandler: its body cap (100 MB) is far
// above the 256 KiB net/http drains from an unread body, so a body refused by
// its declared length is never one the server waits for: the connection ends
// with the answer, though the client holds its socket open.
func TestToHandlerEarly413EndsTheConnection889(t *testing.T) {
	srv := httptest.NewServer(ToHandler(func(c *Context) error { return c.String(200, "ok") }))
	defer srv.Close()
	conn, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(conn, "POST /x HTTP/1.1\r\nHost: t\r\nContent-Length: 104857601\r\n\r\n")
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(status, "HTTP/1.1 413") {
		t.Fatalf("status %q, err %v, want 413", status, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.Copy(io.Discard, br); err != nil {
		t.Fatalf("the connection stayed open after the 413: %v", err)
	}
}
