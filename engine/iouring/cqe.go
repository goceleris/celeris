//go:build linux

package iouring

// completionEntry mirrors io_uring_cqe.
type completionEntry struct {
	UserData uint64
	Res      int32
	Flags    uint32
}

// UserData encoding: upper 8 bits = op tag, lower 56 bits = fd.
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
	fdMask        uint64 = (1 << 56) - 1
)

func encodeUserData(op uint64, fd int) uint64 {
	return op | (uint64(fd) & fdMask)
}

func decodeOp(userData uint64) uint64 {
	return userData & udMask
}

func decodeFD(userData uint64) int {
	return int(userData & fdMask)
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
