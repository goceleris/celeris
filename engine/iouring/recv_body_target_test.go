//go:build linux

package iouring

import (
	"context"
	"testing"
	"unsafe"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/protocol/h2/stream"
)

// partialBodyH1State drives a real H1State into a partial-body state so
// NextRecvBuf returns a non-nil bodyBuf tail. It feeds a request whose
// Content-Length exceeds the body bytes supplied, leaving the parser
// accumulating into bodyBuf.
func partialBodyH1State(t *testing.T) *conn.H1State {
	t.Helper()
	st := conn.NewH1State()
	st.MaxRequestBodySize = 1 << 20
	handlerCalled := false
	h := stream.HandlerFunc(func(_ context.Context, _ *stream.Stream) error {
		handlerCalled = true
		return nil
	})
	// Content-Length 100, but only 10 body bytes provided → partial body.
	req := "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n0123456789"
	if err := conn.ProcessH1(context.Background(), []byte(req), st, h, func([]byte) {}); err != nil {
		t.Fatalf("ProcessH1 on partial body: %v", err)
	}
	if handlerCalled {
		t.Fatalf("handler fired on an incomplete body")
	}
	if st.NextRecvBuf() == nil {
		t.Fatalf("NextRecvBuf is nil — H1State not in a partial-body state")
	}
	return st
}

// TestPickRecvTargetUsesBodyBuf verifies the body-target selection that the
// dirty-list recv re-arm now relies on (v1.5.0 review 2.4). When the H1 parser
// is mid-body, pickRecvTarget must return the bodyBuf tail and set
// cs.recvIntoBody=true so handleRecv routes the next CQE through the
// direct-body path. Before the fix the dirty loop re-armed into cs.buf with
// recvIntoBody stuck true, corrupting the body.
func TestPickRecvTargetUsesBodyBuf(t *testing.T) {
	st := partialBodyH1State(t)
	cs := &connState{fd: 7, liveIdx: -1, buf: make([]byte, 4096), h1State: st}
	cs.protocol.Store(int32(engine.HTTP1))
	cs.detected = true

	w := &Worker{} // sync mode, no bufRing, not h1Only

	cs.recvIntoBody = true // simulate the stale flag the dirty retry must reconcile
	target := w.pickRecvTarget(cs)

	if !cs.recvIntoBody {
		t.Fatalf("recvIntoBody = false, want true (body target should be selected)")
	}
	body := st.NextRecvBuf()
	if len(target) == 0 {
		t.Fatalf("body target is empty")
	}
	if &target[0] == &cs.buf[0] {
		t.Fatalf("pickRecvTarget returned cs.buf, want the bodyBuf tail")
	}
	if len(target) != len(body) {
		t.Fatalf("target len %d, want body tail len %d", len(target), len(body))
	}
}

// TestBodyRecvPinSurvivesCloseH1 is the regression guard for v1.5.0 review 2.3
// (the #256 body-buffer use-after-free variant). When a single-shot recv is
// armed directly into H1State.bodyBuf, conn.CloseH1 nils H1State.bodyBuf on
// close — which would let GC reclaim the backing array while a kernel recv SQE
// still targets it. pickRecvTarget pins the array on cs.bodyRecvPin so it stays
// alive until the connState drains from pendingRelease; this test confirms the
// pin still references the original array after CloseH1.
func TestBodyRecvPinSurvivesCloseH1(t *testing.T) {
	st := partialBodyH1State(t)
	cs := &connState{fd: 7, liveIdx: -1, buf: make([]byte, 4096), h1State: st}
	cs.protocol.Store(int32(engine.HTTP1))
	cs.detected = true
	w := &Worker{} // sync mode, no bufRing

	target := w.pickRecvTarget(cs)
	if !cs.recvIntoBody {
		t.Fatalf("setup: recvIntoBody not set — body path not taken")
	}
	if cs.bodyRecvPin == nil {
		t.Fatalf("bodyRecvPin not set after arming a body recv")
	}
	if len(target) == 0 || len(cs.bodyRecvPin) == 0 {
		t.Fatalf("setup: empty target/pin")
	}
	pinAddr := uintptr(unsafe.Pointer(&cs.bodyRecvPin[0]))
	targetAddr := uintptr(unsafe.Pointer(&target[0]))
	if pinAddr != targetAddr {
		t.Fatalf("pin (0x%x) does not alias the armed recv target (0x%x)", pinAddr, targetAddr)
	}

	// Simulate the close path: CloseH1 drops H1State.bodyBuf. The pin must
	// keep the backing array reachable independently.
	conn.CloseH1(st)
	if cs.bodyRecvPin == nil {
		t.Fatalf("bodyRecvPin was cleared by CloseH1 — UAF window reopened")
	}
	if got := uintptr(unsafe.Pointer(&cs.bodyRecvPin[0])); got != pinAddr {
		t.Fatalf("pin moved after CloseH1: 0x%x → 0x%x", pinAddr, got)
	}
	// Write through the pin to prove the array is still valid memory.
	cs.bodyRecvPin[0] = 0xAB

	// releaseConnState (post-pendingRelease) must finally drop the pin.
	releaseConnState(cs)
	if cs.bodyRecvPin != nil {
		t.Errorf("releaseConnState did not clear bodyRecvPin")
	}
}

// TestPickRecvTargetFallsBackToBuf confirms the non-body case: with no H1
// partial body, pickRecvTarget returns cs.buf and clears recvIntoBody, so a
// dirty re-arm via pickRecvTarget behaves exactly like the old cs.buf arm.
func TestPickRecvTargetFallsBackToBuf(t *testing.T) {
	cs := &connState{fd: 8, liveIdx: -1, buf: make([]byte, 4096)}
	cs.recvIntoBody = true
	w := &Worker{}
	target := w.pickRecvTarget(cs)
	if cs.recvIntoBody {
		t.Errorf("recvIntoBody = true, want false on the cs.buf fallback")
	}
	if &target[0] != &cs.buf[0] {
		t.Errorf("pickRecvTarget did not return cs.buf on the fallback path")
	}
}
