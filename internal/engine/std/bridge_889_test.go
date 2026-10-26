package std

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

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// These tests pin three items of celeris#889 for the std bridge: item 24 (the
// stream's context is bound to the request's context only when the handler
// asks for it), item 36 (an early 413 does not leave the connection's
// goroutine draining a body that never comes) and item 42 (the HTTP/2
// preface is not a request).

// parentCtx889 is a request context that counts how the stream's binding
// touches it: registrations through context.AfterFunc (the context package
// calls AfterFunc on a context that has the method) and their removals.
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

// newParent889 is a cancelable context whose bindings are counted.
func newParent889() (*parentCtx889, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	return &parentCtx889{Context: ctx}, cancel
}

// h2Bridge889 is a bridge on a std engine with handler h.
func h2Bridge889(t testing.TB, h stream.Handler) *Bridge {
	t.Helper()
	e, err := New(resource.Config{Addr: "127.0.0.1:0", Engine: engine.Std, Protocol: engine.HTTP1}, h)
	if err != nil {
		t.Fatal(err)
	}
	return &Bridge{engine: e, handler: h}
}

// ctxAskHandler889 is a handler that asks the stream for its context, as
// Context.Context does, and runs fn with it.
type ctxAskHandler889 func(ctx context.Context)

func (f ctxAskHandler889) HandleStream(_ context.Context, s *stream.Stream) error {
	f(s.Context())
	return s.ResponseWriter.WriteResponse(s, 200, nil, []byte("ok"))
}

// TestBridgeBindsStreamContextOnlyWhenAsked889 (item 24): a handler that never
// calls Context() on an h2c request costs the request's context nothing: no
// registration on it. A handler that does call it is bound, and the binding
// is removed when the request ends.
func TestBridgeBindsStreamContextOnlyWhenAsked889(t *testing.T) {
	for _, major := range []int{1, 2} {
		for _, asks := range []bool{false, true} {
			name := "HTTP" + string(rune('0'+major))
			if asks {
				name += "/asks"
			} else {
				name += "/never-asks"
			}
			t.Run(name, func(t *testing.T) {
				var h stream.Handler = okStreamHandler{}
				if asks {
					h = ctxAskHandler889(func(ctx context.Context) { _ = ctx.Err() })
				}
				br := h2Bridge889(t, h)
				parent, cancel := newParent889()
				defer cancel()
				req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(parent)
				req.ProtoMajor = major
				w := &discardResponseWriter{h: http.Header{}}
				br.ServeHTTP(w, req)

				wantReg := int32(0)
				if asks && major == 2 {
					wantReg = 1 // an HTTP/1 stream's context is context.Background(): nothing to bind
				}
				if got := parent.registered.Load(); got != wantReg {
					t.Fatalf("registrations on the request's context = %d, want %d", got, wantReg)
				}
				if got := parent.stopped.Load(); got != wantReg {
					t.Fatalf("removals of the registration at the end of the request = %d, want %d", got, wantReg)
				}
			})
		}
	}
}

// TestBridgeStreamContextEndsWithRequestContext889: the lazy binding still does
// what #949 added it for. The context the handler gets ends when the request's
// ends, whether that is before the handler asks or while it waits.
func TestBridgeStreamContextEndsWithRequestContext889(t *testing.T) {
	t.Run("cancelled-while-waiting", func(t *testing.T) {
		parent, cancel := newParent889()
		defer cancel()
		got := make(chan error, 1)
		br := h2Bridge889(t, ctxAskHandler889(func(ctx context.Context) {
			go func() {
				time.Sleep(20 * time.Millisecond)
				cancel()
			}()
			select {
			case <-ctx.Done():
				got <- ctx.Err()
			case <-time.After(5 * time.Second):
				got <- errors.New("the stream's context did not end with the request's")
			}
		}))
		req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(parent)
		req.ProtoMajor = 2
		br.ServeHTTP(&discardResponseWriter{h: http.Header{}}, req)
		if err := <-got; !errors.Is(err, context.Canceled) {
			t.Fatalf("Err = %v, want context.Canceled", err)
		}
	})
	t.Run("cancelled-before-asking", func(t *testing.T) {
		parent, cancel := newParent889()
		cancel()
		var err error
		var doneClosed bool
		br := h2Bridge889(t, ctxAskHandler889(func(ctx context.Context) {
			err = ctx.Err()
			select {
			case <-ctx.Done():
				doneClosed = true
			default:
			}
		}))
		req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(parent)
		req.ProtoMajor = 2
		br.ServeHTTP(&discardResponseWriter{h: http.Header{}}, req)
		if !errors.Is(err, context.Canceled) || !doneClosed {
			t.Fatalf("a context asked for after the request's ended: Err = %v, Done closed = %v", err, doneClosed)
		}
	})
}

// TestBridgeH2AllocsDoNotDependOnTheRequestContext889 (item 24, the cost the
// item is about): a request whose context is cancelable, which is every
// request net/http serves, allocates no more over HTTP/2 than over HTTP/1
// when the handler never asks for the context (the pooled stream and the
// bridge are the same for both; only the binding differs).
func TestBridgeH2AllocsDoNotDependOnTheRequestContext889(t *testing.T) {
	br := h2Bridge889(t, okStreamHandler{})
	base := httptest.NewRequest(http.MethodGet, "/", nil)
	w := &discardResponseWriter{h: http.Header{}}
	measure := func(major int) float64 {
		base.ProtoMajor = major
		return testing.AllocsPerRun(500, func() {
			ctx, cancel := context.WithCancel(context.Background())
			clear(w.h)
			br.ServeHTTP(w, base.WithContext(ctx))
			cancel()
		})
	}
	// Warm the pools.
	measure(1)
	measure(2)
	h1, h2 := measure(1), measure(2)
	t.Logf("allocs/op with a fresh cancelable context: HTTP/1 %.2f, HTTP/2 %.2f", h1, h2)
	// A margin of half an allocation for the pool's noise under -race
	// (sync.Pool drops a Put at random there); the binding costs about 8.
	if h2 > h1+0.5 {
		t.Fatalf("HTTP/2 allocates %.2f per request, HTTP/1 %.2f: the stream's context was bound to the request's though the handler never asked for it", h2, h1)
	}
}

