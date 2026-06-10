//go:build linux

package iouring

import (
	"testing"
	"unsafe"
)

// TestEncodeDecodeUserDataGenRoundTrip verifies the v1.5.0 review-2.6 user_data
// layout: op(bits 56-63) | gen(bits 40-47) | fd(bits 0-39). Every op tag, the
// full generation range (0..255), and fd values up to 65535 (every real fd /
// fixed-file index) must survive an encode→decode round trip independently.
func TestEncodeDecodeUserDataGenRoundTrip(t *testing.T) {
	ops := []uint64{udRecv, udSend, udClose, udHeaderTimer}
	fds := []int{0, 1, 2, 3, 255, 256, 1023, 1024, 4095, 65534, 65535}
	gens := []uint8{0, 1, 2, 127, 128, 200, 254, 255}

	for _, op := range ops {
		for _, fd := range fds {
			for _, gen := range gens {
				ud := encodeUserDataGen(op, fd, gen)
				if gotOp := decodeOp(ud); gotOp != op {
					t.Fatalf("op: encode(op=%#x,fd=%d,gen=%d) → decodeOp=%#x", op, fd, gen, gotOp)
				}
				if gotFD := decodeFD(ud); gotFD != fd {
					t.Fatalf("fd: encode(op=%#x,fd=%d,gen=%d) → decodeFD=%d", op, fd, gen, gotFD)
				}
				if gotGen := decodeGen(ud); gotGen != gen {
					t.Fatalf("gen: encode(op=%#x,fd=%d,gen=%d) → decodeGen=%d", op, fd, gen, gotGen)
				}
			}
		}
	}
}

// TestEncodeUserDataGenZeroEqualsLegacy confirms encodeUserDataGen with gen=0
// collapses to exactly the value the non-conn-bound encodeUserData produces, so
// accept / h2wakeup / udProvide / driver tags are unaffected by the new layout.
func TestEncodeUserDataGenZeroEqualsLegacy(t *testing.T) {
	for _, op := range []uint64{udAccept, udH2Wakeup, udProvide, udRecv, udSend} {
		for _, fd := range []int{0, 1, 42, 65535} {
			if g := encodeUserDataGen(op, fd, 0); g != encodeUserData(op, fd) {
				t.Fatalf("encodeUserDataGen(op=%#x,fd=%d,gen=0)=%#x != encodeUserData=%#x",
					op, fd, g, encodeUserData(op, fd))
			}
		}
	}
}

// TestDecodeFDIgnoresGenBits guards that the 40-bit fd mask isolates the fd even
// when high generation bits are set — the inlined run() dispatch decodes
// fd := int(ud & fdMask) directly, so a non-zero gen must NOT bleed into fd.
func TestDecodeFDIgnoresGenBits(t *testing.T) {
	const fd = 12345
	ud := encodeUserDataGen(udRecv, fd, 0xFF)
	if got := int(ud & fdMask); got != fd {
		t.Fatalf("ud&fdMask = %d, want %d (gen bits bled into fd)", got, fd)
	}
	if got := decodeFD(ud); got != fd {
		t.Fatalf("decodeFD = %d, want %d", got, fd)
	}
}

// newTestBufferRing builds a BufferRing backed by plain Go memory (NOT
// registered with the kernel) so the buffer-recycle accounting on the
// stale-drop path can be exercised without a real io_uring. PushBuffer only
// touches ringAddr/bufRegion/tail, all of which this satisfies; br.tail is the
// observable "buffer was returned" counter.
func newTestBufferRing(count, size int) *BufferRing {
	ringRegion := make([]byte, count*bufRingEntrySize)
	bufRegion := make([]byte, count*size)
	return &BufferRing{
		groupID:    bufRingGroupID,
		count:      count,
		bufferSize: size,
		mask:       uint16(count - 1),
		ringAddr:   unsafe.Pointer(&ringRegion[0]),
		ringRegion: ringRegion,
		bufRegion:  bufRegion,
		tail:       0,
	}
}

// cqeFlagsWithBuffer builds a CQE Flags value carrying provided-buffer id with
// the IORING_CQE_F_BUFFER bit set, matching cqeBufferID/cqeHasBuffer decoding.
func cqeFlagsWithBuffer(bufID uint16) uint32 {
	return (uint32(bufID) << 16) | cqeFBuffer
}

