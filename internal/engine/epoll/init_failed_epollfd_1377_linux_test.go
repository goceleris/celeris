//go:build linux

package epoll

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	celerisengine "github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#1377: a loop whose listen socket cannot be created closes the epoll
// descriptor it made, and must not leave l.epollFD naming the closed number: a
// driver that reaches the loop afterwards would epoll_ctl on whatever file has
// taken the number. Only this failure is tested here (the loop's bind finds the
// address taken). The other early return, the epoll_ctl of the listen socket,
// has no seam to make it fail.
func TestFailedListenSocketLeavesNoClosedEpollFD1377(t *testing.T) {
	// A plain listener on the address: the loop's bind (SO_REUSEPORT) is
	// refused with EADDRINUSE after its short bounded retry.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = held.Close() })

	e, err := New(resource.Config{
		Addr:      held.Addr().String(),
		Engine:    celerisengine.Epoll,
		Protocol:  celerisengine.HTTP1,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Resources: resource.Resources{Workers: 2},
	}, respondingHandler{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- e.Listen(context.Background()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "listen socket") {
			t.Fatalf("Listen = %v, want the listen socket error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Listen did not return with the address taken")
	}

	e.mu.Lock()
	loops := e.loops
	e.mu.Unlock()
	if len(loops) == 0 {
		t.Fatal("the engine has no loops")
	}
	for i, l := range loops {
		l.ctlMu.RLock()
		fd, closed := l.epollFD, l.ctlClosed
		l.ctlMu.RUnlock()
		if fd != -1 || !closed {
			t.Errorf("loop %d after the failed init: epollFD = %d, ctlClosed = %v, want -1 and true", i, fd, closed)
		}
	}

	// A driver that registers a conn on the loop is refused, not sent to the
	// epoll_ctl of a closed number.
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	defer func() { _ = unix.Close(pair[0]); _ = unix.Close(pair[1]) }()
	err = loops[0].RegisterConn(pair[0], func([]byte) {}, func(error) {})
	if err == nil {
		_ = loops[0].UnregisterConn(pair[0])
	}
	if !errors.Is(err, errLoopShutdown) {
		t.Errorf("RegisterConn on a loop whose init failed = %v, want errLoopShutdown", err)
	}
}
