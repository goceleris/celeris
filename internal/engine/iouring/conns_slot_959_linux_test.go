//go:build linux

package iouring

// celeris#959: Worker.conns has no lock. RegisterConn reads w.conns[fd] from
// the driver's goroutine (its "already an HTTP connection" check, twice) while
// the worker goroutine writes the slots bare: the accept install, finishClose,
// handleClose, the hijack, the transplant paths. A driver that registers the
// number of an HTTP connection the worker is accepting or closing reads a slot
// the worker writes, a data race under -race, and the io_uring twin of the
// epoll defect of celeris#775 (there the accept install already took
// driverMu, here nothing is locked at all).
//
// The test finds the descriptor numbers the engine accepted its HTTP
// connections on, and calls RegisterConn on exactly those numbers while the
// worker holds, then closes, those connections, so the read and the worker's
// write (the accept's, made earlier with nothing ordering it before the
// read, and the close's) are of the same slot and the race detector sees the
// pair. The spinner starts only once the connections are accepted: a
// RegisterConn that wins a number before the worker has installed its slot
// would arm a driver recv on a live HTTP socket, and its cancel (by the
// duplicate, which names the same file) would end the HTTP conn's recv. They fail by the race detector (go test -race, which
// CI runs this package under) and, for the apparatus, by t.Fatal.

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

// serverSideFDs959 returns the descriptors of this process that are the server
// side of a TCP connection accepted on addr: their local address is addr and
// they have a peer (the client sides have addr as their peer, and the
// listening sockets have no peer).
func serverSideFDs959(t *testing.T, addr *net.TCPAddr) []int {
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

// waitServerFDs959 waits until exactly n server-side descriptors exist and
// returns them. Anything else for 5 s is an apparatus failure: a test that
// spins on too few numbers, or on none, would pass for the wrong reason.
func waitServerFDs959(t *testing.T, addr *net.TCPAddr, n int) []int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		fds := serverSideFDs959(t, addr)
		if len(fds) == n {
			return fds
		}
		if time.Now().After(deadline) {
			t.Fatalf("apparatus: %d server-side descriptors after 5 s, want %d (fixed files on, or the engine did not accept/close them?)", len(fds), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// watchdog959 dumps every goroutine's stack and aborts the process when the
// test has not finished within d: a deadlock between the slot lock and the
// locks around it must fail loudly, not hang the suite (RULE 10).
func watchdog959(t *testing.T, d time.Duration) (stop func()) {
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

// spinRegister959 calls RegisterConn on every number in fds, on every worker,
// until stop is closed. A number that is an HTTP connection's is refused after
// RegisterConn has read its slot; one the engine has since freed fails the
// duplicate (EBADF) or, if another descriptor of the process took it, is
// registered and unregistered at once.
func spinRegister959(e *Engine, fds []int, stop <-chan struct{}, regs *atomic.Int64) (wait func()) {
	var wg sync.WaitGroup
	for i := 0; i < e.NumWorkers(); i++ {
		wl := e.WorkerLoop(i)
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

// TestRegisterConnRacesAcceptAndClose959: driver registrations on the numbers
// of HTTP connections the worker has accepted and is closing. The accept
// install's slot write and finishClose's must not race RegisterConn's slot
// reads. Reported by the race detector (go test -race). The engine takes up to
// a second to notice an accept or a close on an idle ring, so a round lasts
// seconds.
func TestRegisterConnRacesAcceptAndClose959(t *testing.T) {
	defer watchdog959(t, 2*time.Minute)()
	eng, stopEng := startTestEngine(t)
	t.Cleanup(stopEng)
	tcp := eng.Addr().(*net.TCPAddr)

	var regs atomic.Int64
	const rounds, perRound = 3, 16
	for round := 0; round < rounds; round++ {
		var clients []net.Conn
		for range perRound {
			c, err := net.Dial("tcp", tcp.String())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			clients = append(clients, c)
		}
		fds := waitServerFDs959(t, tcp, perRound)
		stop := make(chan struct{})
		wait := spinRegister959(eng, fds, stop, &regs)
		time.Sleep(time.Millisecond)
		for _, c := range clients {
			_ = c.Close()
		}
		waitServerFDs959(t, tcp, 0)
		close(stop)
		wait()
	}
	if regs.Load() == 0 {
		t.Fatal("apparatus: no RegisterConn call ran, so the slot read was never exercised")
	}
	t.Logf("celeris959 RACE rounds=%d RegisterConn calls=%d", rounds, regs.Load())
}
