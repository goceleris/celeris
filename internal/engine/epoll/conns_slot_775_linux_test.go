//go:build linux

package epoll

// celeris#775: l.conns[fd] is cleared under driverMu everywhere the loop's
// own goroutine is not the only reader. RegisterConn reads the slot, under
// driverMu, from the driver's goroutine (its "already an HTTP connection"
// check), and closeConn and shutdown cleared it without the lock: a data
// race with a defined fix, one lock around each slot write.
//
// The tests find the descriptor numbers the engine accepted its HTTP
// connections on, and call RegisterConn on exactly those numbers, so the
// read and the loop's write are of the same slot and the race detector sees
// the pair. TestCloseConnClearsSlotUnderDriverMu775 does not need -race: it
// holds the read lock and checks that closeConn waits for it.

import (
	"net"
	"os"
	"runtime/pprof"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// serverSideFDs returns the descriptors of this process that are the server
// side of a TCP connection accepted on addr: their local address is addr and
// they have a peer. The client sides have addr as their peer, and the
// listening sockets have no peer.
func serverSideFDs(t *testing.T, addr *net.TCPAddr) []int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	var fds []int
	for _, e := range ents {
		fd, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		local, err := unix.Getsockname(fd)
		if err != nil {
			continue
		}
		in4, ok := local.(*unix.SockaddrInet4)
		if !ok || in4.Port != addr.Port || !net.IP(in4.Addr[:]).Equal(addr.IP) {
			continue
		}
		if _, err := unix.Getpeername(fd); err != nil {
			continue
		}
		fds = append(fds, fd)
	}
	return fds
}

// watchdog dumps every goroutine's stack and aborts the process when the test
// has not finished within d: a deadlock between driverMu and the locks the
// loop takes under it must fail loudly, not hang the suite (RULE 10).
func watchdog(t *testing.T, d time.Duration) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-time.After(d):
			_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
			panic("watchdog: " + t.Name() + " did not finish: a lock cycle or a stuck worker")
		}
	}()
	return func() { close(done) }
}

