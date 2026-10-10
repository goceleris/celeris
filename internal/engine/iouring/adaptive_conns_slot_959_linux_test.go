//go:build linux

package iouring_test

// celeris#959 through the adaptive engine started on io_uring: the driver
// wrapper (internal/adaptive/provider.go) forwards RegisterConn to the io_uring
// worker it is pinned to, so a driver that registers the number of an HTTP
// connection the worker is accepting or closing reaches the same slot read
// the iouring package's TestRegisterConnRacesAcceptAndClose959 drives
// directly. Reported by the race detector (go test -race).

import (
	"context"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/adaptive"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/probe"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

type noopHandler959 struct{}

func (noopHandler959) HandleStream(context.Context, *stream.Stream) error { return nil }

// serverFDs959 lists this process's server-side descriptors of connections
// accepted on addr (local address addr, with a peer).
func serverFDs959(t *testing.T, addr *net.TCPAddr) []int {
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

func waitFDs959(t *testing.T, addr *net.TCPAddr, n int) []int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		fds := serverFDs959(t, addr)
		if len(fds) == n {
			return fds
		}
		if time.Now().After(deadline) {
			t.Fatalf("apparatus: %d server-side descriptors after 5 s, want %d", len(fds), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAdaptiveRegisterConnRacesAcceptAndClose959(t *testing.T) {
	if !probe.Probe().IOUringTier.Available() {
		t.Skip("io_uring unavailable")
	}
	t.Setenv("CELERIS_ADAPTIVE_START", "iouring")
	cfg := resource.Config{
		Addr:      "127.0.0.1:0",
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
	}
	e, err := adaptive.New(cfg, noopHandler959{}, nil)
	if err != nil {
		t.Skipf("adaptive.New unsupported here: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("adaptive engine did not stop within 15s")
		}
	}()
	for deadline := time.Now().Add(15 * time.Second); e.Addr() == nil || e.NumWorkers() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("adaptive engine never bound")
		}
	}
	if got := e.ActiveEngine().Type(); got != engine.IOUring {
		t.Fatalf("apparatus: the adaptive engine started on %s, want io_uring (CELERIS_ADAPTIVE_START=iouring)", got)
	}
	tcp := e.Addr().(*net.TCPAddr)

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
		fds := waitFDs959(t, tcp, perRound)
		stop := make(chan struct{})
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
		time.Sleep(time.Millisecond)
		for _, c := range clients {
			_ = c.Close()
		}
		waitFDs959(t, tcp, 0)
		close(stop)
		wg.Wait()
	}
	if regs.Load() == 0 {
		t.Fatal("apparatus: no RegisterConn call ran, so the slot read was never exercised")
	}
	t.Logf("celeris959 ADAPTIVE rounds=%d RegisterConn calls=%d active=%s", rounds, regs.Load(), e.ActiveEngine().Type())
}
