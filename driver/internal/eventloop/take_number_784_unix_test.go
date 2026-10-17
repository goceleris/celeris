//go:build unix

package eventloop

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// c784TakeNumber closes nothing: the caller has closed fd. It opens a new
// socketpair and puts one end on fd's number, the way the next accept or dial
// of the process would reuse it. It returns the socket now on the number
// (== fd) and its peer. Either end of the new pair may already be fd, the
// lowest free number; otherwise the first end is moved onto fd with dup2, and
// the other end is never fd, so dup2 never closes the peer. It builds on
// every unix platform (the darwin test of the fallback loop uses it too), so
// it uses plain SOCK_STREAM and CloseOnExec: SOCK_CLOEXEC and dup3 are Linux
// only.
func c784TakeNumber(t *testing.T, fd int, nonblock bool) (int, int) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	return c784PutOnNumber(t, [2]int{pair[0], pair[1]}, fd, nonblock)
}

// c784PutOnNumber is c784TakeNumber for a socketpair the caller has made, so a
// test can choose which numbers the pair holds relative to fd. fd is either
// one of the pair's numbers or a free number.
func c784PutOnNumber(t *testing.T, pair [2]int, fd int, nonblock bool) (int, int) {
	t.Helper()
	var peer int
	switch fd {
	case pair[0]:
		peer = pair[1]
	case pair[1]:
		peer = pair[0]
	default:
		if err := unix.Dup2(pair[0], fd); err != nil {
			t.Fatalf("dup2 onto %d: %v", fd, err)
		}
		_ = unix.Close(pair[0])
		peer = pair[1]
	}
	unix.CloseOnExec(fd)
	unix.CloseOnExec(peer)
	if nonblock {
		if err := unix.SetNonblock(fd, true); err != nil {
			t.Fatalf("nonblock: %v", err)
		}
		if err := unix.SetNonblock(peer, true); err != nil {
			t.Fatalf("nonblock: %v", err)
		}
	}
	t.Cleanup(func() {
		_ = unix.Close(fd)
		_ = unix.Close(peer)
	})
	return fd, peer
}

// TestC784PutOnNumberKeepsAConnectedPairWhicheverEndHoldsTheNumber890: the
// helper's three cases, each forced: the number is the pair's first end, its
// second end, or neither (then the first end is moved onto it). In each the
// returned fd must be the number, the peer must be another open descriptor,
// and the two must be the ends of one connection. The case "second end" is
// the one the darwin test handled wrongly (it moved the first end onto the
// number, which closed the peer), and the Linux helper handled right
// (celeris#890 item 38).
func TestC784PutOnNumberKeepsAConnectedPairWhicheverEndHoldsTheNumber890(t *testing.T) {
	for _, tc := range []struct {
		name string
		num  func(t *testing.T, pair [2]int) int
	}{
		{"FirstEnd", func(_ *testing.T, p [2]int) int { return p[0] }},
		{"SecondEnd", func(_ *testing.T, p [2]int) int { return p[1] }},
		{"Neither", func(t *testing.T, p [2]int) int {
			spare, err := unix.Dup(p[0])
			if err != nil {
				t.Fatalf("dup: %v", err)
			}
			if err := unix.Close(spare); err != nil { // a free number, as after the owner closed it
				t.Fatalf("close spare: %v", err)
			}
			return spare
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
			if err != nil {
				t.Fatalf("socketpair: %v", err)
			}
			pair := [2]int{p[0], p[1]}
			want := tc.num(t, pair)
			fd, peer := c784PutOnNumber(t, pair, want, true)
			if fd != want {
				t.Fatalf("fd = %d, want the number %d", fd, want)
			}
			if peer == fd {
				t.Fatalf("the peer is the number %d itself: the pair's other end was lost", fd)
			}
			if _, err := unix.FcntlInt(uintptr(peer), unix.F_GETFL, 0); err != nil {
				t.Fatalf("peer %d is not an open descriptor: %v", peer, err)
			}
			// One connection: what one end writes, the other reads.
			for _, dir := range []struct {
				name     string
				from, to int
			}{{"peer->fd", peer, fd}, {"fd->peer", fd, peer}} {
				if _, err := unix.Write(dir.from, []byte("ping")); err != nil {
					t.Fatalf("%s: write: %v", dir.name, err)
				}
				var buf [8]byte
				var n int
				for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
					n, err = unix.Read(dir.to, buf[:])
					if n > 0 || (err != nil && err != unix.EAGAIN) {
						break
					}
				}
				if string(buf[:max(n, 0)]) != "ping" {
					t.Fatalf("%s: read %q (err %v), want \"ping\": the two descriptors are not the ends of one connection", dir.name, buf[:max(n, 0)], err)
				}
			}
		})
	}
}
