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
