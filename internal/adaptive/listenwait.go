//go:build linux

package adaptive

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/deferlinger"
	"github.com/goceleris/celeris/internal/engine"
)

// celeris#683: a switch must not close the outgoing engine's listeners before
// the incoming engine has one.
//
// ResumeAccept on the epoll and io_uring sub-engines wakes the incoming
// engine's parked loops and returns; each loop then re-creates its listen
// socket on its own thread, a few tens of microseconds later when it is
// scheduled promptly and as late as the scheduler allows when it is not.
// beginPause then starts the outgoing engine's pause. While the outgoing
// engine lingers (celeris#662: TCP_DEFER_ACCEPT cleared, listener open for
// about 1.5 s) that is harmless, because the outgoing engine keeps the port
// served for far longer than a loop takes to wake. When it does not linger
// (resource.Config.DisableDeferAccept, or a zero linger) its listeners close
// at once, and a dial that arrives between that close and the first
// re-created listener finds the SO_REUSEPORT group empty and is refused.
//
// The sub-engines expose no signal that a loop has listened (the field is
// loop-thread-only), so performSwitch asks the kernel: it lists the TCP LISTEN
// sockets bound to the port before it resumes the incoming engine, and, in the
// one configuration that can open the gap, waits for a socket that was not in
// that list before it pauses the outgoing one. The list is a sock_diag dump
// filtered to LISTEN, so it costs a few listening sockets, not the connection
// table.

// listenWaitBound is how long performSwitch waits for the incoming engine to
// listen before it pauses the outgoing one anyway. The measured lag is
// microseconds; the bound exists so that an incoming engine that cannot
// listen (its loops exited, the port was taken) does not hold the switch,
// and e.mu with it, indefinitely. A variable so a test can shorten it.
var listenWaitBound = 250 * time.Millisecond

// listenWaitPoll is the interval between two kernel lists while waiting.
const listenWaitPoll = 50 * time.Microsecond

// listenWatch is the set of LISTEN sockets on the switch's port at the moment
// the incoming engine was resumed.
type listenWatch struct {
	port   int
	before map[uint32]struct{}
}

// listenWaitCounters count what the wait did. They are deliberately not an
// EngineMetrics field: a timeout is a diagnostic of one switch, and a new
// metric field carries the whole published-series catch-up (RULE 93).
type listenWaitCounters struct {
	waits    atomic.Uint64 // waits started
	timeouts atomic.Uint64 // waits that gave up at listenWaitBound
	errors   atomic.Uint64 // waits that could not list sockets and were skipped
}

// listenWatchNeeded reports whether this switch can open a listener-free gap,
// and the port to watch. It is the configuration in which the outgoing engine
// closes its listeners at the pause instead of lingering: the listener was
// created without TCP_DEFER_ACCEPT, or the linger is zero.
//
// Only a real sub-engine is waited for: one that implements beginPauser is an
// epoll or io_uring engine with listeners of its own. A fake engine in a unit
// test has none, and a wait for it would only run into the bound.
func (e *Engine) listenWatchNeeded(incoming, outgoing engine.Engine, freshlyBuilt bool) (port int, ok bool) {
	if freshlyBuilt {
		// A standby that was just built has been Listen'd and has bound:
		// its listeners exist before the resume, and a wait for a NEW one
		// would only run into the bound.
		return 0, false
	}
	if _, isReal := outgoing.(beginPauser); !isReal {
		return 0, false
	}
	if !e.cfg.DisableDeferAccept && deferlinger.Linger() > 0 {
		return 0, false // the outgoing engine lingers; see above
	}
	ta, isTCP := incoming.Addr().(*net.TCPAddr)
	if !isTCP || ta.Port == 0 {
		return 0, false
	}
	return ta.Port, true
}

// newListenWatch lists the LISTEN sockets on port now. An error means the
// kernel could not be asked (no netlink, a seccomp profile): the caller goes
// on without the wait, as it did before.
func newListenWatch(port int) (*listenWatch, error) {
	set, err := listenInodes(port)
	if err != nil {
		return nil, err
	}
	return &listenWatch{port: port, before: set}, nil
}

// wait polls the kernel until a LISTEN socket that was not there when the
// watch was made exists on the port, or bound passes. It returns how long it
// waited and whether a new socket appeared.
func (w *listenWatch) wait(bound time.Duration) (time.Duration, bool, error) {
	t0 := time.Now()
	for {
		set, err := listenInodes(w.port)
		if err != nil {
			return time.Since(t0), false, err
		}
		for ino := range set {
			if _, was := w.before[ino]; !was {
				return time.Since(t0), true, nil
			}
		}
		if time.Since(t0) >= bound {
			return time.Since(t0), false, nil
		}
		time.Sleep(listenWaitPoll)
	}
}

