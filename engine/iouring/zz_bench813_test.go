//go:build linux

package iouring

// BENCH (lane ZC-backstop, celeris#812 / PR #813), evidence only. This file lives on the measure/812-zc-base and
// measure/812-zc-fix branches (celeris main fe9264f and the PR head c417fe3, each plus this file), never in a PR.
//
// What #813 adds to the paths that run when nothing is held, which is all a server ever runs unless a peer stalls
// mid-SEND_ZC past the closing drain and the release backstop:
//   - prepSendSQE, on every send: `w.sendZC && w.zcHoldBytes < zcHoldBytesMax` where main reads `w.sendZC`;
//   - a closed connection's accounting: noteClosedInflight counts a SEND_ZC apart (zcSendOwed), and a stale terminal
//     CQE (noteStaleTerminalOp, through staleConnCQE) takes it off and pays a length check of Worker.zcHolds.
// Every benchmark below uses only names both trees have, so the two arms run the same file.
//   SendArmLinked512     the per-request hot path: an H1 response's SEND linked to the next RECV (never a SEND_ZC).
//   SendArmUnlinked2K    an unlinked send below sendZCMinBytes (plain SEND).
//   SendArmUnlinked64K   an unlinked send at or above sendZCMinBytes: armed as a SEND_ZC on both trees (nothing held).
//   CloseRetireRecv      a closed connection whose recv is owed (every server-side close with a recv armed): the
//                        identity registered by noteClosedInflight and retired by the recv's cancelled CQE.
//   CloseRetireZCNotif   a closed connection whose SEND_ZC notification is owed: registered, then retired by the
//                        notification (the case #812 is about, when the notification comes before the backstop).
// TestBench813 runs each through testing.Benchmark (the default 1 s benchtime) and logs one line each, for
// celeris-stress timing mode, which runs tests, not -bench:
//
//	BENCH813 shape=<name> n=<N> ns_per_op=<x> allocs_per_op=<a> bytes_per_op=<b>

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// bench813SQE is scratch space for one SQE (64 bytes), 8-byte aligned.
type bench813SQE [16]uint64

func bench813SendArm(b *testing.B, size int, linked bool) {
	w := &Worker{sendZC: true, zc: &zcStats{}}
	cs := &connState{fd: 7, sendBuf: make([]byte, size)}
	var sqe bench813SQE
	p := unsafe.Pointer(&sqe[0])
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		w.prepSendSQE(p, cs, linked)
	}
	b.StopTimer()
	if want := !linked && size >= sendZCMinBytes; cs.sendIsZC != want {
		b.Fatalf("size %d linked %v armed sendIsZC=%v, want %v", size, linked, cs.sendIsZC, want)
	}
}

func bench813CloseRetire(b *testing.B, zcNotif bool) {
	w := &Worker{conns: make([]*connState, 16), handoffLoss: &handoffLossStats{}}
	const fd, gen = 2, 1 << 31
	cs := &connState{fd: fd, generation: gen}
	c := &completionEntry{UserData: encodeUserDataGen(udRecv, fd, gen), Res: -int32(unix.ECANCELED)}
	if zcNotif {
		// The SEND_ZC's first CQE came before the close; its notification is owed.
		cs.sendIsZC, cs.zcNotifPending = true, true
		c = &completionEntry{UserData: encodeUserDataGen(udSend, fd, gen), Flags: cqeFNotif}
	} else {
		cs.recvArmed = true
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		cs.kernelInflight = 1
		w.noteClosedInflight(cs)
		w.staleConnCQE(c, fd, c.UserData)
	}
	b.StopTimer()
	if _, ok := w.closedOps[encodeConnOpKey(fd, gen)]; ok {
		b.Fatal("the identity was not retired")
	}
}

func TestBench813(t *testing.T) {
	for _, bm := range []struct {
		name string
		fn   func(*testing.B)
	}{
		{"SendArmLinked512", func(b *testing.B) { bench813SendArm(b, 512, true) }},
		{"SendArmUnlinked2K", func(b *testing.B) { bench813SendArm(b, 2048, false) }},
		{"SendArmUnlinked64K", func(b *testing.B) { bench813SendArm(b, 64<<10, false) }},
		{"CloseRetireRecv", func(b *testing.B) { bench813CloseRetire(b, false) }},
		{"CloseRetireZCNotif", func(b *testing.B) { bench813CloseRetire(b, true) }},
	} {
		r := testing.Benchmark(bm.fn)
		if r.N == 0 {
			t.Errorf("BENCH813 shape=%s: the benchmark failed", bm.name)
			continue
		}
		t.Logf("BENCH813 shape=%s n=%d ns_per_op=%.2f allocs_per_op=%d bytes_per_op=%d",
			bm.name, r.N, float64(r.T.Nanoseconds())/float64(r.N), r.AllocsPerOp(), r.AllocedBytesPerOp())
	}
}
