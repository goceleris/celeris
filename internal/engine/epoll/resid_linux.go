//go:build linux

package epoll

import (
	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/diag/resid"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/mono"
)

// The server-residence stamps of celeris#757 (internal/diag/resid has the
// design, the histograms and the export). THROWAWAY DIAGNOSTIC, never merged.
//
// Every site in the loop is `if rs := l.resid; rs != nil`: Loop.resid is nil
// unless CELERIS_DIAG_RESID is set, so with the gate off a site is one load and
// one predictable branch of a loop-local field, and nothing else changes.
//
// The sample in flight lives here, on the loop, and not on connState (whose
// size class is pinned by TestConnStateStaysInItsSizeClass): a read is sampled,
// ProcessH2 runs, flushInline writes, all in one drainRead iteration on the
// loop's thread, so one slot is enough. Nothing here is touched by another
// goroutine: flushWrites' dispatch-goroutine calls (onLoopThread false) are
// ignored.
type residState struct {
	*resid.Loop

	cs   *connState // the conn of the sample in flight, nil when none
	ew   int64      // T_ew: epoll_wait return of this turn (l.cachedNow)
	rx   int64      // T_rx: after the read returned
	proc int64      // T_proc: after ProcessH2 returned and its locks were released
	wr   int64      // T_wr: after the write(2) that completed the queue returned; 0 if none
	// krx is the kernel's receive stamp of the last skb the read took
	// (SO_TIMESTAMPNS, CLOCK_REALTIME ns), rxWall the wall clock after the read.
	krx, rxWall int64
	k0, p0      uint64 // InlineCount and PoolSubmits before ProcessH2
	k, pool     uint64 // ... and their deltas after it
	partial     bool   // a write was made and did not take the whole queue
	inProc      bool   // inside ProcessH2 (the open question of DESIGN-757: does a write happen there?)

	bStart int64 // T_ew of a sampled turn that has events (the start of B), 0 otherwise

	oob [64]byte // control-message buffer of the sampled recvmsg
}

// newResidState returns the loop's state, nil when the gate is off.
func newResidState(id int) *residState {
	r := resid.NewLoop(id)
	if r == nil {
		return nil
	}
	return &residState{Loop: r}
}

// sampleRead reports whether this read of cs is sampled: only the plain read
// of an HTTP/2 conn the loop owns (not a promoted conn's readPromoted, and not
// before protocol detection). Loop thread.
func (r *residState) sampleRead(cs *connState) bool {
	return cs.detected && !cs.asyncPromoted && cs.protocol == engine.H2C && r.SampleRecv()
}

// recv is the sampled read: recvmsg with the kernel's receive stamp. T_rx and
// the wall clock are taken as soon as it returns.
func (r *residState) recv(fd int, buf []byte) (int, error) {
	n, oobn, _, _, err := unix.Recvmsg(fd, buf, r.oob[:], 0)
	r.rx = mono.Now()
	r.rxWall = resid.WallNowNS()
	r.krx = 0
	if err == nil && oobn > 0 {
		r.krx = resid.KernelRxNS(r.oob[:oobn], unix.SOL_SOCKET, unix.SO_TIMESTAMPNS)
	}
	return n, err
}

// arm starts the sample of cs: the read returned n > 0 bytes.
func (r *residState) arm(cs *connState, ew int64) {
	r.cs, r.ew, r.wr, r.proc, r.partial, r.inProc = cs, ew, 0, 0, false, false
}

// procBegin is called just before ProcessH2, inside the detachMu region on the
// async path (ProcessH2 can run on the dispatch goroutine under that lock, and
// the counters are read under it too).
func (r *residState) procBegin(h2 *conn.H2State) {
	r.k0, r.p0 = h2.ResidCounts()
	r.inProc = true
}

// procEnd is called right after ProcessH2, inside the same lock region.
func (r *residState) procEnd(h2 *conn.H2State) {
	r.inProc = false
	k1, p1 := h2.ResidCounts()
	r.k, r.pool = k1-r.k0, p1-r.p0
}

// procStamp takes T_proc, after the locks of the ProcessH2 call are released:
// the handler's work is in S2, the flush's in S3.
func (r *residState) procStamp() { r.proc = mono.Now() }

// noteWrite is flushWrites' hook: a write(2) of cs's queue returned. complete
// says that it took the whole queue. Loop thread only.
func (r *residState) noteWrite(cs *connState, complete bool) {
	if r.cs != cs {
		return
	}
	if r.inProc {
		r.InProc.Add(1)
	}
	if !complete {
		r.partial = true
		return
	}
	r.wr = mono.Now()
}

// finish closes the sample after flushInline: it records the segments of a
// timed request, or counts why the sample could not be timed.
func (r *residState) finish() {
	cs := r.cs
	r.cs = nil
	if cs == nil {
		return
	}
	switch {
	case r.pool != 0:
		r.Unst.Add(1) // a pool-bound stream: its residence is not this path's
		return
	case r.k == 0:
		return // frames that carried no request (SETTINGS ack, WINDOW_UPDATE, PING)
	case r.wr == 0 && r.partial:
		r.Part.Add(1)
		return
	case r.wr == 0:
		r.NoWr.Add(1)
		return
	case r.wr < r.proc:
		r.Early.Add(1) // a write returned before ProcessH2 did: S3 would be negative
		return
	}
	k := r.k
	r.N.Add(1)
	r.H[resid.HistR].Add(r.wr-r.rx, k)
	r.H[resid.HistS1].Add(r.rx-r.ew, k)
	r.H[resid.HistS2].Add(r.proc-r.rx, k)
	r.H[resid.HistS3].Add(r.wr-r.proc, k)
	if r.krx > 0 {
		r.H[resid.HistG].Add(r.rxWall-r.krx, k)
	}
	if h := residSampleHook; h != nil {
		h(residSample{Ew: r.ew, Rx: r.rx, Proc: r.proc, Wr: r.wr, K: k, G: r.rxWall - r.krx, HasG: r.krx > 0})
	}
}

// turnBegin is called before epoll_wait: it returns the entry stamp when this
// turn is sampled, else 0.
func (r *residState) turnBegin() int64 {
	r.bStart = 0
	if !r.SampleTurn() {
		return 0
	}
	return mono.Now()
}

// turnWoke is called when epoll_wait returned n > 0 events at ew.
func (r *residState) turnWoke(t0, ew int64) {
	if t0 == 0 {
		return
	}
	r.H[resid.HistWt].Add(ew-t0, 1)
	r.bStart = ew
}

// turnEnd is called at the end of the batch: B of a sampled turn.
func (r *residState) turnEnd() {
	if r.bStart == 0 {
		return
	}
	r.H[resid.HistB].Add(mono.Now()-r.bStart, 1)
	r.bStart = 0
}

// residSample is one timed sample, for the test seam.
type residSample struct {
	Ew, Rx, Proc, Wr int64 // mono.Now() scale
	K                uint64
	G                int64 // wall ns, valid when HasG
	HasG             bool
}

// residSampleHook, when non-nil (tests only, set before the engine starts), is
// called with every timed sample on the loop's thread.
var residSampleHook func(residSample)
