//go:build linux

package iouring

import (
	"context"
	"testing"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

type nopHandler1029 struct{ calls int }

func (h *nopHandler1029) HandleStream(context.Context, *stream.Stream) error {
	h.calls++
	return nil
}

// celeris#1029: a chunked body that spans recvs is kept in the conn's H1State,
// so the conn is not at a request boundary and the async transplant gate must
// refuse it, as it does a fixed-length body in progress.
func TestChunkedMidBodyNotTransplantable1029(t *testing.T) {
	w := &Worker{asyncCancelFlags: true}
	cs := &connState{detected: true, h1State: conn.NewH1State()}
	cs.protocol.Store(int32(engine.HTTP1))
	if !w.asyncTransplantEligible(cs) {
		t.Fatal("premise: an idle HTTP/1 keep-alive conn is eligible")
	}
	h := &nopHandler1029{}
	run := func(s string) {
		t.Helper()
		if err := conn.ProcessH1(context.Background(), []byte(s), cs.h1State, h, func([]byte) {}); err != nil {
			t.Fatal(err)
		}
	}
	run("POST /up HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhel")
	if w.asyncTransplantEligible(cs) {
		t.Fatal("mid chunk: the conn is eligible for transplant")
	}
	run("lo\r\n0\r\n")
	if w.asyncTransplantEligible(cs) {
		t.Fatal("before the final CRLF: the conn is eligible for transplant")
	}
	run("\r\n")
	if h.calls != 1 || !w.asyncTransplantEligible(cs) {
		t.Fatalf("after the body: handler calls %d, eligible %v, want 1, true", h.calls, w.asyncTransplantEligible(cs))
	}
}