// dialHold889 sends head, reads the response to the end of its header block,
// then waits for the server to close the connection while the client keeps its
// socket open and sends nothing more. It returns the status line and whether
// the server closed within wait.
func dialHold889(t *testing.T, addr, head string, wait time.Duration) (status string, closed bool) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, head); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(c)
	status, err = br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	// Whatever is left of the response, then the end of the connection.
	_ = c.SetReadDeadline(time.Now().Add(wait))
	_, err = io.Copy(io.Discard, br)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return status, false
	}
	return status, true
}

// TestEarly413ReleasesTheConnection889 (item 36): the answer to a body refused
// by its declared length is followed by the end of the connection, though the
// client holds its socket open and never sends the body. Before, net/http's
// finishRequest read up to 256 KiB of the unread body (a body whose declared
// length is within that), and a client that sent none kept the server's
// goroutine until ReadTimeout (none here).
func TestEarly413ReleasesTheConnection889(t *testing.T) {
	const declared = 8192
	post := "POST /x HTTP/1.1\r\nHost: std.test\r\nContent-Length: 8192\r\n"
	for _, tc := range []struct {
		name  string
		proto engine.Protocol
		head  string
	}{
		{"HTTP1", engine.HTTP1, post + "\r\n"},
		{"Auto", engine.Auto, post + "\r\n"},
		// Some of the body is on the wire when the answer goes out: the 413
		// must still arrive whole before the connection ends.
		{"HTTP1/partial-body", engine.HTTP1, post + "\r\n" + strings.Repeat("x", 100)},
		{"H2CUpgrade", engine.Auto, post + "Connection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: AAMAAABk\r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPathHandler()
			_, addr := startH2CEngineWith(t, h, nil, func(c *resource.Config) {
				c.Protocol = tc.proto
				c.MaxRequestBodySize = 1024
				c.SetH2Upgrade(true)
			})
			status, closed := dialHold889(t, addr, tc.head, 2*time.Second)
			if !strings.HasPrefix(status, "HTTP/1.1 413") {
				t.Fatalf("declared %d bytes over a 1024 limit answered %q, want 413", declared, status)
			}
			if !closed {
				t.Fatalf("the server kept the connection open after the 413: its goroutine waits for the %d-byte body the client never sends", declared)
			}
			if got := h.snapshot(); len(got) != 0 {
				t.Fatalf("handler saw %v for a refused request", got)
			}
		})
	}
}

// TestEarly413AboveTheDrainBound889 pins what makes item 36 limited to a
// declared length within 256 KiB: net/http gives up on a body declaring more
// than that without reading it, so the connection ends at once. It holds on
// main; ToHandler's cap (100 MB) is above the bound, so it is never in the
// drained range.
func TestEarly413AboveTheDrainBound889(t *testing.T) {
	_, addr := startH2CEngineWith(t, newPathHandler(), nil, func(c *resource.Config) {
		c.Protocol = engine.HTTP1
		c.MaxRequestBodySize = 1024
	})
	status, closed := dialHold889(t, addr,
		"POST /x HTTP/1.1\r\nHost: std.test\r\nContent-Length: 10000000\r\n\r\n", 2*time.Second)
	if !strings.HasPrefix(status, "HTTP/1.1 413") || !closed {
		t.Fatalf("status %q, closed %v: want 413 and a closed connection", status, closed)
	}
}

// TestHTTP2PrefaceIsNotARequest889 (item 42): "PRI * HTTP/2.0" is the start
// of the HTTP/2 connection preface, never a request. The native engines answer
// 400 to it and the handler never sees it; std handed it to the handler as
// method PRI, path *, and the router answered 404.
func TestHTTP2PrefaceIsNotARequest889(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proto engine.Protocol
		wire  string
	}{
		{"HTTP1/preface", engine.HTTP1, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"},
		{"HTTP1/preface-with-header", engine.HTTP1, "PRI * HTTP/2.0\r\nHost: std.test\r\nX-A: b\r\n\r\nSM\r\n\r\n"},
		// Prior-knowledge detection wants no headers: with one, the request
		// goes to the bridge.
		{"Auto/preface-with-header", engine.Auto, "PRI * HTTP/2.0\r\nHost: std.test\r\nX-A: b\r\n\r\nSM\r\n\r\n"},
		{"H2C/preface-with-header", engine.H2C, "PRI * HTTP/2.0\r\nHost: std.test\r\nX-A: b\r\n\r\nSM\r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPathHandler()
			_, addr := startH2CEngineWith(t, h, nil, func(c *resource.Config) { c.Protocol = tc.proto })
			got := firstReply889(t, addr, tc.wire)
			if !strings.HasPrefix(string(got), "HTTP/1.1 400") {
				t.Fatalf("%q answered %q, want HTTP/1.1 400", tc.wire, got)
			}
			if seen := h.snapshot(); len(seen) != 0 {
				t.Fatalf("the handler saw the preface as a request: %v", seen)
			}
		})
	}
}
