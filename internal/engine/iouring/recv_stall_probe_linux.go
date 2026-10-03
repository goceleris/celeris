//go:build linux

package iouring

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// recvStallProbe prints one record per recv-arming stall episode that
// outlived recvStallProbeFloor, keyed by the connection's remote address.
//
// The aggregate witnesses in recvArmStats answer "did the engine stall a
// recv, and for how long at worst". They cannot answer "was it THIS
// connection", which is the question an end-to-end oracle has to join on:
// the engine knows the peer as cs.remoteAddr and a client knows itself as
// LocalAddr(), so the pair is a join key (the technique that settled
// celeris#562). Without it, a run in which n connections stalled and n
// connections timed out is only circumstantial.
//
// Off unless CELERIS_DEBUG_RECV_STALL=1, and even then it is reached only
// from endRecvStall — once per episode, never per pass and never per
// request. The floor keeps the sub-millisecond episodes that ordinary
// SQ-ring pressure produces out of the log; only a stall long enough to
// matter to a peer is printed.
var recvStallProbeActive = os.Getenv("CELERIS_DEBUG_RECV_STALL") == "1"

// recvStallProbeFloor is the shortest episode worth a record. An SQ ring
// that fills and drains inside one event-loop pass is normal operation.
const recvStallProbeFloor = 250 * time.Millisecond

func recvStallProbe(cs *connState, d int64) {
	if !recvStallProbeActive || d < int64(recvStallProbeFloor) {
		return
	}
	fmt.Fprintf(os.Stderr, "RECVSTALL raddr=%s fd=%d dur=%s paused=%v armed=%v needsRecv=%v sending=%v\n",
		cs.remoteAddr, cs.fd, time.Duration(d).Round(time.Millisecond),
		cs.recvPaused, cs.recvArmed, cs.needsRecv, cs.sending)
}

// recvSilenceFloor is how long a live, unpaused connection may go without
// a single recv completion before the watchdog records it. The repro's
// peers write continuously, so three seconds of nothing is not slowness.
const recvSilenceFloor = 3 * time.Second

// reportRecvSilence records, once per connection, the engine's own view of
// a connection that has stopped receiving.
//
// The aggregate witnesses say whether a particular arming bug fired. This
// says what state the connection is ACTUALLY in when it goes silent, which
// is the question the oracle's own failure message asks for
// ("dump the engine's per-conn state"). SIOCINQ/SIOCOUTQ are read from the
// worker thread on the same fd, so the kernel queue depths are joined to
// the engine state without a second process racing /proc.
//
// Called from checkTimeouts on its existing ~50-100 ms cadence, under the
// probe gate, and short-circuits on the per-conn flag after the first
// record. Off in a release build's hot path entirely.
func (w *Worker) reportRecvSilence(cs *connState, now int64) {
	if cs.stallReported || cs.closing || cs.lastRecvCQE == 0 {
		return
	}
	silent := now - cs.lastRecvCQE
	if silent < int64(recvSilenceFloor) {
		return
	}
	cs.stallReported = true
	inq, _ := unix.IoctlGetInt(cs.fd, unix.SIOCINQ)
	outq, _ := unix.IoctlGetInt(cs.fd, unix.SIOCOUTQ)
	// cs.sendBuf, cs.writeBuf and cs.h1State belong to detachMu once the
	// connection is detached — the middleware goroutine appends to writeBuf
	// under that lock from makeWriteFn, and switchToH2Local nils h1State
	// under it. Reading them bare from the worker thread is a data race,
	// and the race detector says so on the first run; TryLock (never Lock)
	// for the same reason snapshotH1Deadlines uses it — a probe must not be
	// able to pin the event loop behind a slow handler (celeris#593). When
	// the lock is busy the shared fields print as -1 and the worker-owned
	// arming state, which is the part that matters, still prints.
	sendN, writeN, detached := -1, -1, false
	locked := true
	if mu := cs.detachMu; mu != nil {
		locked = mu.TryLock()
	}
	if locked {
		sendN, writeN = len(cs.sendBuf), len(cs.writeBuf)
		detached = cs.h1State != nil && cs.h1State.Detached.Load()
		if mu := cs.detachMu; mu != nil {
			mu.Unlock()
		}
	}
	fmt.Fprintf(os.Stderr,
		"RECVSILENT raddr=%s fd=%d silent=%s armed=%v paused=%v desired=%v needsRecv=%v sending=%v dirty=%v "+
			"closing=%v linked=%v zcNotif=%v cancelPending=%d inflight=%d outstanding=%d sendBuf=%d writeBuf=%d "+
			"pauses=%d detached=%v locked=%v bufRing=%v inq=%d outq=%d\n",
		cs.remoteAddr, cs.fd, time.Duration(silent).Round(time.Millisecond),
		cs.recvArmed, cs.recvPaused, cs.recvPauseDesired.Load(), cs.needsRecv, cs.sending, cs.dirty,
		cs.closing, cs.recvLinked, cs.zcNotifPending, cs.recvCancelPending, cs.kernelInflight,
		cs.recvOutstanding, sendN, writeN, cs.pausesApplied, detached, locked,
		w.bufRing != nil, inq, outq)
}

// linkedRecvFloor is the shortest linked-recv wait worth a record. A
// healthy request/response send completes in microseconds.
const linkedRecvFloor = 250 * time.Millisecond

// linkedRecvProbe prints one record per linked recv that was held behind
// its send for longer than linkedRecvFloor, keyed by remote address so it
// joins to the peer's own view of the connection. Same gate and same
// cold-path discipline as recvStallProbe.
func linkedRecvProbe(cs *connState, d int64) {
	if !recvStallProbeActive || d < int64(linkedRecvFloor) {
		return
	}
	inq, _ := unix.IoctlGetInt(cs.fd, unix.SIOCINQ)
	outq, _ := unix.IoctlGetInt(cs.fd, unix.SIOCOUTQ)
	fmt.Fprintf(os.Stderr, "LINKBLOCK raddr=%s fd=%d blocked=%s inq=%d outq=%d sendBuf=%d writeBuf=%d\n",
		cs.remoteAddr, cs.fd, time.Duration(d).Round(time.Millisecond), inq, outq,
		len(cs.sendBuf), len(cs.writeBuf))
}
