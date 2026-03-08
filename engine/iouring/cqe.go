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
	udAccept  uint64 = 0x01 << 56
	udRecv    uint64 = 0x02 << 56
	udSend    uint64 = 0x03 << 56
	udClose   uint64 = 0x04 << 56
	udPeek    uint64 = 0x05 << 56
	udProvide uint64 = 0x06 << 56
	udMask    uint64 = 0xFF << 56
	fdMask    uint64 = (1 << 56) - 1
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
