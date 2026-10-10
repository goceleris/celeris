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

// serverFDFor returns this process's descriptor whose peer is local (the
// server side of a loopback connection the test dialed), or -1.
func serverFDFor(local string) int {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	for _, ent := range ents {
		fd, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		if namesPeer(fd, local) {
			return fd
		}
	}
	return -1
}

// namesPeer reports whether fd is a socket whose peer is local: the server
// side of the loopback connection the test dialed from local. It tells a
// descriptor number still naming that connection's socket from the same
// number freed and given to another socket.
func namesPeer(fd int, local string) bool {
	sa, err := unix.Getpeername(fd)
	return err == nil && sockaddrString(fd, sa) == local
}