// TestStaleConnCQEDropsAndRecyclesBuffer is the core review-2.6 guard. A recv
// CQE stamped with a generation that does NOT match the live connState at fd
// must be reported stale (so the dispatcher skips the handler) AND, when it
// carried a provided ring buffer, that buffer must be recycled — otherwise the
// ring leaks an entry → ENOBUFS → CQE storm (celeris#322).
func TestStaleConnCQEDropsAndRecyclesBuffer(t *testing.T) {
	const fd = 7
	const liveGen uint8 = 42

	w := &Worker{
		conns:   make([]*connState, 64),
		bufRing: newTestBufferRing(8, 64),
	}
	w.conns[fd] = &connState{fd: fd, generation: liveGen}

	tailBefore := w.bufRing.tail

	// Stale: gen-1 (a CQE from the prior occupant) with a provided buffer.
	stale := &completionEntry{
		UserData: encodeUserDataGen(udRecv, fd, liveGen-1),
		Res:      10,
		Flags:    cqeFlagsWithBuffer(3),
	}
	if !w.staleConnCQE(stale, fd, stale.UserData) {
		t.Fatalf("gen mismatch (live=%d, cqe=%d) not reported stale", liveGen, liveGen-1)
	}
	if !w.hasBufReturns {
		t.Fatalf("stale buffer-bearing recv did not set hasBufReturns")
	}
	if w.bufRing.tail != tailBefore+1 {
		t.Fatalf("buffer not recycled: tail %d → %d (want +1)", tailBefore, w.bufRing.tail)
	}

	// Also stale in the other direction (gen+1, a CQE for a future occupant).
	staleFwd := &completionEntry{
		UserData: encodeUserDataGen(udRecv, fd, liveGen+1),
		Res:      10,
		Flags:    cqeFlagsWithBuffer(4),
	}
	tailMid := w.bufRing.tail
	if !w.staleConnCQE(staleFwd, fd, staleFwd.UserData) {
		t.Fatalf("gen mismatch (live=%d, cqe=%d) not reported stale", liveGen, liveGen+1)
	}
	if w.bufRing.tail != tailMid+1 {
		t.Fatalf("forward-stale buffer not recycled: tail %d → %d (want +1)", tailMid, w.bufRing.tail)
	}

	// Matching gen: NOT stale, buffer left for handleRecv to consume/recycle.
	tailMatch := w.bufRing.tail
	match := &completionEntry{
		UserData: encodeUserDataGen(udRecv, fd, liveGen),
		Res:      10,
		Flags:    cqeFlagsWithBuffer(5),
	}
	if w.staleConnCQE(match, fd, match.UserData) {
		t.Fatalf("matching gen (%d) wrongly reported stale", liveGen)
	}
	if w.bufRing.tail != tailMatch {
		t.Fatalf("matching-gen path recycled a buffer it should have left alone: tail %d → %d",
			tailMatch, w.bufRing.tail)
	}
}

// TestStaleConnCQENilSlot verifies a CQE for an fd whose slot is now empty
// (conn closed, fd not yet reused) is stale, and any buffer is still recycled.
func TestStaleConnCQENilSlot(t *testing.T) {
	const fd = 9
	w := &Worker{
		conns:   make([]*connState, 64),
		bufRing: newTestBufferRing(8, 64),
	}
	// w.conns[fd] is nil.
	tailBefore := w.bufRing.tail
	c := &completionEntry{
		UserData: encodeUserDataGen(udRecv, fd, 3),
		Res:      5,
		Flags:    cqeFlagsWithBuffer(2),
	}
	if !w.staleConnCQE(c, fd, c.UserData) {
		t.Fatalf("nil-slot CQE not reported stale")
	}
	if w.bufRing.tail != tailBefore+1 {
		t.Fatalf("nil-slot buffer not recycled: tail %d → %d", tailBefore, w.bufRing.tail)
	}
}

// TestStaleConnCQENoBufferNoRecycle verifies the non-bufRing / no-buffer path:
// a stale CQE without a provided buffer is still dropped, and nothing is
// pushed to the ring (no spurious recycle / no nil deref when bufRing is nil).
func TestStaleConnCQENoBufferNoRecycle(t *testing.T) {
	const fd = 4
	w := &Worker{conns: make([]*connState, 64)} // bufRing == nil
	w.conns[fd] = &connState{fd: fd, generation: 5}

	// Stale, no buffer flag.
	c := &completionEntry{
		UserData: encodeUserDataGen(udClose, fd, 4),
		Res:      0,
		Flags:    0,
	}
	if !w.staleConnCQE(c, fd, c.UserData) {
		t.Fatalf("stale close CQE not reported stale")
	}
	if w.hasBufReturns {
		t.Fatalf("no-buffer stale drop wrongly set hasBufReturns")
	}
}

// TestGenerationDiffersAcrossReuse exercises the real lifecycle: acquire a
// connState at an fd, release it, re-acquire at the same fd, and confirm the
// generation advanced. A CQE stamped with the OLD generation must then be
// dropped by staleConnCQE while one with the NEW generation is handled.
func TestGenerationDiffersAcrossReuse(t *testing.T) {
	const fd = 11

	cs1 := acquireConnState(nil, fd, 0, false)
	oldGen := cs1.generation
	releaseConnState(cs1) // returns to pool; generation NOT reset

	cs2 := acquireConnState(nil, fd, 0, false)
	newGen := cs2.generation
	defer releaseConnState(cs2)

	if newGen == oldGen {
		t.Fatalf("generation did not advance across reuse: old=%d new=%d", oldGen, newGen)
	}

	w := &Worker{
		conns:   make([]*connState, 64),
		bufRing: newTestBufferRing(8, 64),
	}
	w.conns[fd] = cs2 // the new occupant

	// CQE from the OLD occupant (oldGen) → stale → dropped + buffer recycled.
	tailBefore := w.bufRing.tail
	old := &completionEntry{
		UserData: encodeUserDataGen(udRecv, fd, oldGen),
		Res:      8,
		Flags:    cqeFlagsWithBuffer(1),
	}
	if !w.staleConnCQE(old, fd, old.UserData) {
		t.Fatalf("old-generation CQE (gen=%d) not dropped against live gen=%d", oldGen, newGen)
	}
	if w.bufRing.tail != tailBefore+1 {
		t.Fatalf("old-generation buffer not recycled: tail %d → %d", tailBefore, w.bufRing.tail)
	}

	// CQE for the CURRENT occupant (newGen) → not stale.
	cur := &completionEntry{
		UserData: encodeUserDataGen(udRecv, fd, newGen),
		Res:      8,
		Flags:    cqeFlagsWithBuffer(2),
	}
	if w.staleConnCQE(cur, fd, cur.UserData) {
		t.Fatalf("current-generation CQE (gen=%d) wrongly dropped", newGen)
	}
}
