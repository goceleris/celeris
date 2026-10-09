//go:build linux

package adaptive

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/epoll"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#683: a switch whose outgoing engine closes its listeners at the
// pause (resource.Config.DisableDeferAccept: nothing to linger for) must not
// leave the port without a listener while the incoming engine's loops are
// slow to listen. epoll's ResumeAccept returns before any loop has re-created
// its socket, so the old order (resume, then pause the outgoing engine) opened
// a listener-free window as long as the slowest first loop, and a dial inside
// it was refused. With the default configuration the outgoing engine lingers
// for ~1.5 s (celeris#662) and the window is covered; the measurement that
// says so is in the PR.

// lateEpoll683 is the positive control and the failing-first fixture: an epoll
// engine whose ResumeAccept returns at once and takes effect delay later, so
// every loop's re-listen is late by construction.
type lateEpoll683 struct {
	*epoll.Engine
	delay time.Duration
	calls atomic.Int32
}

func (l *lateEpoll683) ResumeAccept() error {
	l.calls.Add(1)
	go func() {
		time.Sleep(l.delay)
		_ = l.Engine.ResumeAccept()
	}()
	return nil
}

// installLateEpoll683 swaps the epoll sub-engine for a late one in every slot
// the switch reads: the engine, the controller and, when epoll is active, the
// active pointer.
func installLateEpoll683(t *testing.T, e *Engine, delay time.Duration) *lateEpoll683 {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	ep, ok := e.primary.(*epoll.Engine)
	if !ok {
		t.Fatalf("the primary is %T, want *epoll.Engine", e.primary)
	}
	l := &lateEpoll683{Engine: ep, delay: delay}
	e.switchMu.Lock()
	e.primary = l
	e.ctrl.primary = l
	e.switchMu.Unlock()
	if e.ActiveEngine().Type() == engine.Epoll {
		var eng engine.Engine = l
		e.active.Store(&eng)
	}
	return l
}

// dialer683 dials 127.0.0.1:port at ~5k/s across four goroutines and records
// every refused dial with its start time.
type dialer683 struct {
	t0      time.Time
	mu      sync.Mutex
	refused []time.Duration
	other   int
	dials   atomic.Int64
	stop    chan struct{}
	wg      sync.WaitGroup
}

func startDialer683(addr string) *dialer683 {
	d := &dialer683{t0: time.Now(), stop: make(chan struct{})}
	for range 4 {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			for tick := time.Now(); ; tick = tick.Add(800 * time.Microsecond) {
				select {
				case <-d.stop:
					return
				default:
				}
				if w := time.Until(tick); w > 0 {
					time.Sleep(w)
				}
				at := time.Since(d.t0)
				c, err := net.DialTimeout("tcp", addr, time.Second)
				d.dials.Add(1)
				if err != nil {
					d.mu.Lock()
					if errors.Is(err, syscall.ECONNREFUSED) {
						d.refused = append(d.refused, at)
					} else {
						d.other++
					}
					d.mu.Unlock()
					continue
				}
				_ = c.Close()
			}
		}()
	}
	return d
}

func (d *dialer683) finish() (refused []time.Duration, other int) {
	close(d.stop)
	d.wg.Wait()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.refused, d.other
}

func TestRevertKeepsAListenerUpWhenTheIncomingEngineListensLate683(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	e, addr, stop := s0Bind(t, resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1, DisableDeferAccept: true,
		Resources: resource.Resources{Workers: 4}}, respHandler{})
	defer stop()
	port := e.Addr().(*net.TCPAddr).Port

	forceSwitchTo(t, e, engine.IOUring)
	// DisableDeferAccept: epoll's pause closed its listeners at once, and its
	// idle loops park. That is the state a revert finds, and the one in which
	// the issue's refusals were measured.
	time.Sleep(1500 * time.Millisecond)
	onlyIOUring, err := listenInodes(port)
	if err != nil {
		t.Skipf("cannot list listening sockets here: %v", err)
	}
	late := installLateEpoll683(t, e, 20*time.Millisecond)

	d := startDialer683(addr)
	time.Sleep(200 * time.Millisecond)
	swStart := time.Since(d.t0)
	t0 := time.Now()
	forceSwitchTo(t, e, engine.Epoll)
	swWall := time.Since(t0)
	swEnd := time.Since(d.t0)
	time.Sleep(400 * time.Millisecond)
	refused, other := d.finish()

	both, _ := listenInodes(port)
	var inWindow int
	for _, at := range refused {
		if at >= swStart-20*time.Millisecond && at <= swEnd+400*time.Millisecond {
			inWindow++
		}
	}
	t.Logf("celeris683 LATE-RESUME revert: switch wall %v, %d dials, refused %d (in the switch window %d), other errors %d, "+
		"listeners before %d after %d (%d new), wait timeouts %d, resume calls %d",
		swWall.Round(time.Microsecond), d.dials.Load(), len(refused), inWindow, other, len(onlyIOUring), len(both), freshCount683(both, onlyIOUring),
		e.listenWaitTimeouts(), late.calls.Load())

	// Premises: the resume was late and was the switch's, epoll had no
	// listener before it and has some after, and the dialer ran.
	if late.calls.Load() != 1 {
		t.Fatalf("celeris683 PREMISE: the late ResumeAccept ran %d times, want 1", late.calls.Load())
	}
	if fresh := freshCount683(both, onlyIOUring); fresh == 0 {
		t.Fatalf("celeris683 PREMISE: no listener on the port after the revert is new, so epoll never listened again "+
			"(before %d, after %d; the outgoing io_uring listeners close at the pause, so the count alone says nothing)",
			len(onlyIOUring), len(both))
	}
	if d.dials.Load() < 500 {
		t.Fatalf("celeris683 PREMISE: only %d dials in the run", d.dials.Load())
	}
	if len(refused) != 0 {
		t.Errorf("celeris683 GAP: %d dials were refused (%d inside the switch window): the outgoing io_uring listeners "+
			"closed before epoll had one", len(refused), inWindow)
	}
	if n := e.listenWaitTimeouts(); n != 0 {
		t.Errorf("celeris683: the wait for the incoming listener gave up %d time(s)", n)
	}
}

