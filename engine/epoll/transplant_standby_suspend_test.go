//go:build linux

package epoll

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// celeris#624. A standby epoll loop parks indefinitely once it has no listen
// socket and no connections — it leaves epoll_wait entirely and waits on a Go
// channel only ResumeAccept closes. detachFromEpoll decrements connCount the
// instant a connection is detached FOR A TRANSPLANT, but on the async path the
// hand-off itself is finished later, by that same loop, in drainDetachQueue.
//
// So the LAST connection a standby loop drains used to park the loop on top of
// its own unfinished hand-off. The descriptor stayed open and owned by no
// engine, the target never heard of it, no OnDisconnect fired, and the live
// gauge stayed one short for the life of the process — one lost connection per
// loop that finishes its drain, which is exactly the residual measured in the
// field: detached - adopted == accepted - closed - active, with engine closes
// equal to hook closes because nothing closed.

// countingTarget stands in for the io_uring engine: it records the descriptors
// handed to it so the test needs no io_uring and no second engine, and it
// keeps them open so a lost hand-off cannot be mistaken for a closed conn.
type countingTarget struct {
	mu  sync.Mutex
	fds []int
}

func (c *countingTarget) AdoptConn(fd int, _ engine.Carryover) error {
	c.mu.Lock()
	c.fds = append(c.fds, fd)
	c.mu.Unlock()
	return nil
}

func (c *countingTarget) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.fds)
}

func (c *countingTarget) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, fd := range c.fds {
		_ = unix.Close(fd)
	}
	c.fds = nil
}

var _ engine.TransplantTarget = (*countingTarget)(nil)

// suspendTestHandler marks every route async so each conn promotes to a
// per-conn dispatch goroutine — the DEFERRED transplant path, the only one
// that leaves a hand-off owed across the suspend check.
type suspendTestHandler struct{}

func (suspendTestHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}},
		[]byte("ok"))
}
func (suspendTestHandler) RouteAsync(_, _ string) bool { return true }
func (suspendTestHandler) HasAsyncRoutes() bool        { return true }

var _ stream.AsyncRouteResolver = suspendTestHandler{}

func TestStandbyLoopCompletesItsLastHandoffInsteadOfSuspending(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	var disconnects atomic.Int64
	cfg := resource.Config{
		Addr:          addr,
		Protocol:      engine.HTTP1,
		Resources:     resource.Resources{Workers: 2},
		AsyncHandlers: true,
		OnDisconnect:  func(string) { disconnects.Add(1) },
	}
	e, err := New(cfg, suspendTestHandler{})
	if err != nil {
		t.Skipf("epoll engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	defer func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
		}
	}()
	for dl := time.Now().Add(10 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Skip("engine did not bind")
	}

	target := &countingTarget{}
	defer target.closeAll()

	const conns = 8
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var served atomic.Int64
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, derr := net.DialTimeout("tcp", addr, 3*time.Second)
			if derr != nil {
				return
			}
			defer func() { _ = c.Close() }()
			br := bufio.NewReader(c)
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = c.SetWriteDeadline(time.Now().Add(time.Second))
				if _, werr := c.Write([]byte("GET /x HTTP/1.1\r\nHost: x\r\n\r\n")); werr != nil {
					return
				}
				// After this conn is handed to the target nobody answers it,
				// so a read timeout is the expected end state, not a failure.
				_ = c.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
				resp, rerr := http.ReadResponse(br, nil)
				if rerr != nil {
					<-stop
					return
				}
				_, _ = resp.Body.Read(make([]byte, 8))
				_ = resp.Body.Close()
				served.Add(1)
			}
		}()
	}

	// Let every conn promote to its dispatch goroutine before the drain.
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); {
		if e.Metrics().AsyncPromotedConns >= conns {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := e.Metrics().AsyncPromotedConns; got < conns {
		close(stop)
		wg.Wait()
		t.Skipf("only %d/%d conns promoted to a dispatch goroutine; the deferred "+
			"transplant path is not exercised", got, conns)
	}

	// Become the standby: no listen socket, accept paused. This is what the
	// adaptive engine does to the demoted engine at a promotion, and it is
	// what arms the suspend park.
	if perr := e.PauseAccept(); perr != nil {
		t.Fatalf("PauseAccept: %v", perr)
	}
	e.StartTransplant(target)

	// Wait for the drain to detach every conn, then for the hand-offs to
	// land. The bug is entirely in the second half: the detach always
	// happens, it is the completion that the park swallows.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if e.Metrics().TransplantDetached >= conns {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	detached := e.Metrics().TransplantDetached
	if detached < conns {
		close(stop)
		wg.Wait()
		t.Skipf("drain moved only %d/%d conns before the deadline; nothing to assert", detached, conns)
	}
	for time.Now().Before(deadline) {
		if uint64(target.count()) >= detached {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	close(stop)
	wg.Wait()

	m := e.Metrics()
	adopted := target.count()
	t.Logf("served=%d detached=%d adopted-by-target=%d active=%d close=%d hookCloses=%d "+
		"refused=%d drainStopped=%d stranded=%d adoptRefused=%d",
		served.Load(), m.TransplantDetached, adopted, m.ActiveConnections,
		m.CloseCount, disconnects.Load(), m.TransplantHandoffRefused,
		m.TransplantDrainStopped, m.TransplantStranded, m.TransplantAdoptRefused)

	if uint64(adopted) != m.TransplantDetached {
		t.Errorf("target adopted %d of %d detached connections: %d hand-off(s) owed to a "+
			"standby loop were never completed.\n"+
			"connCount reaches 0 on the detach, so the loop's DRAINING→SUSPENDED gate "+
			"parks it on a Go channel before drainDetachQueue can finish the last "+
			"hand-off. The descriptor is left open and owned by no engine, with no "+
			"close and no OnDisconnect — celeris#624.",
			adopted, m.TransplantDetached, m.TransplantDetached-uint64(adopted))
	}
	// The conns moved; none of them ended. A close here would mean the fix
	// chose to kill the connection instead of completing the hand-off.
	if m.CloseCount != 0 || disconnects.Load() != 0 {
		t.Errorf("engine closes = %d, hook closes = %d, want 0 and 0 — a transplant is "+
			"a move, not a close", m.CloseCount, disconnects.Load())
	}
	if m.ActiveConnections != 0 {
		t.Errorf("ActiveConnections = %d, want 0 — every conn was detached", m.ActiveConnections)
	}
}
