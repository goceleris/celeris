//go:build linux

package iouring

import (
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/conn"
)

// celeris#657: a reverse hand-off detaches a connection with its recv still
// armed, and a recv that outlives the hand-off can read a request some client
// is waiting on. Its CQE is stale, so staleConnCQE dropped it with no trace
// while the #624 ledger balanced. These tests pin the witnesses one counter
// at a time; each fails if its increment is removed.

// handoffLossCounts reads the four witnesses in one go, for messages.
type handoffLossCounts struct {
	closed, transplanted, unattributed, inFlight uint64
}

func readHandoffLoss(s *handoffLossStats) handoffLossCounts {
	return handoffLossCounts{
		closed:       s.staleRecvDataClosed.Load(),
		transplanted: s.staleRecvDataTransplanted.Load(),
		unattributed: s.staleRecvDataUnattributed.Load(),
		inFlight:     s.handoffInFlight.Load(),
	}
}

// newHandoffLossWorker is a ring-backed Worker the hand-off paths can run on
// end to end: cancelConnOps needs a real ring to place its cancel SQEs, and
// the witnesses under test are recorded on either side of it.
func newHandoffLossWorker(t *testing.T, fds ...int) (*Worker, *recordingTarget) {
	t.Helper()
	w := newLedgerWorker(fds...)
	w.ring = newTestRing(t)
	w.handoffLoss = &handoffLossStats{}
	tgt := &recordingTarget{}
	w.transplant.Store(&transplantTargetHolder{target: tgt})
	return w, tgt
}

// idleSyncConn is a sync HTTP/1 keep-alive at a clean boundary, with the
// recv the last response's linked RECV left armed — the state every sync
// conn is in when tryTransplant moves it.
func idleSyncConn(fd int, gen uint32) *connState {
	cs := &connState{fd: fd, generation: gen, liveIdx: -1, detected: true,
		h1State: conn.NewH1State(), recvArmed: true, kernelInflight: 1}
	cs.protocol.Store(int32(engine.HTTP1))
	return cs
}

// staleRecv is a terminal recv completion for (fd, gen) with result res.
func staleRecv(fd int, gen uint32, res int32) *completionEntry {
	return &completionEntry{UserData: encodeUserDataGen(udRecv, fd, gen), Res: res}
}

// TestStaleRecvDataCountsATransplantedConn drives the whole sync path:
// tryTransplant hands off a conn with its recv armed, and that recv then
// completes with a request's bytes. The CQE is stale (the slot is empty),
// its identity was registered by the hand-off, so it must count as
// Transplanted — and as nothing else. The CQE is terminal and the identity
// owes exactly one op, so noteStaleTerminalOp retires the identity on this
// very CQE: the count must be taken before that, or it reads Unattributed.
func TestStaleRecvDataCountsATransplantedConn(t *testing.T) {
	fd, other := socketPairFDs(t)
	defer func() { _ = unix.Close(other) }()
	w, tgt := newHandoffLossWorker(t, fd)
	const gen = 5
	w.conns[fd] = idleSyncConn(fd, gen)
	w.connCount = 1
	w.activeConns.Add(1)

	w.tryTransplant(fd)
	if w.conns[fd] != nil || tgt.adopted.Load() != 1 {
		t.Fatal("the conn was not handed off — the eligibility gates rejected " +
			"the setup, so this test proves nothing about the counter")
	}

	c := staleRecv(fd, gen, 27) // the next request, read by the old recv
	if !w.staleConnCQE(c, fd, c.UserData) {
		t.Fatal("the recv CQE of a handed-off conn was not reported stale")
	}
	got := readHandoffLoss(w.handoffLoss)
	if got.transplanted != 1 || got.closed != 0 || got.unattributed != 0 {
		t.Errorf("stale data after a hand-off counted as %+v, want exactly one "+
			"Transplanted — the request this recv read is lost and nothing says so", got)
	}
	if len(w.closedOps) != 0 {
		t.Errorf("closedOps holds %d entries after the recv's terminal CQE, want 0 "+
			"— the witness must not change the release accounting", len(w.closedOps))
	}
}