// The wait is not held under freezeState: a driver registering while the
// switch waits for a late incoming engine proceeds at once (RULE 10). With the
// wait inside the lock the registration would sit out the whole delay.
func TestSwitchWaitForTheIncomingListenerDoesNotBlockDriverRegistration683(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	e, _, stop := s0Bind(t, resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1, DisableDeferAccept: true,
		Resources: resource.Resources{Workers: 2}}, respHandler{})
	defer stop()
	port := e.Addr().(*net.TCPAddr).Port
	forceSwitchTo(t, e, engine.IOUring)
	time.Sleep(1500 * time.Millisecond)
	if _, err := listenInodes(port); err != nil {
		t.Skipf("cannot list listening sockets here: %v", err)
	}
	const delay = 150 * time.Millisecond
	installLateEpoll683(t, e, delay)

	done := make(chan time.Duration, 1)
	go func() {
		t0 := time.Now()
		e.ForceSwitch()
		done <- time.Since(t0)
	}()
	time.Sleep(40 * time.Millisecond) // inside the wait: the engine resumed, the delay not yet over
	t0 := time.Now()
	e.acquireDriverFD()
	e.releaseDriverFD()
	reg := time.Since(t0)
	wall := <-done
	t.Logf("celeris683 LOCK: driver register+unregister took %v while the switch waited; the switch took %v", reg, wall)
	if wall < delay/2 {
		t.Fatalf("celeris683 PREMISE: the switch took %v, it did not wait for the late engine (delay %v)", wall, delay)
	}
	if reg > delay/4 {
		t.Errorf("celeris683 LOCK: a driver registration took %v during the switch's wait (delay %v): "+
			"the wait holds freezeState", reg, delay)
	}
	if got := e.ActiveEngine().Type(); got != engine.Epoll {
		t.Errorf("the switch left %v active, want epoll", got)
	}
}

// The kernel list the wait reads names a socket exactly when the process has
// it open.
func TestListenInodesSeesASocketOnlyWhileItListens683(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("net.Listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	f, err := ln.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	set, err := listenInodes(port)
	if err != nil {
		t.Skipf("cannot list listening sockets here: %v", err)
	}
	if _, ok := set[uint32(st.Ino)]; !ok || len(set) != 1 {
		t.Fatalf("listenInodes(%d) = %v, want exactly the listener's inode %d", port, set, st.Ino)
	}
	other, err := listenInodes(port%60000 + 1) // a port nothing here listens on
	if err != nil || len(other) != 0 {
		t.Fatalf("listenInodes on a port with no listener = %v, %v", other, err)
	}
	_ = ln.Close()
	set, err = listenInodes(port)
	if err != nil || len(set) != 0 {
		t.Fatalf("listenInodes after Close = %v, %v, want none", set, err)
	}
}

// wait returns when a socket that was not in the watch appears, after about
// as long as it took to appear, and gives up at the bound when none does.
func TestListenWatchWaitsForANewListenerAndTimesOut683(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("net.Listen: %v", err)
	}
	defer func() { _ = first.Close() }()
	port := first.Addr().(*net.TCPAddr).Port
	w, err := newListenWatch(port)
	if err != nil {
		t.Skipf("cannot list listening sockets here: %v", err)
	}

	waited, ok, err := w.wait(60 * time.Millisecond)
	if err != nil || ok || waited < 60*time.Millisecond || waited > 500*time.Millisecond {
		t.Fatalf("no new listener: wait = %v, %v, %v; want a timeout at ~60ms", waited, ok, err)
	}

	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		}); err != nil {
			return err
		}
		return serr
	}}
	// The first listener has no SO_REUSEPORT, so a second one cannot join it:
	// watch a fresh reuseport group instead.
	_ = first.Close()
	a, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("reuseport listen: %v", err)
	}
	defer func() { _ = a.Close() }()
	port = a.Addr().(*net.TCPAddr).Port
	w, err = newListenWatch(port)
	if err != nil {
		t.Fatal(err)
	}
	bc := make(chan net.Listener, 1)
	go func() {
		time.Sleep(30 * time.Millisecond)
		b, _ := lc.Listen(context.Background(), "tcp", "127.0.0.1:"+strconv.Itoa(port))
		bc <- b
	}()
	waited, ok, err = w.wait(2 * time.Second)
	if err != nil || !ok || waited < 25*time.Millisecond || waited > time.Second {
		t.Fatalf("a listener joined after 30ms: wait = %v, %v, %v", waited, ok, err)
	}
	if b := <-bc; b != nil {
		_ = b.Close()
	}
}

// freshCount683 is the number of inodes in after that are not in before.
func freshCount683(after, before map[uint32]struct{}) int {
	n := 0
	for ino := range after {
		if _, was := before[ino]; !was {
			n++
		}
	}
	return n
}
