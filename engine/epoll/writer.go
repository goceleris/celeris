//go:build linux

package epoll

import (
	"golang.org/x/sys/unix"
)

func flushWrites(cs *connState) error {
	if len(cs.pending) == 0 {
		return nil
	}

	if len(cs.pending) == 1 {
		if err := writeAll(cs.fd, cs.pending[0]); err != nil {
			cs.pending = cs.pending[:0]
			return err
		}
		cs.pending = cs.pending[:0]
		return nil
	}

	iovecs := make([][]byte, len(cs.pending))
	copy(iovecs, cs.pending)
	cs.pending = cs.pending[:0]
	return writevAll(cs.fd, iovecs)
}

// writeAll writes the entire buffer, polling for writability on EAGAIN.
func writeAll(fd int, data []byte) error {
	for len(data) > 0 {
		n, err := unix.Write(fd, data)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
				if pollErr := pollWrite(fd); pollErr != nil {
					return pollErr
				}
				continue
			}
			return err
		}
		data = data[n:]
	}
	return nil
}

// writevAll writes all iovecs, polling for writability on EAGAIN.
func writevAll(fd int, iovecs [][]byte) error {
	for len(iovecs) > 0 {
		n, err := unix.Writev(fd, iovecs)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
				if pollErr := pollWrite(fd); pollErr != nil {
					return pollErr
				}
				continue
			}
			return err
		}

		remaining := n
		for remaining > 0 && len(iovecs) > 0 {
			if remaining >= len(iovecs[0]) {
				remaining -= len(iovecs[0])
				iovecs = iovecs[1:]
			} else {
				iovecs[0] = iovecs[0][remaining:]
				remaining = 0
			}
		}
	}
	return nil
}

// pollWrite blocks until fd is writable or timeout (100ms).
func pollWrite(fd int) error {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
	_, err := unix.Poll(fds, 100)
	if err != nil && err != unix.EINTR {
		return err
	}
	return nil
}
