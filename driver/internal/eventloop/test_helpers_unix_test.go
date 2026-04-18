//go:build !windows

package eventloop

import "golang.org/x/sys/unix"

// dupFD duplicates fd and sets the dup to non-blocking. Callers registering
// the dup with a [Loop] worker rely on non-blocking semantics: the worker
// uses edge-triggered epoll and loops reads until EAGAIN, so a blocking fd
// would wedge the worker goroutine on the second read. (*os.File) dups
// returned by net.TCPConn.File() are blocking by default — tests must flip
// them back before handing the fd to the event loop.
func dupFD(fd int) (int, error) {
	nfd, err := unix.Dup(fd)
	if err != nil {
		return -1, err
	}
	if err := unix.SetNonblock(nfd, true); err != nil {
		_ = unix.Close(nfd)
		return -1, err
	}
	return nfd, nil
}
func closeFD(fd int) error { return unix.Close(fd) }
