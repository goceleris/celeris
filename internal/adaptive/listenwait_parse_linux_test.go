//go:build linux

package adaptive

import (
	"encoding/binary"
	"testing"

	"golang.org/x/sys/unix"
)

// diagMsg builds one sock_diag reply message (struct nlmsghdr + struct
// inet_diag_msg) for sport and inode. pad appends NLMSG_ALIGN padding the way
// the kernel does; without it the buffer ends exactly at nlmsg_len.
func diagMsg(seq uint32, typ uint16, sport uint16, inode uint32, pad bool) []byte {
	ne := binary.NativeEndian
	n := nlmsgHdrLen + diagMsgMinLen + 1 // not a multiple of 4
	b := make([]byte, n)
	ne.PutUint32(b[0:], uint32(n))
	ne.PutUint16(b[4:], typ)
	ne.PutUint32(b[8:], seq)
	body := b[nlmsgHdrLen:]
	binary.BigEndian.PutUint16(body[diagMsgSport:], sport)
	ne.PutUint32(body[diagMsgInode:], inode)
	if pad {
		b = append(b, make([]byte, (n+3)&^3-n)...)
	}
	return b
}

func nlDone(seq uint32) []byte {
	b := make([]byte, nlmsgHdrLen)
	binary.NativeEndian.PutUint32(b[0:], nlmsgHdrLen)
	binary.NativeEndian.PutUint16(b[4:], unix.NLMSG_DONE)
	binary.NativeEndian.PutUint32(b[8:], seq)
	return b
}

// parseListenDump must not slice past the buffer when the last message's
// nlmsg_len is not 4-aligned and ends the buffer exactly (a message the kernel
// would have padded), must keep only the wanted port, and must reject a reply
// to some other request (celeris#683 review).
func TestParseListenDump683(t *testing.T) {
	const port = 8080

	t.Run("unpadded final message does not panic", func(t *testing.T) {
		out := map[uint32]struct{}{}
		done, err := parseListenDump(diagMsg(diagSeq, 0x14, port, 7, false), port, out)
		if err != nil || done {
			t.Fatalf("parse = %v, %v; want not done, no error", done, err)
		}
		if _, ok := out[7]; !ok || len(out) != 1 {
			t.Fatalf("inodes = %v, want {7}", out)
		}
	})

	t.Run("padded messages then DONE, other ports ignored", func(t *testing.T) {
		out := map[uint32]struct{}{}
		var msgs []byte
		msgs = append(msgs, diagMsg(diagSeq, 0x14, port, 1, true)...)
		msgs = append(msgs, diagMsg(diagSeq, 0x14, port+1, 2, true)...)
		msgs = append(msgs, diagMsg(diagSeq, 0x14, port, 3, true)...)
		msgs = append(msgs, nlDone(diagSeq)...)
		done, err := parseListenDump(msgs, port, out)
		if err != nil || !done {
			t.Fatalf("parse = %v, %v; want done", done, err)
		}
		_, has1 := out[1]
		_, has2 := out[2]
		_, has3 := out[3]
		if !has1 || !has3 || has2 || len(out) != 2 {
			t.Errorf("inodes = %v, want {1, 3} (inode 2 is another port)", out)
		}
	})

	t.Run("a reply to another request is refused", func(t *testing.T) {
		out := map[uint32]struct{}{}
		if _, err := parseListenDump(diagMsg(diagSeq+1, 0x14, port, 9, true), port, out); err == nil {
			t.Fatalf("a reply with another sequence number was accepted: %v", out)
		}
		if len(out) != 0 {
			t.Fatalf("inodes = %v from a refused reply", out)
		}
	})

	t.Run("a length past the buffer is malformed", func(t *testing.T) {
		msgs := diagMsg(diagSeq, 0x14, port, 5, false)
		binary.NativeEndian.PutUint32(msgs[0:], uint32(len(msgs)+4))
		if _, err := parseListenDump(msgs, port, map[uint32]struct{}{}); err == nil {
			t.Fatal("a message longer than the buffer was accepted")
		}
	})
}
