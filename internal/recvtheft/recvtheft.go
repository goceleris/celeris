//go:build validation

package recvtheft

import (
	"sync"
	"sync/atomic"
	"time"
)

// Enabled is true in a -tags=validation build and false (a constant, so
// guarded code compiles away) in production (recvtheft_off.go).
const Enabled = true

// ArmSeq records the SQ ring sequence number at which a connection's latest
// recv SQE was placed (the ring's tail minus one right after the placement).
// Compared with the kernel's SQ head it says whether the kernel has consumed
// that SQE yet. Worker-thread only, like every other recv field of the
// connection. Zero-size in production.
type ArmSeq struct{ seq uint32 }

// Set records the sequence number of the recv SQE just placed.
func (a *ArmSeq) Set(seq uint32) { a.seq = seq }

// Get returns the recorded sequence number.
func (a *ArmSeq) Get() uint32 { return a.seq }

// closeWithUnsubmittedRecv is the witness: closes (finishClose,
// finishCloseDetached) reached while the connection's recv SQE was still in
// the SQ ring, not yet consumed by the kernel.
var closeWithUnsubmittedRecv atomic.Uint64

// NoteCloseWithUnsubmittedRecv counts one such close. Engine hook.
func NoteCloseWithUnsubmittedRecv() { closeWithUnsubmittedRecv.Add(1) }

// CloseWithUnsubmittedRecv returns the witness count since process start.
func CloseWithUnsubmittedRecv() uint64 { return closeWithUnsubmittedRecv.Load() }

// closeWithLinkedRecv is the witness of the linked form (celeris#685): closes
// reached while the connection's recv was chained behind a SEND
// (IOSQE_IO_LINK) and had not completed. Such a recv was submitted, so the
// witness above does not count it, but the kernel issues it, and resolves
// its descriptor number, only after the SEND completes.
var closeWithLinkedRecv atomic.Uint64

// NoteCloseWithLinkedRecv counts one such close. Engine hook.
func NoteCloseWithLinkedRecv() { closeWithLinkedRecv.Add(1) }

// CloseWithLinkedRecv returns the linked witness count since process start.
func CloseWithLinkedRecv() uint64 { return closeWithLinkedRecv.Load() }

// Options configures one [Trial].
type Options struct {
	// SubmitBeforeClose is the control arm: the close paths submit the SQ
	// ring after queueing their cancels and before closing the descriptor,
	// so a recv prepared earlier in the iteration is issued while its
	// number still names the closing connection's socket.
	SubmitBeforeClose bool
	// PromoteGate bounds how long a worker that has just re-armed a
	// promoted connection's recv (promoteConnToAsync) waits for the
	// dispatch goroutine to queue a close. Zero disables the gate. The
	// gate fires once per trial.
	PromoteGate time.Duration
	// HoldMax bounds each hold, so a test that stops driving the trial
	// cannot park a worker for longer than this. Zero means 5 s.
	HoldMax time.Duration
	// LinkedRecv makes the close hold fire on a close with a LINKED recv
	// owed (the celeris#685 linked form, [CloseWithLinkedRecv]) instead of
	// one with an unsubmitted recv.
	LinkedRecv bool
}

// CloseEvent is the close hold firing: worker Worker closed descriptor FD
// with a recv SQE for it still unsubmitted, and is parked.
type CloseEvent struct{ Worker, FD int }

// AcceptEvent is an accept the engine completed while a trial had a target:
// worker Worker accepted a connection as descriptor FD.
type AcceptEvent struct{ Worker, FD int }

// StaleRecv is one stale recv completion that carried data: the recv's
// identity (FD, Gen), its result and the first bytes it wrote.
type StaleRecv struct {
	Worker int
	FD     int
	Gen    uint32
	Res    int32
	Head   []byte
}

// Trial is one armed run of the measurement. At most one is armed at a time.
type Trial struct {
	opts Options

	gateUsed atomic.Bool

	closeFired   atomic.Bool
	closeCh      chan CloseEvent
	releaseClose chan struct{}
	relCloseOnce sync.Once
	target       atomic.Int64
	holder       atomic.Int64

	acceptCh      chan AcceptEvent
	acceptFired   atomic.Bool
	acceptHeldCh  chan AcceptEvent
	releaseAccept chan struct{}
	relAcceptOnce sync.Once

	mu    sync.Mutex
	stale []StaleRecv
}