// acceptedConns dials n connections to eng and waits until the engine has
// accepted them all. It returns the client ends and the server-side
// descriptor numbers.
func acceptedConns(t *testing.T, eng *Engine, n int) ([]net.Conn, []int) {
	t.Helper()
	tcp := eng.Addr().(*net.TCPAddr)
	var clients []net.Conn
	for range n {
		c, err := net.Dial("tcp", tcp.String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		clients = append(clients, c)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if fds := serverSideFDs(t, tcp); len(fds) == n && eng.loops[0].activeConns.Load() == int64(n) {
			return clients, fds
		}
		if time.Now().After(deadline) {
			t.Fatalf("the engine accepted %d of %d connections", eng.loops[0].activeConns.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// spinRegister calls RegisterConn on every number in fds, on every worker,
// until stop is closed. A number that is an HTTP connection's is refused
// after RegisterConn has read its slot; one the engine has since freed fails
// the epoll_ctl, or, if another descriptor of the process took it, is
// registered and unregistered at once.
func spinRegister(eng *Engine, fds []int, stop <-chan struct{}, regs *atomic.Int64) (wait func()) {
	var wg sync.WaitGroup
	for w := 0; w < eng.NumWorkers(); w++ {
		wl := eng.WorkerLoop(w)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				for _, fd := range fds {
					select {
					case <-stop:
						return
					default:
					}
					regs.Add(1)
					if wl.RegisterConn(fd, func([]byte) {}, func(error) {}) == nil {
						_ = wl.UnregisterConn(fd)
					}
				}
			}
		}()
	}
	return wg.Wait
}

// TestRegisterConnRacesCloseConn775: driver registrations on the numbers of
// HTTP connections the loop is closing. closeConn's slot write must not race
// RegisterConn's slot read. Reported by the race detector on the unlocked
// write (go test -race); CI runs this package under it.
func TestRegisterConnRacesCloseConn775(t *testing.T) {
	defer watchdog(t, 2*time.Minute)()
	eng, stopEng := newTestEngine(t)
	t.Cleanup(stopEng)

	var regs atomic.Int64
	for round := 0; round < 8; round++ {
		clients, fds := acceptedConns(t, eng, 16)
		stop := make(chan struct{})
		wait := spinRegister(eng, fds, stop, &regs)
		time.Sleep(time.Millisecond)
		closed0 := eng.loops[0].closeCount.Load()
		for _, c := range clients {
			_ = c.Close()
		}
		deadline := time.Now().Add(5 * time.Second)
		for eng.loops[0].closeCount.Load() < closed0+uint64(len(clients)) {
			if time.Now().After(deadline) {
				t.Fatalf("round %d: the engine closed %d of %d connections", round, eng.loops[0].closeCount.Load()-closed0, len(clients))
			}
			time.Sleep(time.Millisecond)
		}
		close(stop)
		wait()
	}
	t.Logf("775 closeConn: %d RegisterConn calls on accepted numbers", regs.Load())
	if regs.Load() == 0 {
		t.Fatal("no RegisterConn call ran: the test did not exercise the slot read")
	}
}

// TestShutdownRacesRegisterConn775: the same for shutdown's slot write
// (phase 3, every live conn). The engine is stopped while a driver goroutine
// keeps registering on the numbers of its open connections.
func TestShutdownRacesRegisterConn775(t *testing.T) {
	defer watchdog(t, 2*time.Minute)()
	var regs atomic.Int64
	for round := 0; round < 5; round++ {
		eng, stopEng := newTestEngine(t)
		clients, fds := acceptedConns(t, eng, 16)
		stop := make(chan struct{})
		wait := spinRegister(eng, fds, stop, &regs)
		time.Sleep(time.Millisecond)
		stopEng() // cancels Listen and waits for every loop to shut down
		close(stop)
		wait()
		for _, c := range clients {
			_ = c.Close()
		}
	}
	t.Logf("775 shutdown: %d RegisterConn calls on accepted numbers", regs.Load())
	if regs.Load() == 0 {
		t.Fatal("no RegisterConn call ran: the test did not exercise the slot read")
	}
}

// TestCloseConnClearsSlotUnderDriverMu775 is the lock itself, deterministic and
// independent of -race: with driverMu held for reading, closeConn must not
// finish, because it takes the write lock to clear the slot. On the unlocked
// write it finishes (and counts the close) while the read lock is held.
func TestCloseConnClearsSlotUnderDriverMu775(t *testing.T) {
	defer watchdog(t, time.Minute)()
	eng, stopEng := newTestEngine(t)
	t.Cleanup(stopEng)
	clients, _ := acceptedConns(t, eng, 1)

	for _, l := range eng.loops {
		l.driverMu.RLock()
	}
	released := false
	release := func() {
		if !released {
			released = true
			for _, l := range eng.loops {
				l.driverMu.RUnlock()
			}
		}
	}
	defer release()

	before := eng.loops[0].closeCount.Load()
	_ = clients[0].Close()
	time.Sleep(300 * time.Millisecond)
	if got := eng.loops[0].closeCount.Load(); got != before {
		t.Errorf("closeConn finished while driverMu was held for reading (%d closes counted): it cleared l.conns[fd] without the write lock", got-before)
	}
	release()
	deadline := time.Now().Add(5 * time.Second)
	for eng.loops[0].closeCount.Load() == before {
		if time.Now().After(deadline) {
			t.Fatal("closeConn did not finish after driverMu was released")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestDriverTakesNumberFreedByCloseConn775: the slot is cleared before the
// descriptor is closed, so the moment the number is free a driver can register
// on it. The hook runs on the loop's goroutine right after close(2), where a
// slot cleared after the close would still be set: RegisterConn must succeed
// on the freed number, not be refused as "already an HTTP connection" for a
// connection that is gone.
//
// Another descriptor of the process can take the freed number between close(2)
// and the hook's F_GETFD check (a few microseconds, and nothing else in this
// test opens descriptors, but the test binary is one process). Then the test
// cannot reach RegisterConn on that number, which says nothing about the code:
// it starts over, on a new engine and a new connection, up to
// freedNumberAttempts775 times, and fails only if every attempt lost the number
// (celeris#885 item 25).
func TestDriverTakesNumberFreedByCloseConn775(t *testing.T) {
	defer watchdog(t, time.Minute)()
	runFreedNumber775(t, -1)
}

// TestDriverTakesNumberFreedByCloseConn775RetriesACollision is the control of
// the retry: a decoy socket takes the freed number between close(2) and the
// check, on the first attempt, as another descriptor of the process would. That
// attempt must report a collision (the decoy is verified to hold the number; if
// another descriptor got it before the decoy did, that is one more collision
// and the decoy moves to the next attempt), the test must not fail on it, and a
// later attempt must reach RegisterConn and check the slot like any other. With
// one attempt allowed (the test as it was: t.Fatalf on the first collision)
// this fails.
func TestDriverTakesNumberFreedByCloseConn775RetriesACollision(t *testing.T) {
	defer watchdog(t, time.Minute)()
	// At least 2: a natural collision on the second attempt too is retried as
	// well, so "exactly 2" would bring back the flake this test is the control of.
	if used := runFreedNumber775(t, 0); used < 2 {
		t.Errorf("the check passed after %d attempt(s), want at least 2: the attempt with the decoy on the freed number must have been retried", used)
	}
}

// freedNumberAttempts775 is how many times a collision on the freed number is
// retried (see TestDriverTakesNumberFreedByCloseConn775).
const freedNumberAttempts775 = 5

// freedNumber775 is what one attempt saw.
type freedNumber775 struct {
	fd               int
	regErr, unregErr error
	slotBusy         bool
	reused           bool // the number was free after close(2) and a new descriptor took it
	decoyHeld        bool // collideOn: the decoy holds the freed number
}

// runFreedNumber775 makes the attempts, with a decoy on the freed number of
// attempt collideAt (-1: none; if another descriptor takes the number before
// the decoy does, that is one more retried collision and the decoy moves to the
// next attempt), until one reuses the number, checks its result and returns the
// number of attempts it took. If none does, it fails.
func runFreedNumber775(t *testing.T, collideAt int) int {
	t.Helper()
	decoyAt := collideAt
	for attempt := range freedNumberAttempts775 {
		r := freedNumberAttempt775(t, attempt == decoyAt)
		if attempt == decoyAt && !r.decoyHeld {
			// Another descriptor of the process took the number before the decoy
			// did: a natural collision, retried like any other, and the decoy
			// moves to the next attempt so the control still injects its own.
			t.Logf("attempt %d of %d: the decoy did not get the number %d that closeConn freed (another descriptor took it first); the decoy moves to the next attempt", attempt+1, freedNumberAttempts775, r.fd)
			decoyAt++
			continue
		}
		if !r.reused {
			t.Logf("attempt %d of %d: another descriptor of the process took the number %d that closeConn freed, before the check; starting over", attempt+1, freedNumberAttempts775, r.fd)
			continue
		}
		if r.slotBusy {
			t.Errorf("l.conns[%d] was still set after closeConn closed the descriptor: the slot is cleared after the close, not before", r.fd)
		}
		if r.regErr != nil {
			t.Errorf("RegisterConn on the number closeConn had just freed: %v", r.regErr)
		}
		if r.unregErr != nil {
			t.Errorf("UnregisterConn: %v", r.unregErr)
		}
		return attempt + 1
	}
	t.Fatalf("the freed number could not be taken by a new descriptor in %d attempts: the test did not reach RegisterConn", freedNumberAttempts775)
	return 0
}

// freedNumberAttempt775 accepts one connection on an engine of its own, closes
// the client, and reports what the hook saw when closeConn had closed the
// server side. An engine per attempt: the hook is a package var, set before the
// engine is created and cleared after it is stopped, so no loop goroutine reads
// it while it is written. With decoy, a socket takes the freed number first
// (pinned with dup3 if another descriptor came before it and the number is
// still free), which is what a collision looks like to the check.
func freedNumberAttempt775(t *testing.T, decoy bool) freedNumber775 {
	t.Helper()
	var target atomic.Int64
	target.Store(-1)
	got := make(chan freedNumber775, 1)
	testHookConnClosed = func(l *Loop, fd int) {
		if int64(fd) != target.Load() {
			return
		}
		r := freedNumber775{fd: fd}
		if decoy {
			if d, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0); err == nil {
				if d != fd {
					if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == unix.EBADF {
						if unix.Dup3(d, fd, unix.O_CLOEXEC) == nil {
							_ = unix.Close(d)
							d = fd
						}
					}
				}
				r.decoyHeld = d == fd
				defer func() { _ = unix.Close(d) }()
			}
		}
		// close(2) has just returned: the number is the lowest free one, so a
		// new socket normally gets it. Pin it with dup3 if another descriptor
		// of the process came first (only when the number is verified free).
		nfd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			r.regErr = err
			got <- r
			return
		}
		if nfd != fd {
			if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == unix.EBADF {
				if err := unix.Dup3(nfd, fd, unix.O_CLOEXEC); err == nil {
					r.reused = true
				}
			}
			_ = unix.Close(nfd)
		} else {
			r.reused = true
		}
		if !r.reused {
			got <- r
			return
		}
		l.driverMu.RLock()
		r.slotBusy = fd < len(l.conns) && l.conns[fd] != nil
		l.driverMu.RUnlock()
		r.regErr = l.RegisterConn(fd, func([]byte) {}, func(error) {})
		if r.regErr == nil {
			r.unregErr = l.UnregisterConn(fd)
		}
		_ = unix.Close(fd)
		got <- r
	}
	defer func() { testHookConnClosed = nil }()

	eng, stopEng := newTestEngine(t)
	defer stopEng()
	clients, fds := acceptedConns(t, eng, 1)
	target.Store(int64(fds[0]))
	_ = clients[0].Close()

	select {
	case r := <-got:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("closeConn did not close the connection")
	}
	return freedNumber775{}
}
