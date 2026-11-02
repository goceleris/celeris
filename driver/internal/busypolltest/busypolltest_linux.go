//go:build linux

package busypolltest

import (
	"context"
	"net"
	"testing"
	"time"

	celeris "github.com/goceleris/celeris"
	"golang.org/x/sys/unix"
)

// StartServer runs a celeris server on the epoll engine, on a loopback
// listener and with no routes, whose Config.SocketBusyPoll is busyPoll, and
// returns it once it is listening. Its event loop is the one a driver opened
// WithEngine(srv) registers its descriptors on. The server is shut down when
// the test ends. A host that cannot run the epoll engine skips the test.
func StartServer(t testing.TB, busyPoll time.Duration) *celeris.Server {
	t.Helper()
	srv := celeris.New(celeris.Config{Addr: "127.0.0.1:0", Engine: celeris.Epoll, SocketBusyPoll: busyPoll})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.StartWithListener(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for srv.Addr() == nil && time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Skipf("the epoll engine cannot run on this host: %v", err)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if srv.Addr() == nil {
		t.Fatal("the server never bound within 10 s")
	}
	if srv.EventLoopProvider() == nil {
		t.Skip("the epoll server has no EventLoopProvider")
	}
	return srv
}

// BusyPoll is SO_BUSY_POLL, in microseconds, of fd.
func BusyPoll(t testing.TB, fd int) int {
	t.Helper()
	v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_BUSY_POLL)
	if err != nil {
		t.Fatalf("getsockopt SO_BUSY_POLL on fd %d: %v", fd, err)
	}
	return v
}