var current atomic.Pointer[Trial]

// Arm arms a new trial and returns it. A trial still armed is disarmed
// (released) first.
func Arm(o Options) *Trial {
	if o.HoldMax <= 0 {
		o.HoldMax = 5 * time.Second
	}
	t := &Trial{
		opts:          o,
		closeCh:       make(chan CloseEvent, 1),
		releaseClose:  make(chan struct{}),
		acceptCh:      make(chan AcceptEvent, 64),
		acceptHeldCh:  make(chan AcceptEvent, 1),
		releaseAccept: make(chan struct{}),
	}
	t.target.Store(-1)
	t.holder.Store(-1)
	if old := current.Swap(t); old != nil {
		old.release()
	}
	return t
}

// Disarm releases every hold of t and disarms it.
func (t *Trial) Disarm() {
	current.CompareAndSwap(t, nil)
	t.release()
}

func (t *Trial) release() {
	t.ReleaseClose()
	t.ReleaseAccept()
}

// WaitClose waits up to d for the close hold to fire.
func (t *Trial) WaitClose(d time.Duration) (CloseEvent, bool) {
	select {
	case ev := <-t.closeCh:
		return ev, true
	case <-time.After(d):
		return CloseEvent{}, false
	}
}

// ReleaseClose lets the worker parked by the close hold continue.
func (t *Trial) ReleaseClose() { t.relCloseOnce.Do(func() { close(t.releaseClose) }) }

// NextAccept waits up to d for the next accept completed since the close
// hold fired.
func (t *Trial) NextAccept(d time.Duration) (AcceptEvent, bool) {
	select {
	case ev := <-t.acceptCh:
		return ev, true
	case <-time.After(d):
		return AcceptEvent{}, false
	}
}

// WaitAcceptHeld waits up to d for the accept hold to fire.
func (t *Trial) WaitAcceptHeld(d time.Duration) (AcceptEvent, bool) {
	select {
	case ev := <-t.acceptHeldCh:
		return ev, true
	case <-time.After(d):
		return AcceptEvent{}, false
	}
}

// ReleaseAccept lets the worker parked by the accept hold continue.
func (t *Trial) ReleaseAccept() { t.relAcceptOnce.Do(func() { close(t.releaseAccept) }) }

// Stale returns a copy of the stale recv completions with data recorded while
// t was armed.
func (t *Trial) Stale() []StaleRecv {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]StaleRecv(nil), t.stale...)
}

// GateUsed reports whether the promote gate fired in this trial.
func (t *Trial) GateUsed() bool { return t.gateUsed.Load() }

// SubmitBeforeClose reports whether the armed trial is the control arm.
// Engine hook, worker thread.
func SubmitBeforeClose() bool {
	t := current.Load()
	return t != nil && t.opts.SubmitBeforeClose
}

// AfterPromoteArm is called by promoteConnToAsync after it re-armed the
// connection's recv. The first time in a trial with a PromoteGate, it waits
// until queued reports that a detach-queue entry is pending (the dispatch
// goroutine's close) or the gate expires, so the close is drained in the same
// loop iteration as the re-arm. Engine hook, worker thread.
func AfterPromoteArm(queued func() bool) {
	t := current.Load()
	if t == nil || t.opts.PromoteGate <= 0 || t.closeFired.Load() || !t.gateUsed.CompareAndSwap(false, true) {
		return
	}
	deadline := time.Now().Add(t.opts.PromoteGate)
	for !queued() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Microsecond)
	}
}

// HoldAfterClose is deferred by finishClose / finishCloseDetached when they
// found the connection's recv SQE unsubmitted (linked false), or its recv
// linked behind a SEND and not completed (linked true), so it runs when the
// close path returns: right after the descriptor is closed, on a tree that
// closes it there. The first time in a trial whose [Options.LinkedRecv]
// matches linked, it records (worker, fd) as the trial's target and parks the
// worker until [Trial.ReleaseClose] or HoldMax. Engine hook, worker thread.
func HoldAfterClose(worker, fd int, linked bool) {
	t := current.Load()
	if t == nil || t.opts.LinkedRecv != linked || !t.closeFired.CompareAndSwap(false, true) {
		return
	}
	t.holder.Store(int64(worker))
	t.target.Store(int64(fd))
	t.closeCh <- CloseEvent{Worker: worker, FD: fd}
	select {
	case <-t.releaseClose:
	case <-time.After(t.opts.HoldMax):
	}
}

