// Package sockopts provides socket option helpers for TCP tuning across platforms.
package sockopts

import (
	"net"
	"time"
)

// Options configures socket-level tuning parameters applied to accepted connections.
type Options struct {
	TCPNoDelay  bool
	TCPQuickAck bool
	SOBusyPoll  time.Duration
	RecvBuf     int
	SendBuf     int
}

// ApplyFD applies socket options to a raw file descriptor. Platform-specific
// options (TCP_QUICKACK, SO_BUSY_POLL) are handled by build-tagged files.
func ApplyFD(fd int, opts Options) error {
	return applyFD(fd, opts)
}

// ApplyConn applies socket options to a net.Conn. Used by StdEngine.
func ApplyConn(conn net.Conn, opts Options) error {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return nil
	}
	if err := tc.SetNoDelay(opts.TCPNoDelay); err != nil {
		return err
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return err
	}
	var sockErr error
	if cerr := raw.Control(func(fd uintptr) {
		sockErr = applyFD(int(fd), Options{
			TCPQuickAck: opts.TCPQuickAck,
			SOBusyPoll:  opts.SOBusyPoll,
			RecvBuf:     opts.RecvBuf,
			SendBuf:     opts.SendBuf,
		})
	}); cerr != nil {
		return cerr
	}
	return sockErr
}
