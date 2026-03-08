//go:build linux

package sockopts

import (
	"golang.org/x/sys/unix"
)

func applyFD(fd int, opts Options) error {
	if opts.TCPNoDelay {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1); err != nil {
			return err
		}
	}
	if opts.TCPQuickAck {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_QUICKACK, 1); err != nil {
			return err
		}
	}
	if opts.SOBusyPoll > 0 {
		micros := int(opts.SOBusyPoll.Microseconds())
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_BUSY_POLL, micros); err != nil {
			return err
		}
	}
	if opts.RecvBuf > 0 {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, opts.RecvBuf); err != nil {
			return err
		}
	}
	if opts.SendBuf > 0 {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, opts.SendBuf); err != nil {
			return err
		}
	}
	return nil
}
