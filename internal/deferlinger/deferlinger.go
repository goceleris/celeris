// Package deferlinger holds the state the epoll and io_uring engines share for
// pausing accept on a listen socket that has TCP_DEFER_ACCEPT set
// (celeris#662, celeris#675).
//
// # Why a pause cannot simply close the listener
//
// While TCP_DEFER_ACCEPT is set, the kernel keeps a connection whose handshake
// has completed but which has sent no data out of the accept queue: it stays a
// request socket, accept4 answers EAGAIN for it, and no drain can reach it. If
// the pause closes the listen socket then, the request socket is orphaned and
// the client's first request is answered with a reset. The engine sees
// nothing: no accept, no close, no error.
//
// The kernel does not hold such a connection forever. When the listener's
// SYN-ACK timer fires, about one second after the SYN, it retransmits the
// SYN-ACK, the client's reply creates the child, and the connection reaches
// the accept queue with no data. So a listener that stays open for long
// enough after the pause loses nothing that was deferred before it.
//
// # The linger
//
// At a pause, each loop or worker, on its own thread:
//
//  1. clears TCP_DEFER_ACCEPT on its own listener, so no connection that
//     arrives from then on is deferred;
//  2. takes its deadline, Linger after that setsockopt returned;
//  3. keeps accepting and serving through the normal path until the deadline;
//  4. runs the engine's ordinary drain-and-close.
//
// The deadline is anchored at the clear, not at the pause call, because the
// youngest connection that can still be deferred is the last one whose
// handshake completed before the clear. A resume that arrives during the
// linger puts the option back on the same descriptor; the listener is never
// closed.
//
// Linger is the kernel's initial retransmission timeout (one second), plus
// the timer wheel's rounding, plus margin. It is not public configuration:
// the variables below exist so tests can shorten it, disable the clear or the
// guard, and delay the clear, and nothing else writes them.
//
// # tcp_synack_retries = 0
//
// Once the option is cleared, the kernel expires a deferred request socket at
// its first timer when net.ipv4.tcp_synack_retries is 0, instead of
// retransmitting: every connection deferred before the clear would be lost.
// When that sysctl reads 0, the pausing listener also gets TCP_SYNCNT=1, which
// gives its request sockets one retransmission. The guard is applied only when
// the sysctl was read successfully and is 0: the kernel rejects TCP_SYNCNT
// values below 1, so a guard applied on a guess could not be undone and would
// permanently tighten a listener that the host had configured more leniently.
// TCP_SYNCNT stays set after a resume; one retransmission is more lenient than
// the host's zero.
//
// # Residuals
//
// A handshake still in flight when the listener finally closes, or one that
// completes between the drain's last EAGAIN and the close, is reset as it
// always was: that is inherent to closing a listen socket. A path whose
// retransmitted SYN-ACK is lost, or a client that does not answer it, is
// promoted later than the linger and can be lost too. A server that needs an
// instant pause that loses nothing sets resource.Config.DisableDeferAccept:
// with no option to clear, there is nothing to linger for, and the listener
// closes at once.
package deferlinger

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultLinger is how long a pausing listener stays open after it cleared
// TCP_DEFER_ACCEPT: the kernel's initial retransmission timeout of one
// second, the timer wheel's round-up, and margin.
const DefaultLinger = 1500 * time.Millisecond

// epoch anchors the package's clock. time.Now carries a monotonic clock
// reading, and time.Since between two readings that both carry one uses the
// monotonic clock alone (package time, "Monotonic Clocks").
var epoch = time.Now()

// Now is the clock every linger deadline is taken and read on: nanoseconds
// since the package was initialised, on the monotonic clock. A step of the
// wall clock (settimeofday, an NTP step) does not move it, so it can neither
// end a linger early -- which would bring back the resets celeris#662 fixed
// -- nor keep a paused listener open and accepting past its deadline, after
// PauseAccept, whose own bound is monotonic too, has returned.
func Now() int64 { return int64(time.Since(epoch)) }

// Deadline is a point on the Now clock. The zero Deadline means none: the
// listener is not lingering. It is a distinct type so that a deadline cannot
// be compared with a wall-clock reading such as time.Now().UnixNano()
// without a conversion that shows up in review.
type Deadline int64

// After returns the Deadline d from now on the Now clock.
func After(d time.Duration) Deadline { return Deadline(Now() + int64(d)) }

// Passed reports whether the Now clock has reached d.
func (d Deadline) Passed() bool { return Now() >= int64(d) }