// TestStaleRecvDataCountsAnAsyncTransplantedConn is the same loss on the
// self-initiated path a promoted async conn takes (finishAsyncTransplant),
// which registers its identity through its own call site.
func TestStaleRecvDataCountsAnAsyncTransplantedConn(t *testing.T) {
	fd, other := socketPairFDs(t)
	defer func() { _ = unix.Close(other) }()
	w, tgt := newHandoffLossWorker(t, fd)
	const gen = 6
	cs := &connState{fd: fd, generation: gen, liveIdx: -1, recvArmed: true, kernelInflight: 1}
	w.conns[fd] = cs
	w.connCount = 1
	w.activeConns.Add(1)

	w.finishAsyncTransplant(cs)
	if w.conns[fd] != nil || tgt.adopted.Load() != 1 {
		t.Fatal("the async conn was not handed off — setup rejected, the counter is unproven")
	}

	c := staleRecv(fd, gen, 31)
	if !w.staleConnCQE(c, fd, c.UserData) {
		t.Fatal("the recv CQE of a handed-off async conn was not reported stale")
	}
	got := readHandoffLoss(w.handoffLoss)
	if got.transplanted != 1 || got.closed != 0 || got.unattributed != 0 {
		t.Errorf("stale data after an async hand-off counted as %+v, want exactly "+
			"one Transplanted", got)
	}
}

// TestStaleRecvDataCountsAClosedConn is the benign class: the conn was
// CLOSED by this worker (the close paths register through
// noteClosedInflight) and the peer's bytes raced the close. It must count as
// Closed, never as a hand-off loss.
func TestStaleRecvDataCountsAClosedConn(t *testing.T) {
	const fd, gen = 9, 3
	w := &Worker{conns: make([]*connState, 16), handoffLoss: &handoffLossStats{}}
	cs := &connState{fd: fd, generation: gen, kernelInflight: 1, recvArmed: true}
	w.noteClosedInflight(cs)
	w.queuePendingRelease(cs)

	c := staleRecv(fd, gen, 40)
	if !w.staleConnCQE(c, fd, c.UserData) {
		t.Fatal("the recv CQE of a closed conn was not reported stale")
	}
	got := readHandoffLoss(w.handoffLoss)
	if got.closed != 1 || got.transplanted != 0 || got.unattributed != 0 {
		t.Errorf("stale data after a close counted as %+v, want exactly one Closed", got)
	}
}

// TestStaleRecvDataCountsAnUnattributedIdentity: a stale data CQE whose
// identity nothing registered (no close with ops in flight, no hand-off, or
// already retired) still read bytes off a socket. It must be counted — as
// Unattributed — rather than vanish.
func TestStaleRecvDataCountsAnUnattributedIdentity(t *testing.T) {
	const fd, gen = 11, 2
	w := &Worker{conns: make([]*connState, 16), handoffLoss: &handoffLossStats{}}

	c := staleRecv(fd, gen, 18)
	if !w.staleConnCQE(c, fd, c.UserData) {
		t.Fatal("a recv CQE for an empty slot was not reported stale")
	}
	got := readHandoffLoss(w.handoffLoss)
	if got.unattributed != 1 || got.closed != 0 || got.transplanted != 0 {
		t.Errorf("stale data for an unknown identity counted as %+v, want exactly "+
			"one Unattributed", got)
	}
}

// TestStaleRecvDataIgnoresCompletionsWithoutData is the other side of every
// test above: a stale completion that read NOTHING lost nothing, so neither a
// cancelled recv (-ECANCELED), nor a FIN (0), nor a stale SEND, nor a live
// conn's own data CQE may move any StaleRecvData counter — even for an
// identity registered as handed off.
func TestStaleRecvDataIgnoresCompletionsWithoutData(t *testing.T) {
	const fd, gen = 7, 4
	w := &Worker{conns: make([]*connState, 16), handoffLoss: &handoffLossStats{}}
	gone := &connState{fd: fd, generation: gen, kernelInflight: 3, recvArmed: true}
	w.noteHandedOffInflight(gone)

	for _, c := range []*completionEntry{
		staleRecv(fd, gen, -int32(unix.ECANCELED)),
		staleRecv(fd, gen, 0),
		{UserData: encodeUserDataGen(udSend, fd, gen), Res: 64},
	} {
		if !w.staleConnCQE(c, fd, c.UserData) {
			t.Fatalf("CQE %#x res %d not reported stale", c.UserData, c.Res)
		}
	}
	// A live conn's own completion is not stale and lost nothing.
	const liveFD, liveGen = 3, 8
	w.conns[liveFD] = &connState{fd: liveFD, generation: liveGen, recvArmed: true, kernelInflight: 1}
	live := staleRecv(liveFD, liveGen, 55)
	if w.staleConnCQE(live, liveFD, live.UserData) {
		t.Fatal("a live conn's own recv CQE was reported stale")
	}

	if got := readHandoffLoss(w.handoffLoss); got != (handoffLossCounts{}) {
		t.Errorf("completions that read no stale bytes moved the witnesses: %+v, want all 0", got)
	}
}

