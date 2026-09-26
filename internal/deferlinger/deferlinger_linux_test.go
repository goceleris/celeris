//go:build linux

package deferlinger

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// withSynackFile points the sysctl read at a file holding content, or at a
// path that does not exist when content is nil.
func withSynackFile(t *testing.T, content []byte) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tcp_synack_retries")
	if content != nil {
		if err := os.WriteFile(p, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := synackRetriesPath
	synackRetriesPath = p
	t.Cleanup(func() { synackRetriesPath = old })
}

// TestGuardWantedOnlyOnReadableZero pins the read path of the
// tcp_synack_retries=0 guard. The guard is TCP_SYNCNT=1, which the kernel
// cannot take back (it rejects values below 1), so applying it on a value it
// could not read would permanently tighten a resumed listener that the host
// had configured with more retries. Only a successful read of 0 guards.
func TestGuardWantedOnlyOnReadableZero(t *testing.T) {
	for _, tc := range []struct {
		name         string
		content      []byte
		wantGuard    bool
		wantReadable bool
	}{
		{"missing file", nil, false, false},
		{"zero", []byte("0\n"), true, true},
		{"zero, no newline", []byte("0"), true, true},
		{"the kernel default", []byte("5\n"), false, true},
		{"one", []byte("1\n"), false, true},
		{"garbage", []byte("x\n"), false, false},
		{"empty", []byte(""), false, false},
	} {
		withSynackFile(t, tc.content)
		guard, readable := GuardWanted()
		if guard != tc.wantGuard || readable != tc.wantReadable {
			t.Errorf("%s: GuardWanted() = (guard %v, readable %v), want (%v, %v)",
				tc.name, guard, readable, tc.wantGuard, tc.wantReadable)
		}
	}
}

// TestBeginRecordsTheGuardDecision drives the same decision through
// PauseState.Begin, which is what the engines call, and checks that an
// unreadable sysctl is counted and never guards.
func TestBeginRecordsTheGuardDecision(t *testing.T) {
	var p PauseState

	withSynackFile(t, nil)
	before := Snapshot().SynackUnread
	p.Begin(nil, "test")
	if p.Guard() {
		t.Error("an unreadable tcp_synack_retries guarded the pause")
	}
	if got := Snapshot().SynackUnread - before; got != 1 {
		t.Errorf("SynackUnread moved by %d, want 1", got)
	}

	withSynackFile(t, []byte("0\n"))
	p.Begin(nil, "test")
	if !p.Guard() {
		t.Error("tcp_synack_retries=0 did not guard the pause")
	}
	old := SetGuardEnabled(false)
	if p.Guard() {
		t.Error("GuardEnabled=false still guarded")
	}
	SetGuardEnabled(old)

	withSynackFile(t, []byte("5\n"))
	p.Begin(nil, "test")
	if p.Guard() {
		t.Error("tcp_synack_retries=5 guarded the pause")
	}

	var nilState *PauseState
	if nilState.Guard() || !nilState.Observed() || nilState.Began() != 0 {
		t.Error("a nil PauseState must never guard, never delay, and report no start")
	}
}

func listener(t *testing.T, deferAccept bool) int {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if deferAccept {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_DEFER_ACCEPT, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(fd, 16); err != nil {
		t.Fatal(err)
	}
	return fd
}

func deferOn(t *testing.T, fd int) bool {
	t.Helper()
	v, err := unix.GetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_DEFER_ACCEPT)
	if err != nil {
		t.Fatal(err)
	}
	return v != 0
}

// TestEnterClearsThenLeaveRestores checks the socket-level effect of the two
// transitions, and the guard, on a real listener.
func TestEnterClearsThenLeaveRestores(t *testing.T) {
	withSynackFile(t, []byte("0\n"))
	fd := listener(t, true)
	if !deferOn(t, fd) {
		t.Fatal("premise: the listener does not read TCP_DEFER_ACCEPT on")
	}
	var p PauseState
	p.Begin(nil, "test")
	s0 := Snapshot()
	until := Enter(fd, true, &p, nil, "loop", 0)
	if until == 0 {
		t.Fatal("Enter closed at once on a listener with the option")
	}
	if deferOn(t, fd) {
		t.Error("Enter left TCP_DEFER_ACCEPT on")
	}
	if v, err := unix.GetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_SYNCNT); err != nil || v != 1 {
		t.Errorf("TCP_SYNCNT = %d (%v) at tcp_synack_retries=0, want 1", v, err)
	}
	s1 := Snapshot()
	if s1.Lingers-s0.Lingers != 1 || s1.Guards-s0.Guards != 1 || s1.SetFailures != s0.SetFailures {
		t.Errorf("counters moved lingers +%d guards +%d setFailures +%d, want +1 +1 +0",
			s1.Lingers-s0.Lingers, s1.Guards-s0.Guards, s1.SetFailures-s0.SetFailures)
	}
	Leave(fd, true, nil, "loop", 0)
	if !deferOn(t, fd) {
		t.Error("Leave did not restore TCP_DEFER_ACCEPT")
	}
	if got := Snapshot().Aborts - s1.Aborts; got != 1 {
		t.Errorf("Aborts moved by %d, want 1", got)
	}
}

// TestEnterDeadlineAnchoredAtTheClear pins where the deadline is taken. A loop
// can observe the pause late -- an io_uring worker sees it at its next ring
// wait -- and the youngest connection that can still be deferred is the last
// one to complete before the clear, so a deadline taken at the pause call
// would close the listener before the kernel promotes it.
func TestEnterDeadlineAnchoredAtTheClear(t *testing.T) {
	withSynackFile(t, []byte("5\n"))
	fd := listener(t, true)
	var p PauseState
	p.Begin(nil, "test")
	time.Sleep(50 * time.Millisecond) // the loop observes the pause late
	before := time.Now().UnixNano()
	until := Enter(fd, true, &p, nil, "loop", 0)
	if floor := before + int64(Linger()); until < floor {
		t.Errorf("deadline is %v before clear+Linger: it was not taken after the clear",
			time.Duration(floor-until))
	}
	if until > time.Now().UnixNano()+int64(Linger()) {
		t.Error("deadline lies beyond now+Linger")
	}
}

// TestEnterClosesAtOnce covers the two cases that must not linger: a listener
// created without the option (DisableDeferAccept, the instant lossless opt-out)
// and a zero Linger.
func TestEnterClosesAtOnce(t *testing.T) {
	withSynackFile(t, []byte("5\n"))
	var p PauseState
	p.Begin(nil, "test")

	fd := listener(t, false)
	if until := Enter(fd, false, &p, nil, "loop", 0); until != 0 {
		t.Error("a listener without TCP_DEFER_ACCEPT lingered")
	}

	fd = listener(t, true)
	old := SetLinger(0)
	defer SetLinger(old)
	if until := Enter(fd, true, &p, nil, "loop", 0); until != 0 {
		t.Error("Linger 0 lingered")
	}
	if !deferOn(t, fd) {
		t.Error("a listener that closes at once had its option cleared for nothing")
	}
}

// TestObserveDelayHook checks the test hook the engine tests use to model a
// late observation.
func TestObserveDelayHook(t *testing.T) {
	var p PauseState
	withSynackFile(t, []byte("5\n"))
	p.Begin(nil, "test")
	if !p.Observed() {
		t.Fatal("with no ObserveDelay a pause must be observable at once")
	}
	old := SetObserveDelay(time.Hour)
	defer SetObserveDelay(old)
	if p.Observed() {
		t.Error("ObserveDelay did not delay the observation")
	}
}
