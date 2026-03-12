//go:build linux

package epoll

import (
	"golang.org/x/sys/unix"
)

func flushWrites(cs *connState) error {
	if len(cs.writeBuf) == 0 {
		return nil
	}
	if err := writeAll(cs.fd, cs.writeBuf); err != nil {
		cs.writeBuf = cs.writeBuf[:0]
		return err
	}
	cs.writeBuf = cs.writeBuf[:0]
	return nil
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

// pollWrite blocks until fd is writable or timeout (100ms).
func pollWrite(fd int) error {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
	_, err := unix.Poll(fds, 100)
	if err != nil && err != unix.EINTR {
		return err
	}
	return nil
}