// TestTransplantHandoffInFlightCountsTryTransplant pins the precondition
// witness on the sync hand-off site: it counts a hand-off made with a recv
// armed, a kernel op outstanding, or a SEND_ZC notification pending — each
// term on its own — and does not count one made with nothing in flight.
func TestTransplantHandoffInFlightCountsTryTransplant(t *testing.T) {
	type state struct {
		name                      string
		recvArmed, zcNotifPending bool
		kernelInflight            int32
		want                      uint64 // cumulative witness after this hand-off
	}
	states := []state{
		{name: "nothing in flight", want: 0},
		{name: "recv armed", recvArmed: true, kernelInflight: 1, want: 1},
		{name: "kernel op only", kernelInflight: 1, want: 2},
		{name: "SEND_ZC notif only", zcNotifPending: true, want: 3},
	}
	fds := make([]int, 0, len(states))
	for range states {
		fd, other := socketPairFDs(t)
		t.Cleanup(func() { _ = unix.Close(other) })
		fds = append(fds, fd)
	}
	w, tgt := newHandoffLossWorker(t, fds...)
	for i, st := range states {
		fd := fds[i]
		cs := idleSyncConn(fd, uint32(20+i))
		cs.recvArmed, cs.kernelInflight, cs.zcNotifPending = st.recvArmed, st.kernelInflight, st.zcNotifPending
		w.conns[fd] = cs
		w.connCount++
		w.activeConns.Add(1)

		w.tryTransplant(fd)
		if w.conns[fd] != nil || tgt.adopted.Load() != int64(i+1) {
			t.Fatalf("%s: the conn was not handed off — the counter is unproven", st.name)
		}
		if got := w.handoffLoss.handoffInFlight.Load(); got != st.want {
			t.Errorf("%s: TransplantHandoffInFlight = %d after this hand-off, want %d",
				st.name, got, st.want)
		}
	}
}

// TestTransplantHandoffInFlightCountsFinishAsyncTransplant is the same
// witness on the async hand-off site. finishAsyncTransplant already refuses a
// conn with a SEND or a SEND_ZC notification outstanding, so what it can hand
// off with an op in flight is the armed recv.
func TestTransplantHandoffInFlightCountsFinishAsyncTransplant(t *testing.T) {
	fdIdle, otherIdle := socketPairFDs(t)
	defer func() { _ = unix.Close(otherIdle) }()
	fdArmed, otherArmed := socketPairFDs(t)
	defer func() { _ = unix.Close(otherArmed) }()
	w, tgt := newHandoffLossWorker(t, fdIdle, fdArmed)

	quiet := &connState{fd: fdIdle, generation: 1, liveIdx: -1}
	w.conns[fdIdle] = quiet
	w.connCount++
	w.activeConns.Add(1)
	w.finishAsyncTransplant(quiet)
	if w.conns[fdIdle] != nil || tgt.adopted.Load() != 1 {
		t.Fatal("the quiet async conn was not handed off — setup rejected")
	}
	if got := w.handoffLoss.handoffInFlight.Load(); got != 0 {
		t.Errorf("TransplantHandoffInFlight = %d after a hand-off with nothing in "+
			"flight, want 0", got)
	}

	armed := &connState{fd: fdArmed, generation: 2, liveIdx: -1, recvArmed: true, kernelInflight: 1}
	w.conns[fdArmed] = armed
	w.connCount++
	w.activeConns.Add(1)
	w.finishAsyncTransplant(armed)
	if w.conns[fdArmed] != nil || tgt.adopted.Load() != 2 {
		t.Fatal("the armed async conn was not handed off — setup rejected")
	}
	if got := w.handoffLoss.handoffInFlight.Load(); got != 1 {
		t.Errorf("TransplantHandoffInFlight = %d after a hand-off with its recv "+
			"armed, want 1", got)
	}
}
