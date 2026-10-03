package handoff_test

import (
	"errors"
	"fmt"
	"testing"
	"unsafe"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/internal/handoff"
)

// view returns a string that shares b's bytes, as a request string on epoll
// and io_uring shares the connection's receive buffer.
func view(b []byte) string { return unsafe.String(&b[0], len(b)) }

type fieldError struct{ id string }

func (e *fieldError) Error() string { return "field " + e.id }

// TestErrorCopiesWhatAWaiterReads pins celeris#732's coalescing sites: a
// handed-off error keeps its message, and an HTTPError found with errors.As
// its Message, after the bytes the original was built from change, while
// errors.Is still finds what the original wraps.
func TestErrorCopiesWhatAWaiterReads(t *testing.T) {
	if handoff.Error(nil) != nil {
		t.Fatal("Error(nil) is not nil")
	}

	buf := []byte("err-aaaa")
	orig := errors.New(view(buf))
	h := handoff.Error(orig)
	copy(buf, "err-cccc")
	if orig.Error() != "err-cccc" {
		t.Fatalf("the fixture is not a view: %q", orig.Error())
	}
	if h.Error() != "err-aaaa" {
		t.Errorf("handed-off message %q, want err-aaaa", h.Error())
	}
	if !errors.Is(h, orig) || errors.Unwrap(h) != orig {
		t.Error("the handed-off error does not unwrap to the original")
	}

	// An HTTPError wrapping a sentinel, itself wrapped by fmt.Errorf.
	sentinel := errors.New("sentinel")
	hbuf := []byte("msg-aaaa")
	he := celeris.NewHTTPError(400, view(hbuf)).WithError(sentinel)
	h = handoff.Error(fmt.Errorf("outer: %w", he))
	copy(hbuf, "msg-cccc")
	if want := "outer: code=400, message=msg-aaaa, err=sentinel"; h.Error() != want {
		t.Errorf("handed-off message %q, want %q", h.Error(), want)
	}
	var got *celeris.HTTPError
	if !errors.As(h, &got) {
		t.Fatal("errors.As finds no HTTPError")
	}
	if got == he || got.Code != 400 || got.Message != "msg-aaaa" {
		t.Errorf("errors.As found %p code %d message %q; want a copy of %p with code 400, message msg-aaaa", got, got.Code, got.Message, he)
	}
	if !errors.Is(h, he) || !errors.Is(h, sentinel) || !errors.Is(got, sentinel) {
		t.Error("errors.Is does not find the original HTTPError and its sentinel")
	}
	if !errors.Is(handoff.Error(celeris.ErrUnauthorized), celeris.ErrUnauthorized) {
		t.Error("errors.Is does not find celeris.ErrUnauthorized")
	}

	// errors.As to another type finds the original value.
	fe := &fieldError{id: "f"}
	var gotFE *fieldError
	if !errors.As(handoff.Error(fe), &gotFE) || gotFE != fe {
		t.Error("errors.As to a non-HTTPError type does not find the original")
	}
}

// TestPanicCopiesStringsAndErrors pins the panic value a waiter re-panics
// with.
func TestPanicCopiesStringsAndErrors(t *testing.T) {
	buf := []byte("pv-aaaa")
	p := handoff.Panic(view(buf))
	ebuf := []byte("pe-aaaa")
	e := errors.New(view(ebuf))
	pe := handoff.Panic(e)
	copy(buf, "pv-cccc")
	copy(ebuf, "pe-cccc")
	if s, ok := p.(string); !ok || s != "pv-aaaa" {
		t.Errorf("string panic handed off as %#v, want \"pv-aaaa\"", p)
	}
	if err, ok := pe.(error); !ok || err.Error() != "pe-aaaa" || !errors.Is(err, e) {
		t.Errorf("error panic handed off as %v", pe)
	}
	if v := handoff.Panic(42); v != 42 {
		t.Errorf("Panic(42) = %v", v)
	}
	if handoff.Panic(nil) != nil {
		t.Error("Panic(nil) is not nil")
	}
}