// listenInodes returns the inodes of the TCP LISTEN sockets bound to port in
// this network namespace, IPv4 and IPv6, from a sock_diag dump.
func listenInodes(port int) (map[uint32]struct{}, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return nil, fmt.Errorf("sock_diag socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	// A dump answers in microseconds; the timeout is for a kernel that does
	// not, so that the switch cannot be held by it.
	tv := unix.NsecToTimeval(int64(100 * time.Millisecond))
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)

	out := make(map[uint32]struct{})
	for _, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		if err := dumpListeners(fd, family, port, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

const (
	nlmsgHdrLen   = 16 // struct nlmsghdr
	diagReqLen    = 56 // struct inet_diag_req_v2
	diagMsgInode  = 68 // offset of idiag_inode in struct inet_diag_msg
	diagMsgMinLen = 72 // sizeof(struct inet_diag_msg)
	diagMsgSport  = 4  // offset of id.idiag_sport in struct inet_diag_msg
	tcpListen     = 10 // TCP_LISTEN
)

func dumpListeners(fd int, family uint8, port int, out map[uint32]struct{}) error {
	var req [nlmsgHdrLen + diagReqLen]byte
	ne := binary.NativeEndian
	ne.PutUint32(req[0:], uint32(len(req)))
	ne.PutUint16(req[4:], unix.SOCK_DIAG_BY_FAMILY)
	ne.PutUint16(req[6:], unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	ne.PutUint32(req[8:], 1) // seq
	b := req[nlmsgHdrLen:]
	b[0] = family
	b[1] = unix.IPPROTO_TCP
	ne.PutUint32(b[4:], 1<<tcpListen)
	binary.BigEndian.PutUint16(b[8:], uint16(port)) // id.idiag_sport, network order
	if err := unix.Sendto(fd, req[:], 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("sock_diag send: %w", err)
	}
	buf := make([]byte, 32<<10)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return fmt.Errorf("sock_diag recv: %w", err)
		}
		msgs := buf[:n]
		for len(msgs) >= nlmsgHdrLen {
			l := int(ne.Uint32(msgs[0:]))
			typ := ne.Uint16(msgs[4:])
			if l < nlmsgHdrLen || l > len(msgs) {
				return errors.New("sock_diag: malformed netlink message")
			}
			switch typ {
			case unix.NLMSG_DONE:
				return nil
			case unix.NLMSG_ERROR:
				return errors.New("sock_diag: the kernel refused the dump")
			}
			body := msgs[nlmsgHdrLen:l]
			// The sport filter is applied by the kernel; check it anyway, so a
			// kernel that ignores it cannot make another port's listener
			// look like ours.
			if len(body) >= diagMsgMinLen && int(binary.BigEndian.Uint16(body[diagMsgSport:])) == port {
				out[ne.Uint32(body[diagMsgInode:])] = struct{}{}
			}
			msgs = msgs[(l+3)&^3:]
		}
	}
}

// watchIncomingListener takes the watch for a switch that needs one, before the
// incoming engine is resumed. It returns nil when none is needed or the kernel
// cannot be asked.
func (e *Engine) watchIncomingListener(incoming, outgoing engine.Engine, freshlyBuilt bool) *listenWatch {
	port, ok := e.listenWatchNeeded(incoming, outgoing, freshlyBuilt)
	if !ok {
		return nil
	}
	w, err := newListenWatch(port)
	if err != nil {
		if e.listenWait.errors.Add(1) == 1 {
			e.logger.Warn("engine switch: cannot list listening sockets; the outgoing engine's listeners "+
				"will close without waiting for the incoming engine's", "err", err)
		}
		return nil
	}
	e.listenWait.waits.Add(1)
	return w
}

// awaitIncomingListener holds the switch, after the incoming engine has been
// resumed and before the outgoing one is paused, until the incoming engine has
// a listener in LISTEN. The caller holds e.mu and NOT freezeState, so a driver
// register or unregister is not stalled behind it; nothing the incoming
// engine's loops need to re-create a listener takes either lock.
func (e *Engine) awaitIncomingListener(w *listenWatch) {
	if w == nil {
		return
	}
	waited, listening, err := w.wait(listenWaitBound)
	switch {
	case err != nil:
		e.listenWait.errors.Add(1)
		e.logger.Warn("engine switch: lost the kernel's list of listening sockets while waiting for the "+
			"incoming engine to listen; pausing the outgoing engine now", "err", err, "waited", waited)
	case !listening:
		e.listenWait.timeouts.Add(1)
		e.logger.Warn("engine switch: the incoming engine had no new listener after the bound; pausing "+
			"the outgoing engine anyway. New connections may be refused until it listens",
			"waited", waited, "port", w.port)
	}
}

// listenWaitTimeouts is the number of waits that gave up at the bound.
func (e *Engine) listenWaitTimeouts() uint64 { return e.listenWait.timeouts.Load() }