// AfterAccept is called by onAcceptedFD after it set up the new connection
// and prepared its first recv, before the worker's next submit. Once the
// trial has a target it reports every accept; the first accept of the target
// number by a worker other than the one holding the close parks that worker
// until [Trial.ReleaseAccept] or HoldMax. Engine hook, worker thread.
func AfterAccept(worker, fd int) {
	t := current.Load()
	if t == nil {
		return
	}
	target := t.target.Load()
	if target < 0 {
		return
	}
	ev := AcceptEvent{Worker: worker, FD: fd}
	select {
	case t.acceptCh <- ev:
	default:
	}
	if int64(fd) != target || int64(worker) == t.holder.Load() || !t.acceptFired.CompareAndSwap(false, true) {
		return
	}
	t.acceptHeldCh <- ev
	select {
	case <-t.releaseAccept:
	case <-time.After(t.opts.HoldMax):
	}
}

// NoteStaleRecvData records a stale recv completion that carried data while
// a trial is armed. head is copied (at most 64 bytes). Engine hook, worker
// thread.
func NoteStaleRecvData(worker, fd int, gen uint32, res int32, head []byte) {
	t := current.Load()
	if t == nil {
		return
	}
	if len(head) > 64 {
		head = head[:64]
	}
	t.mu.Lock()
	t.stale = append(t.stale, StaleRecv{Worker: worker, FD: fd, Gen: gen, Res: res, Head: append([]byte(nil), head...)})
	t.mu.Unlock()
}

// wakeHoldNanos is the hypothesis (c) hold. See [SetWakeHold].
var (
	wakeHoldNanos atomic.Int64
	wakeHolds     atomic.Uint64
)

// SetWakeHold makes every io_uring worker sleep for d between releasing a
// promoted connection's asyncInMu (after appending received bytes to
// asyncInBuf) and waking or starting its dispatch goroutine; d <= 0 turns it
// off. It widens the hand-off window of hypothesis (c) of celeris#715.
func SetWakeHold(d time.Duration) {
	if d < 0 {
		d = 0
	}
	wakeHoldNanos.Store(int64(d))
}

// WakeHolds returns how many times [WakeHold] slept.
func WakeHolds() uint64 { return wakeHolds.Load() }

// WakeHold sleeps for the duration set by [SetWakeHold], if any. Engine hook,
// worker thread.
func WakeHold() {
	if d := wakeHoldNanos.Load(); d > 0 {
		wakeHolds.Add(1)
		time.Sleep(time.Duration(d))
	}
}

// hijackWithOpOwed is the hijack witness (celeris#685): hijacks that handed
// the socket over while the kernel still owed the connection an op on it (a
// multishot recv stays armed across its request; a single-shot recv has
// completed by the time its request's handler hijacks).
var hijackWithOpOwed atomic.Uint64

// NoteHijackWithOpOwed counts one such hijack. Engine hook.
func NoteHijackWithOpOwed() { hijackWithOpOwed.Add(1) }

// HijackWithOpOwed returns the hijack witness count since process start.
func HijackWithOpOwed() uint64 { return hijackWithOpOwed.Load() }

// hijackHoldNanos is the hijack hold. See [SetHijackHold].
var (
	hijackHoldNanos atomic.Int64
	hijackHolds     atomic.Uint64
)

// SetHijackHold makes hijackConn sleep for d on the worker thread just before
// it returns the hijacked connection, that is before the worker's next
// io_uring_enter; d <= 0 turns it off. It widens the window in which an op
// the kernel still owes the connection can read the hijacker's first bytes
// (celeris#685).
func SetHijackHold(d time.Duration) {
	if d < 0 {
		d = 0
	}
	hijackHoldNanos.Store(int64(d))
}

// HijackHolds returns how many times [HijackHold] slept.
func HijackHolds() uint64 { return hijackHolds.Load() }

// HijackHold sleeps for the duration set by [SetHijackHold], if any. Engine
// hook, worker thread.
func HijackHold() {
	if d := hijackHoldNanos.Load(); d > 0 {
		hijackHolds.Add(1)
		time.Sleep(time.Duration(d))
	}
}
