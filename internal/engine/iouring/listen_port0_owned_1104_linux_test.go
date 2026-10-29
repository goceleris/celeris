//go:build linux

package iouring

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// celeris#1104: a Listen that fails gives back everything it created. The
// census of the port 0 hold tests (census895) counts every socket of the
// process whose local port is the one the hold chose, whoever owns it: after
// Listen has returned the hold is gone and the port is free, and a socket that
// something else in the test binary opens on it is counted as the engine's.
// The failure the issue records (one socket "still bound", the owner not
// known) cannot say which. What the engine promises is narrower and exact:
// every socket it opened for that Listen (the hold, the workers' listen
// sockets) is closed when Listen returns. So the test names those sockets
// while Listen runs (by inode: the descriptor number of a closed socket is
// reused) and asks, after it returns, whether any is still open in the
// process. A socket that is not the engine's is reported and does not fail
// the test.

// What a recorded socket is: the hold of the port, or a worker's listen socket.
const (
	ownedHold1104   = "the port 0 hold"
	ownedListen1104 = "a worker's listen socket"
)

// ownedSockets1104 is the sockets one or more Listen calls created, by inode.
type ownedSockets1104 struct {
	mu     sync.Mutex
	inodes map[uint64]string // inode -> kind
}

func newOwnedSockets1104() *ownedSockets1104 {
	return &ownedSockets1104{inodes: map[uint64]string{}}
}

// socketInode1104 is the inode of the socket that descriptor fd names, false if
// it is not a socket. It reads the link, not the socket: a descriptor closed
// meanwhile is just false.
func socketInode1104(fd int) (uint64, bool) {
	link, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil || !strings.HasPrefix(link, "socket:[") || !strings.HasSuffix(link, "]") {
		return 0, false
	}
	ino, err := strconv.ParseUint(link[len("socket:["):len(link)-1], 10, 64)
	return ino, err == nil
}

// noteFD records the socket descriptor fd names right now. Safe to call from a
// worker's thread (listenAddrOf is called with the listen socket's descriptor).
func (o *ownedSockets1104) noteFD(fd int, what string) {
	ino, ok := socketInode1104(fd)
	if !ok {
		return
	}
	o.mu.Lock()
	o.inodes[ino] = what
	o.mu.Unlock()
}

// notePort records the hold: the socket of the process that is bound to addr
// (its host and port) right now, was never connected, does not listen and has
// SO_REUSEPORT, which is how ResolvePort0 makes it. Called from portHoldHook,
// while nothing of the engine's but the hold is on the port. A socket the
// process has on that port number at another address (the kernel gives a
// bind(0) a port that a socket bound to 127.0.0.2 or ::1 already has) is not
// the hold and is not recorded. It uses no *testing.T: it runs on the
// goroutine of Listen.
func (o *ownedSockets1104) notePort(addr string) {
	host, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		return
	}
	ip := net.ParseIP(host)
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return
	}
	for _, ent := range ents {
		fd, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		if _, ok := socketInode1104(fd); !ok {
			continue
		}
		if typ, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE); err != nil || typ != unix.SOCK_STREAM {
			continue
		}
		sa, err := unix.Getsockname(fd)
		if err != nil {
			continue
		}
		var p int
		var local net.IP
		switch a := sa.(type) {
		case *unix.SockaddrInet4:
			p, local = a.Port, net.IP(a.Addr[:])
		case *unix.SockaddrInet6:
			p, local = a.Port, net.IP(a.Addr[:])
		}
		if p != port || (ip != nil && !ip.Equal(local)) {
			continue
		}
		if v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN); err != nil || v != 0 {
			continue
		}
		if _, err := unix.Getpeername(fd); err == nil {
			continue
		}
		if v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT); err != nil || v != 1 {
			continue
		}
		o.noteFD(fd, ownedHold1104)
	}
}

// count is how many recorded sockets are of kind.
func (o *ownedSockets1104) count(kind string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, k := range o.inodes {
		if k == kind {
			n++
		}
	}
	return n
}

// stillOpen is the recorded sockets that some descriptor of the process still
// names, described.
func (o *ownedSockets1104) stillOpen() []string {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return []string{"cannot read /proc/self/fd: " + err.Error()}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	var left []string
	for _, ent := range ents {
		fd, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		if ino, ok := socketInode1104(fd); ok {
			if what, mine := o.inodes[ino]; mine {
				left = append(left, fmt.Sprintf("fd %d: %s (inode %d)", fd, what, ino))
			}
		}
	}
	return left
}

// failedListen1104 is TestPort0HoldIsReleasedWhenListenFails895's Listen that
// fails after the port was chosen: the address every worker would report is
// nil, so Listen returns the no-address error. It returns the address the hold
// chose and the sockets the engine opened for it. foreign, when not nil, runs
// in the hold hook with the port, before the hold is recorded: a socket of
// another owner already on the port number when the test looks for the hold.
func failedListen1104(t *testing.T, foreign func(port int)) (string, *ownedSockets1104) {
	t.Helper()
	owned := newOwnedSockets1104()
	orig := listenAddrOf
	listenAddrOf = func(fd int) net.Addr {
		owned.noteFD(fd, ownedListen1104)
		return nil
	}
	t.Cleanup(func() { listenAddrOf = orig })
	var chosen string
	var chosenMu sync.Mutex
	portHoldHook = func(addr string) {
		chosenMu.Lock()
		chosen = addr
		chosenMu.Unlock()
		if _, ps, err := net.SplitHostPort(addr); err == nil && foreign != nil {
			if port, err := strconv.Atoi(ps); err == nil {
				foreign(port)
			}
		}
		owned.notePort(addr)
	}
	t.Cleanup(func() { portHoldHook = nil })
	listenFailsWithNoAddress1104(t)
	chosenMu.Lock()
	defer chosenMu.Unlock()
	return chosen, owned
}
