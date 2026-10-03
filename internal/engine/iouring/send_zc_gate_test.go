//go:build linux

package iouring

import (
	"testing"
	"unsafe"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/validation"
)

// TestUseSendZC pins the zero-copy gating policy (celeris#332): ZC is only
// chosen for unlinked sends whose payload is at least sendZCMinBytes. Below the
// threshold, or for any linked send, or when the capability is absent, the plain
// SEND path must win.
func TestUseSendZC(t *testing.T) {
	cases := []struct {
		name   string
		sendZC bool
		linked bool
		n      int
		want   bool
	}{
		{"large-unlinked-capable", true, false, sendZCMinBytes, true},
		{"above-threshold", true, false, sendZCMinBytes + 1, true},
		{"just-below-threshold", true, false, sendZCMinBytes - 1, false},
		{"small-unlinked", true, false, 100, false},
		{"empty", true, false, 0, false},
		{"large-linked", true, true, sendZCMinBytes * 4, false},
		{"large-no-capability", false, false, sendZCMinBytes * 4, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := useSendZC(tc.sendZC, tc.linked, tc.n); got != tc.want {
				t.Errorf("useSendZC(%v, %v, %d) = %v, want %v",
					tc.sendZC, tc.linked, tc.n, got, tc.want)
			}
		})
	}
}

// prepSendSQEOpcode runs prepSendSQE against a freshly zeroed SQE for a worker
// with ZC enabled and an unlinked send of payload size n, returning the chosen
// opcode byte.
func prepSendSQEOpcode(t *testing.T, n int) byte {
	t.Helper()
	var sqe [sqeSize]byte
	w := &Worker{sendZC: true}
	cs := &connState{fd: 7, sendBuf: make([]byte, n)}
	w.prepSendSQE(unsafe.Pointer(&sqe[0]), cs, false)
	return sqe[0]
}

// TestPrepSendSQEGatesBySize verifies the worker async flush path emits a plain
// SEND for sub-threshold responses (1 CQE, no NOTIF stall) and SEND_ZC only once
// the payload reaches sendZCMinBytes.
func TestPrepSendSQEGatesBySize(t *testing.T) {
	if op := prepSendSQEOpcode(t, 100); op != opSEND {
		t.Errorf("small response opcode = %d, want opSEND(%d)", op, opSEND)
	}
	if op := prepSendSQEOpcode(t, sendZCMinBytes-1); op != opSEND {
		t.Errorf("just-below-threshold opcode = %d, want opSEND(%d)", op, opSEND)
	}
	if op := prepSendSQEOpcode(t, sendZCMinBytes); op != opSENDZC {
		t.Errorf("at-threshold opcode = %d, want opSENDZC(%d)", op, opSENDZC)
	}
}

// TestPrepSendSQELinkedNeverZC guards the link invariant: a linked send must
// never use SEND_ZC (the NOTIF CQE would break the SEND→RECV chain), regardless
// of payload size.
func TestPrepSendSQELinkedNeverZC(t *testing.T) {
	var sqe [sqeSize]byte
	w := &Worker{sendZC: true}
	cs := &connState{fd: 7, sendBuf: make([]byte, sendZCMinBytes*4)}
	w.prepSendSQE(unsafe.Pointer(&sqe[0]), cs, true)
	if sqe[0] != opSEND {
		t.Errorf("linked large send opcode = %d, want opSEND(%d)", sqe[0], opSEND)
	}
	if sqe[1]&sqeIOLink == 0 {
		t.Errorf("linked send missing IOSQE_IO_LINK in flags 0x%02x", sqe[1])
	}
}

// zcWitness is one prepSendSQE observation: the opcode the SQE ended up
// carrying plus the celeris#591 exposure witnesses that the call moved.
type zcWitness struct {
	opcode          byte
	engineSubmits   uint64 // EngineMetrics.ZCSendsSubmitted delta
	vSubmits        uint64 // validation.IouringSendZCSubmits delta
	vSubmitsDetachd uint64 // validation.IouringSendZCSubmitsDetached delta
}

// prepSendSQEWitness runs prepSendSQE once against a freshly zeroed SQE for
// a worker with ZC available and an unlinked send of n payload bytes, and
// reports the opcode together with the witness deltas the call produced.
// detached decides whether the connection carries an H1 state already
// handed to a middleware goroutine, which is what keys the *Detached split.
func prepSendSQEWitness(t *testing.T, n int, detached bool) zcWitness {
	t.Helper()
	var sqe [sqeSize]byte
	zc := &zcStats{}
	w := &Worker{sendZC: true, zc: zc}
	cs := &connState{fd: 7, sendBuf: make([]byte, n)}
	if detached {
		h1 := &conn.H1State{}
		h1.Detached.Store(true)
		cs.h1State = h1
	}
	before := validation.Snapshot()
	w.prepSendSQE(unsafe.Pointer(&sqe[0]), cs, false)
	after := validation.Snapshot()
	return zcWitness{
		opcode:          sqe[0],
		engineSubmits:   zc.submits.Load(),
		vSubmits:        after.IouringSendZCSubmits - before.IouringSendZCSubmits,
		vSubmitsDetachd: after.IouringSendZCSubmitsDetached - before.IouringSendZCSubmitsDetached,
	}
}

