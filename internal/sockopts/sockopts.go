// Package sockopts provides socket option helpers for TCP tuning across platforms.
package sockopts

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"time"
)

// Options configures socket-level tuning parameters applied to accepted connections.
type Options struct {
	TCPNoDelay  bool
	TCPQuickAck bool
	// SOBusyPoll turns SO_BUSY_POLL on (Linux). Zero, the default, sets
	// nothing: busy polling is opt-in (celeris#1036). A positive value of at
	// least one microsecond sets SO_BUSY_POLL to it, truncated to whole
	// microseconds, and asks for SO_PREFER_BUSY_POLL, which the kernel gives
	// only to a process with CAP_NET_ADMIN.
	SOBusyPoll time.Duration
	RecvBuf    int
	SendBuf    int
}

// BusyPollError is what ApplyFD returns when the kernel refuses SO_BUSY_POLL
// (a kernel built without CONFIG_NET_RX_BUSY_POLL answers ENOPROTOOPT). The
// options that come before it have been applied.
type BusyPollError struct {
	Micros int   // the budget asked for, in microseconds
	Err    error // the kernel's answer
}

func (e *BusyPollError) Error() string {
	return fmt.Sprintf("setsockopt SO_BUSY_POLL %d us: %v", e.Micros, e.Err)
}

func (e *BusyPollError) Unwrap() error { return e.Err }

// busyPollRefusedWarned makes the warning of ApplyAccepted once per process:
// every accepted connection would be refused the same way.
var busyPollRefusedWarned atomic.Bool

// ApplyAccepted is ApplyFD for the engines' accept, adopt and transplant
// paths. It ignores the error, as they always did (a connection is served
// without the option), except that it logs a refused SO_BUSY_POLL once, at
// Warn: the caller asked for it with Config.SocketBusyPoll, so it should not
// fail in silence. A nil logger logs nothing.
func ApplyAccepted(fd int, opts Options, logger *slog.Logger) {
	err := ApplyFD(fd, opts)
	var bp *BusyPollError
	if err == nil || !errors.As(err, &bp) || logger == nil {
		return
	}
	if busyPollRefusedWarned.CompareAndSwap(false, true) {
		logger.Warn("the kernel refused SO_BUSY_POLL for Config.SocketBusyPoll; accepted connections are served without busy polling",
			"budget_us", bp.Micros, "error", err.Error(),
			"hint", "the kernel needs CONFIG_NET_RX_BUSY_POLL; logged once")
	}
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
