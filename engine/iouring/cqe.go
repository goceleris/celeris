//go:build linux

package iouring

// completionEntry mirrors io_uring_cqe.
type completionEntry struct {
	UserData uint64
	Res      int32
	Flags    uint32
}

// UserData encoding (v1.5.0, review 2.6): a per-conn generation tag is
// folded between the op and fd so a late/in-flight CQE from a CLOSED
// connection cannot be misattributed to a NEW connection that has since
// reused the same fd (or fixed-file slot).
//
//	bits 56-63 (8)  : op tag      (udMask)
//	bits 40-47 (8)  : generation  (genMask) — connState.generation, conn-bound ops only
//	bits  0-39 (40) : fd / fixed-file index (fdMask)
//
// 40 bits comfortably covers every real fd and fixed-file index (both
// < 65536 in practice). gen=0 is used for ops that are NOT bound to a
// connState (accept on listenFD, the H2 wakeup eventfd, the udProvide
// cancel sentinel, and the driver ops which have their own lifecycle).
const (
	udAccept uint64 = 0x01 << 56
	udRecv   uint64 = 0x02 << 56
	udSend   uint64 = 0x03 << 56
	udClose  uint64 = 0x04 << 56
	// udProvide is a sentinel "ignore this CQE" tag. The recv-pause
	// cancellation path in worker.go tags the cancel SQE with this value
	// so the main dispatcher's switch has no matching case and drops
	// the CQE on the floor instead of routing through handleRecv. Name
	// kept from the legacy PROVIDE_BUFFERS path (BufferGroup, removed
	// in v1.5.0/celeris#320); the value (0x06 << 56) is unchanged to
	// preserve the user-data tag layout for any persisted snapshots.
	udProvide  uint64 = 0x06 << 56
	udH2Wakeup uint64 = 0x07 << 56
	// udHeaderTimer tags IORING_OP_TIMEOUT SQEs submitted by initProtocol /
	// ProcessH1's arm-callback to enforce ReadHeaderTimeout per-conn. The
	// timer fires absolutely at the deadline; CQE handler closes the conn
	// iff HeaderDeadlineNs is still > 0 and now > deadline (race-free
	// against ClearHeaderDeadline). New in v1.4.11.
	udHeaderTimer uint64 = 0x08 << 56
	// Driver tags for EventLoopProvider — kept non-overlapping with HTTP tags
	// so the main CQE dispatch can route them to the driver path without
	// consulting the driverConns map.
	udDriverRecv  uint64 = 0x10 << 56
	udDriverSend  uint64 = 0x11 << 56
	udDriverClose uint64 = 0x12 << 56
	udMask        uint64 = 0xFF << 56
	genShift             = 40
	genMask       uint64 = 0xFF << genShift
	fdMask        uint64 = (1 << 40) - 1
)

// encodeUserData encodes a NON-conn-bound op (gen=0). Used for accept
// (listenFD, no conn yet), udH2Wakeup (global eventfd), udProvide (the
// cancel sentinel) and the driver ops (separate lifecycle).
func encodeUserData(op uint64, fd int) uint64 {
	return op | (uint64(fd) & fdMask)
}

// encodeUserDataGen encodes a CONN-BOUND op, stamping the owning
// connState's generation so a stale CQE from a prior fd occupant is
// detectable at dispatch (see decodeGen). gen=0 collapses to the same
// value encodeUserData would produce.
func encodeUserDataGen(op uint64, fd int, gen uint8) uint64 {
	return op | (uint64(gen) << genShift) | (uint64(fd) & fdMask)
}

func decodeOp(userData uint64) uint64 {
	return userData & udMask
}

func decodeGen(userData uint64) uint8 {
	return uint8((userData & genMask) >> genShift)
}

func decodeFD(userData uint64) int {
	return int(userData & fdMask)
}

// connOpKey strips the op tag from a conn-bound user_data, leaving the
// (generation, fd) identity that every in-flight op of one conn shares.
// Used as the Worker.closedOps map key so a terminal recv/send CQE
// arriving AFTER its conn closed can still be attributed to the closed
// connState for kernelInflight accounting (release gating).
func connOpKey(userData uint64) uint64 {
	return userData &^ udMask
}

// encodeConnOpKey builds the same (generation, fd) identity from a closed
// conn's fields — the value connOpKey extracts from its ops' user_data.
func encodeConnOpKey(fd int, gen uint8) uint64 {
	return (uint64(gen) << genShift) | (uint64(fd) & fdMask)
}

func cqeBufferID(flags uint32) uint16 {
	return uint16(flags >> 16)
}

func cqeHasMore(flags uint32) bool {
	return flags&cqeFMore != 0
}

func cqeHasBuffer(flags uint32) bool {
	return flags&cqeFBuffer != 0
}

func cqeIsNotif(flags uint32) bool {
	return flags&cqeFNotif != 0
}
