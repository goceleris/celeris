//go:build linux

package deferlinger

import (
	"log/slog"

	"golang.org/x/sys/unix"
)

// Clear turns TCP_DEFER_ACCEPT off on listener fd. From the moment it returns,
// a connection whose handshake completes on fd enters the accept queue at once
// rather than being held until its first data arrives.
func Clear(fd int) error {
	return unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_DEFER_ACCEPT, 0)
}

// Restore turns TCP_DEFER_ACCEPT back on on listener fd, with the value
// createListenSocket sets.
func Restore(fd int) error {
	return unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_DEFER_ACCEPT, 1)
}

// GuardSyncnt sets TCP_SYNCNT=1 on listener fd, which gives its request
// sockets one SYN-ACK retransmission when net.ipv4.tcp_synack_retries is 0.
func GuardSyncnt(fd int) error {
	return unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_SYNCNT, 1)
}

// Enter moves listener fd from ACTIVE to LINGERING, on the thread that owns
// it. It returns the deadline on the Now clock, or the zero Deadline when the
// listener must close now: it was created without TCP_DEFER_ACCEPT
// (deferCapable false), so nothing on it can be deferred, or Linger is 0 or
// less.
//
// A failed clear or guard is counted, and logged the first time it happens
// on this engine (p), and the listener still lingers: the connections
// deferred before the pause are then promoted by the kernel's timer as they
// would have been, and only arrivals in the last second before the close are
// exposed.
func Enter(fd int, deferCapable bool, p *PauseState, logger *slog.Logger, owner string, id int) Deadline {
	lg := Linger()
	if !deferCapable || lg <= 0 {
		return 0
	}
	if ClearOnPause() {
		if err := Clear(fd); err != nil {
			setFailures.Add(1)
			p.warnOnce(warnSet, logger, "accept pause: could not clear TCP_DEFER_ACCEPT on the "+
				"pausing listener; connections that arrive in its last second may be reset", owner, id, err)
		}
	}
	if p.Guard() {
		if err := GuardSyncnt(fd); err != nil {
			setFailures.Add(1)
			p.warnOnce(warnSet, logger, "accept pause: could not set TCP_SYNCNT=1 on the "+
				"pausing listener while net.ipv4.tcp_synack_retries is 0; connections deferred "+
				"before the pause may be reset", owner, id, err)
		} else {
			guards.Add(1)
		}
	}
	lingers.Add(1)
	// The deadline is taken here, after the setsockopt calls returned. The
	// youngest connection that can still be deferred is the last one whose
	// handshake completed before the clear, and the kernel promotes it about
	// one second after its SYN; so the linger has to be measured from the
	// clear, not from the pause call, which a loop may observe late. It is
	// on the monotonic Now clock, so a wall-clock step cannot move it.
	return After(lg)
}

// Leave moves listener fd from LINGERING back to ACTIVE when a resume
// arrives before the deadline: the option goes back on the same descriptor.
// A failed restore costs throughput only; it is counted, and logged the first
// time it happens on this engine (p).
func Leave(fd int, deferCapable bool, p *PauseState, logger *slog.Logger, owner string, id int) {
	aborts.Add(1)
	if !deferCapable {
		return
	}
	if err := Restore(fd); err != nil {
		setFailures.Add(1)
		p.warnOnce(warnRestore, logger, "accept resume: could not restore TCP_DEFER_ACCEPT "+
			"on the listener; it keeps serving without it", owner, id, err)
	}
}

// The kinds of setsockopt failure warnOnce logs once per engine.
const (
	warnSet     = iota // a clear or a guard at the pause
	warnRestore        // a restore at a resume
)

// warnOnce logs msg the first time a failure of this kind happens on this
// engine's PauseState; every later one is only counted (SetFailures). The
// engine's loops or workers call it on their own threads, so the first one to
// fail logs, whichever it is. A nil p (a loop built by a unit test) logs every
// time.
func (p *PauseState) warnOnce(kind int, logger *slog.Logger, msg, owner string, id int, err error) {
	if logger == nil {
		return
	}
	if p != nil {
		flag := &p.warnedSet
		if kind == warnRestore {
			flag = &p.warnedRestore
		}
		if !flag.CompareAndSwap(false, true) {
			return
		}
	}
	logger.Warn(msg+" (logged once per engine)", owner, id, "err", err)
}