// Left returns the time from now until d, which is negative once d has
// passed.
func (d Deadline) Left() time.Duration { return time.Duration(int64(d) - Now()) }

// The test hooks. Read on the loop and worker threads in the pause step only,
// never on the steady-state path. Tests that change them must not run in
// parallel with other engine tests.
var (
	lingerNs     atomic.Int64
	clearOnPause atomic.Bool
	guardEnabled atomic.Bool
	observeDelay atomic.Int64
)

func init() {
	lingerNs.Store(int64(DefaultLinger))
	clearOnPause.Store(true)
	guardEnabled.Store(true)
}

// Linger reports how long a pausing listener stays open after its clear. A
// value of zero or less means close at once, which is what an engine did
// before celeris#662.
func Linger() time.Duration { return time.Duration(lingerNs.Load()) }

// SetLinger replaces Linger and returns the previous value. Test hook.
func SetLinger(d time.Duration) (old time.Duration) {
	return time.Duration(lingerNs.Swap(int64(d)))
}

// ClearOnPause reports whether a pausing listener clears TCP_DEFER_ACCEPT.
// Always true outside tests.
func ClearOnPause() bool { return clearOnPause.Load() }

// SetClearOnPause replaces ClearOnPause and returns the previous value. Test
// hook: false keeps the option on through the linger.
func SetClearOnPause(v bool) (old bool) { return clearOnPause.Swap(v) }

// GuardEnabled reports whether the tcp_synack_retries=0 guard may be applied.
// Always true outside tests.
func GuardEnabled() bool { return guardEnabled.Load() }

// SetGuardEnabled replaces GuardEnabled and returns the previous value. Test
// hook.
func SetGuardEnabled(v bool) (old bool) { return guardEnabled.Swap(v) }

// ObserveDelay is how long after the pause call a loop waits before it acts
// on the pause. Zero outside tests. A test sets it to model a loop that
// observes the pause late, which is when anchoring the deadline at the pause
// call instead of at the clear would lose connections.
func ObserveDelay() time.Duration { return time.Duration(observeDelay.Load()) }

// SetObserveDelay replaces ObserveDelay and returns the previous value. Test
// hook.
func SetObserveDelay(d time.Duration) (old time.Duration) {
	return time.Duration(observeDelay.Swap(int64(d)))
}

// synackRetriesPath is a variable so the read path's unit test can point it
// at a file it controls.
var synackRetriesPath = "/proc/sys/net/ipv4/tcp_synack_retries"

// SynackRetriesZero reads net.ipv4.tcp_synack_retries. ok is false when the
// value could not be read or parsed, and zero is then false too.
func SynackRetriesZero() (zero, ok bool) {
	b, err := os.ReadFile(synackRetriesPath)
	if err != nil {
		return false, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return false, false
	}
	return v == 0, true
}

// GuardWanted decides the tcp_synack_retries=0 guard: it is wanted only when
// the sysctl was read successfully and is 0. An unreadable value never
// guards, because the guard cannot be undone (see the package comment).
// readable reports whether the read succeeded.
func GuardWanted() (guard, readable bool) {
	zero, ok := SynackRetriesZero()
	return ok && zero, ok
}

// Process-wide counters, for tests and diagnosis. None of them is exported
// through engine metrics.
var (
	lingers      atomic.Uint64
	closes       atomic.Uint64
	aborts       atomic.Uint64
	setFailures  atomic.Uint64
	guards       atomic.Uint64
	synackUnread atomic.Uint64
	waitWoken    atomic.Uint64
	waitRechecks atomic.Uint64
)

// Stats is a snapshot of the counters.
type Stats struct {
	// Lingers counts listeners that cleared the option and entered a linger.
	Lingers uint64
	// Closes counts listeners the pause path closed, after a linger or at
	// once.
	Closes uint64
	// Aborts counts lingers a resume ended before the deadline.
	Aborts uint64
	// SetFailures counts failed setsockopt calls on the pause path: a clear,
	// a restore or a guard.
	SetFailures uint64
	// Guards counts listeners that got TCP_SYNCNT=1.
	Guards uint64
	// SynackUnread counts pauses that could not read tcp_synack_retries, and
	// so applied no guard.
	SynackUnread uint64
	// WaitWoken counts the times a PauseAccept wait was woken by Notify.
	WaitWoken uint64
	// WaitRechecks counts the times a PauseAccept wait re-checked on its
	// timer instead (WaitRecheck).
	WaitRechecks uint64
}

