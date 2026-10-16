//go:build linux

package epoll

import (
	"context"
	"testing"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

type nopHandler1029 struct{ calls int }

func (h *nopHandler1029) HandleStream(context.Context, *stream.Stream) error {
	h.calls++
	return nil
}

// celeris#1029: a chunked body that spans recvs is kept in the conn's H1State
// (the head at the front of its buffer), so the conn is not at a request
// boundary and a transplant must refuse it: the destination's fresh parser
// would read the chunk bytes as a request head.
func TestChunkedMidBodyIsNotAtBoundary1029(t *testing.T) {
	l, _ := sweepLoop(t)
	_, cs := movableConn(t, l)
	if !l.flushedAtBoundary(cs) {
		t.Fatal("premise: a fresh keep-alive conn is at a boundary")
	}
	h := &nopHandler1029{}
	write := func([]byte) {}
	run := func(s string) {
		t.Helper()
		if err := conn.ProcessH1(context.Background(), []byte(s), cs.h1State, h, write); err != nil {
			t.Fatal(err)
		}
	}
	run("POST /up HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhel")
	if l.flushedAtBoundary(cs) || !cs.h1State.HasPendingData() {
		t.Fatalf("mid chunk: flushedAtBoundary=%v HasPendingData=%v, want false and true", l.flushedAtBoundary(cs), cs.h1State.HasPendingData())
	}
	run("lo\r\n")
	if l.flushedAtBoundary(cs) {
		t.Fatal("between chunks the conn is still inside the request")
	}
	run("0\r\n")
	if l.flushedAtBoundary(cs) {
		t.Fatal("before the final CRLF the conn is still inside the request")
	}
	run("\r\n")
	if h.calls != 1 || !l.flushedAtBoundary(cs) || cs.h1State.HasPendingData() {
		t.Fatalf("after the body: handler calls %d, boundary %v, pending %v, want 1, true, false", h.calls, l.flushedAtBoundary(cs), cs.h1State.HasPendingData())
	}
}
