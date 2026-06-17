//go:build linux

package iouring

import (
	"testing"
	"unsafe"
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