// wantValidation scales an expected validation-counter delta by the build
// mode: production builds compile against the no-op Counter stubs, so every
// delta there must be 0 while the call sites still run.
func wantValidation(n uint64) uint64 {
	if zcValidationBuild {
		return n
	}
	return 0
}

// TestPrepSendSQEWitnessesTrackTheZCArm pins celeris#591: the exposure
// witnesses must move exactly with the opcode choice, never independently of
// it. A submit counted for a plain SEND would make the fabric A/B
// (celeris#585) and the ZC race tier (celeris#587) read "the branch ran" on
// a run that never armed a zero-copy SQE — the failure mode the counters
// exist to rule out — and a submit missed on the ZC arm reads the opposite.
func TestPrepSendSQEWitnessesTrackTheZCArm(t *testing.T) {
	cases := []struct {
		name          string
		n             int
		detached      bool
		wantOpcode    byte
		wantSubmits   uint64
		wantDetachedN uint64
	}{
		{"below-threshold-no-witness", sendZCMinBytes - 1, false, opSEND, 0, 0},
		{"below-threshold-detached-no-witness", sendZCMinBytes - 1, true, opSEND, 0, 0},
		{"at-threshold-attached", sendZCMinBytes, false, opSENDZC, 1, 0},
		{"at-threshold-detached", sendZCMinBytes, true, opSENDZC, 1, 1},
		{"large-detached", sendZCMinBytes * 4, true, opSENDZC, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := prepSendSQEWitness(t, tc.n, tc.detached)
			if got.opcode != tc.wantOpcode {
				t.Fatalf("opcode = %d, want %d", got.opcode, tc.wantOpcode)
			}
			if got.engineSubmits != tc.wantSubmits {
				t.Errorf("EngineMetrics ZCSendsSubmitted delta = %d, want %d",
					got.engineSubmits, tc.wantSubmits)
			}
			if want := wantValidation(tc.wantSubmits); got.vSubmits != want {
				t.Errorf("validation IouringSendZCSubmits delta = %d, want %d (validation build=%v)",
					got.vSubmits, want, zcValidationBuild)
			}
			if want := wantValidation(tc.wantDetachedN); got.vSubmitsDetachd != want {
				t.Errorf("validation IouringSendZCSubmitsDetached delta = %d, want %d (validation build=%v)",
					got.vSubmitsDetachd, want, zcValidationBuild)
			}
		})
	}
}

// TestPrepSendSQELinkedCountsNoSubmit guards the other half of the gate: a
// linked send never uses SEND_ZC, so it must never be counted as one either.
func TestPrepSendSQELinkedCountsNoSubmit(t *testing.T) {
	var sqe [sqeSize]byte
	zc := &zcStats{}
	w := &Worker{sendZC: true, zc: zc}
	h1 := &conn.H1State{}
	h1.Detached.Store(true)
	cs := &connState{fd: 7, sendBuf: make([]byte, sendZCMinBytes*4), h1State: h1}
	before := validation.Snapshot()
	w.prepSendSQE(unsafe.Pointer(&sqe[0]), cs, true)
	after := validation.Snapshot()
	if sqe[0] != opSEND {
		t.Fatalf("linked large send opcode = %d, want opSEND(%d)", sqe[0], opSEND)
	}
	if n := zc.submits.Load(); n != 0 {
		t.Errorf("ZCSendsSubmitted = %d after a linked send, want 0", n)
	}
	if n := after.IouringSendZCSubmits - before.IouringSendZCSubmits; n != 0 {
		t.Errorf("IouringSendZCSubmits delta = %d after a linked send, want 0", n)
	}
	if n := after.IouringSendZCSubmitsDetached - before.IouringSendZCSubmitsDetached; n != 0 {
		t.Errorf("IouringSendZCSubmitsDetached delta = %d after a linked send, want 0", n)
	}
}

// TestZCStatsNilSafe pins the nil-receiver contract: a hand-built Worker
// literal (most unit tests in this package) leaves w.zc nil, and the witness
// sites sit on the ordinary send path, so a nil dereference there would
// panic the event loop rather than fail a test.
func TestZCStatsNilSafe(t *testing.T) {
	var s *zcStats
	s.noteSubmit()
	s.noteNotif()
	s.noteInlineBytes(4096)
	s.noteRingBytes(4096)
	var sqe [sqeSize]byte
	w := &Worker{sendZC: true}
	cs := &connState{fd: 7, sendBuf: make([]byte, sendZCMinBytes)}
	w.prepSendSQE(unsafe.Pointer(&sqe[0]), cs, false)
	if sqe[0] != opSENDZC {
		t.Fatalf("opcode = %d, want opSENDZC(%d)", sqe[0], opSENDZC)
	}
}
