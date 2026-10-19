//go:build linux

package iouring

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// Descriptor probes shared by the celeris#685 / celeris#715 tests (the
// fd-lifetime tests in every build, the recv-theft trials under
// -tags=validation). They read /proc/self/fd, so they see this process's
// descriptors only.

// fdTarget returns what /proc/self/fd/<fd> names ("socket:[inode]" for a
// socket), or "" when fd is not open.
func fdTarget(fd int) string {
	s, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return ""
	}
	return s
}

// serverFDFor returns this process's descriptor that is the server side of
// the loopback connection the test dialed from local to server: a socket
// whose peer is local and whose own address is server. Or -1.
//
// Both ends are matched (celeris#1081). The peer alone does not name one
// connection in a test binary that has run other tests: a socket an earlier
// test left open (the server side of a connection it accepted and kept, such
// as the descriptor a hand-built worker was given) has as its peer that
// test's client port, and connect(2) hands that port out again once the
// client that had it is gone, possibly to this test's client. /proc/self/fd
// lists its names in
// lexical order, so such a socket at "123" was found before this
// connection's at "45", and a test that then checked that descriptor after
// the engine stopped found a socket the engine never owned still open.
func serverFDFor(local, server string) int {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	for _, ent := range ents {
		fd, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		if namesConn(fd, local, server) {
			return fd
		}
	}
	return -1
}

// namesConn reports whether fd is a socket whose peer is local and whose own
// address is server: the server side of the loopback connection dialed from
// local to server.
func namesConn(fd int, local, server string) bool {
	if !namesPeer(fd, local) {
		return false
	}
	sa, err := unix.Getsockname(fd)
	return err == nil && sockaddrString(fd, sa) == server
}

// namesPeer reports whether fd is a socket whose peer is local: the server
// side of the loopback connection the test dialed from local. It tells a
// descriptor number still naming that connection's socket from the same
// number freed and given to another socket.
func namesPeer(fd int, local string) bool {
	sa, err := unix.Getpeername(fd)
	return err == nil && sockaddrString(fd, sa) == local
}