// Snapshot returns the counters.
func Snapshot() Stats {
	return Stats{
		Lingers:      lingers.Load(),
		Closes:       closes.Load(),
		Aborts:       aborts.Load(),
		SetFailures:  setFailures.Load(),
		Guards:       guards.Load(),
		SynackUnread: synackUnread.Load(),
		WaitWoken:    waitWoken.Load(),
		WaitRechecks: waitRechecks.Load(),
	}
}

// NoteClose records that the pause path closed a listener.
func NoteClose() { closes.Add(1) }

// PauseState is one engine's record of the pause in progress, and the place
// its PauseAccept waits. Begin writes it off the loop threads before the
// pause flag is set; the loops read it in their pause step. A nil
// *PauseState is valid: it never guards and never delays, and Notify on it
// does nothing.
type PauseState struct {
	began atomic.Int64
	guard atomic.Bool

	// Each warning is logged once per engine; later occurrences are only
	// counted (SynackUnread, SetFailures).
	warnedUnread  atomic.Bool
	warnedSet     atomic.Bool
	warnedRestore atomic.Bool

	// waitMu guards waitCh, the channel the next Notify closes. It is a
	// leaf lock: nothing else is acquired while it is held, and nothing
	// done under it blocks.
	waitMu sync.Mutex
	waitCh chan struct{}
}

// Begin records a new pause: when it began, and whether its listeners get the
// tcp_synack_retries=0 guard. It reads the sysctl once, here, so no loop
// thread touches /proc. Call it before setting the engine's pause flag.
func (p *PauseState) Begin(logger *slog.Logger, engineName string) {
	guard, readable := GuardWanted()
	if !readable {
		synackUnread.Add(1)
		if logger != nil && p.warnedUnread.CompareAndSwap(false, true) {
			logger.Warn("accept pause: could not read net.ipv4.tcp_synack_retries; "+
				"the pausing listeners get no TCP_SYNCNT guard (logged once per engine)",
				"engine", engineName, "path", synackRetriesPath)
		}
	}
	p.guard.Store(guard)
	p.began.Store(Now())
}

// Began reports when the current pause began, on the Now clock.
func (p *PauseState) Began() int64 {
	if p == nil {
		return 0
	}
	return p.began.Load()
}

// Changed returns the channel the next Notify closes. A PauseAccept that is
// about to wait takes it BEFORE it reads the state it waits on, so a change
// published after that read closes a channel it already holds: no wakeup is
// lost.
func (p *PauseState) Changed() <-chan struct{} {
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	if p.waitCh == nil {
		p.waitCh = make(chan struct{})
	}
	return p.waitCh
}

// Notify wakes every PauseAccept waiting on this engine. A loop or worker
// calls it after it has published a change such a wait reads -- its listener
// closed by the pause, its exit -- and ResumeAccept after it withdrew the
// pause.
func (p *PauseState) Notify() {
	if p == nil {
		return
	}
	p.waitMu.Lock()
	if p.waitCh != nil {
		close(p.waitCh)
		p.waitCh = nil
	}
	p.waitMu.Unlock()
}

// WaitRecheck is the longest a PauseAccept wait sleeps without a Notify. The
// pause close, a loop's exit and a resume all notify; the timer is for the
// one change that does not: a pause that lands on a listener a resume has not
// re-created yet, whose flag the loop sets on its next iteration.
const WaitRecheck = 100 * time.Millisecond

// WaitChanged blocks until changed is closed, deadline (a time.Now reading,
// monotonic) passes, or WaitRecheck elapses, whichever is first. It returns
// false only when deadline has passed.
func WaitChanged(changed <-chan struct{}, deadline time.Time) bool {
	left := time.Until(deadline)
	if left <= 0 {
		return false
	}
	t := time.NewTimer(min(left, WaitRecheck))
	defer t.Stop()
	select {
	case <-changed:
		waitWoken.Add(1)
		return true
	case <-t.C:
		waitRechecks.Add(1)
		return time.Now().Before(deadline)
	}
}

// Guard reports whether the current pause's listeners get TCP_SYNCNT=1.
func (p *PauseState) Guard() bool {
	return p != nil && p.guard.Load() && GuardEnabled()
}

// Observed reports whether a loop may act on the current pause now. It is
// always true unless a test set ObserveDelay.
func (p *PauseState) Observed() bool {
	d := observeDelay.Load()
	if d <= 0 || p == nil {
		return true
	}
	return Now() >= p.began.Load()+d
}
