//go:build linux

package iouring

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#896. Listen used to return while its listen sockets were still
// bound. Each worker closed its listen descriptor in shutdown while the
// multishot accept armed on its ring still held a reference to the socket's
// file, and that reference went only when the kernel tore the ring down,
// asynchronously, after close(ring) returned: for about 2 ms after Listen had
// returned the port was still taken, and a plain listener (net.Listen: no
// SO_REUSEPORT) that bound it got EADDRINUSE; a client that dialled it was
// accepted by the dying socket instead of refused.
//
// The oracle is a plain bind, made once, the moment Listen returns, with no
// retry before it. A celeris engine relistening on the port cannot be the
// oracle: it sets SO_REUSEPORT, joins the dying socket's group and succeeds
// on main as well.

const iterations896 = 200

// engineRun896 is one started engine and the way to stop it.
type engineRun896 struct {
	e      *Engine
	cancel context.CancelFunc
	done   chan error
}

// stop cancels Listen's context and returns the instant Listen has returned,
// with its error.
func (r *engineRun896) stop() (time.Time, error) {
	r.cancel()
	err := <-r.done
	return time.Now(), err
}

// start896 starts a standalone io_uring engine on addr and returns once it
// has published its address. A start that fails only on ring ENOMEM (the
// kernel uncharges closed rings asynchronously, ring_budget_linux_test.go) is
// retried; it is made before the measured part of an iteration, so the retry
// cannot hide a held port.
func start896(t *testing.T, addr string, workers int) *engineRun896 {
	t.Helper()
	return start896Config(t, addr, workers, false)
}

// start896Config is start896 with TCP_DEFER_ACCEPT off, so a pause closes at
// once, when disableDefer is set.
func start896Config(t *testing.T, addr string, workers int, disableDefer bool) *engineRun896 {
	t.Helper()
	var run *engineRun896
	_, _, err := retryRingENOMEM(func() error {
		e, err := New(resource.Config{
			Addr:      addr,
			Engine:    engine.IOUring,
			Protocol:  engine.HTTP1,
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Resources: resource.Resources{Workers: workers},

			DisableDeferAccept: disableDefer,
		}, respondingHandler{})
		if err != nil {
			skipOrFail656(t, "New: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- e.Listen(ctx) }()
		for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); {
			if e.Addr() != nil {
				run = &engineRun896{e: e, cancel: cancel, done: done}
				return nil
			}
			select {
			case lerr := <-done:
				cancel()
				return lerr
			case <-time.After(time.Millisecond):
			}
		}
		cancel()
		<-done
		t.Fatalf("engine on %s never published an address", addr)
		return nil
	})
	if err != nil {
		if ioUringUnavailable639(err) {
			skipUnlessIOUringUnusable(t, err)
		}
		t.Fatalf("engine start on %s: %v", addr, err)
	}
	return run
}

// plainBind896 is the oracle: one net.Listen on addr (SO_REUSEADDR, no
// SO_REUSEPORT), closed at once. nil means the port was free.
func plainBind896(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return ln.Close()
}

// freeAfter896 polls the oracle every 50 us after a refusal and returns how
// long after since the port came free, or -1 if it did not within bound.
func freeAfter896(addr string, since time.Time, bound time.Duration) time.Duration {
	for time.Since(since) < bound {
		if plainBind896(addr) == nil {
			return time.Since(since)
		}
		time.Sleep(50 * time.Microsecond)
	}
	return -1
}

// churn896 runs clients that dial addr and send a request until stop is
// closed, so connections are arriving and being accepted while the engine is
// shut down.
func churn896(addr string, stop <-chan struct{}) *sync.WaitGroup {
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
				if err != nil {
					time.Sleep(200 * time.Microsecond)
					continue
				}
				_, _ = c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
				_ = c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
				buf := make([]byte, 128)
				_, _ = c.Read(buf)
				_ = c.Close()
			}
		}()
	}
	return &wg
}

// TestOracleSeesALiveReuseportListener896 is the oracle control: the plain
// bind of this file's oracle is refused by a socket made the way the engine
// makes its own, so a pass below is not the oracle being blind.
func TestOracleSeesALiveReuseportListener896(t *testing.T) {
	oracleControl656(t, freeAddr639(t))
}

type result896 struct {
	bad       int           // iterations whose probe saw the port still taken
	maxFree   time.Duration // the slowest the plain bind took to succeed after Listen returned
	neverFree int
}

// probe896 is what an iteration does the instant Listen has returned.
type probe896 int

const (
	// probeBind makes the plain-bind oracle: bad is a bind refused.
	probeBind probe896 = iota
	// probeDial dials the port: bad is a connection the closed engine's port
	// still accepted (a refused dial is the correct answer).
	probeDial
)

func (p probe896) String() string {
	if p == probeBind {
		return "plain bind"
	}
	return "dial"
}

// runRelisten896 starts and stops a standalone engine n times, each on its own
// free port, and takes one probe the instant Listen returns. With load,
// clients keep connecting through the shutdown.
func runRelisten896(t *testing.T, n, workers int, load bool, p probe896) result896 {
	t.Helper()
	var res result896
	for i := range n {
		addr := freeAddr639(t)
		run := start896(t, addr, workers)
		var stopLoad chan struct{}
		var wg *sync.WaitGroup
		if load {
			stopLoad = make(chan struct{})
			wg = churn896(addr, stopLoad)
			time.Sleep(5 * time.Millisecond)
		}
		returned, err := run.stop()
		if err != nil {
			t.Fatalf("iteration %d: Listen returned %v after a cancel", i, err)
		}
		switch p {
		case probeBind:
			if perr := plainBind896(addr); perr != nil {
				if !errors.Is(perr, unix.EADDRINUSE) {
					t.Fatalf("iteration %d: plain bind: %v, want nil or EADDRINUSE", i, perr)
				}
				res.bad++
				d := freeAfter896(addr, returned, time.Second)
				if d < 0 {
					res.neverFree++
				}
				res.maxFree = max(res.maxFree, d)
			}
		case probeDial:
			if c, derr := net.DialTimeout("tcp", addr, time.Second); derr == nil {
				res.bad++
				_ = c.Close()
			}
		}
		if load {
			close(stopLoad)
			wg.Wait()
		}
	}
	return res
}

// check896 runs one arm and fails when any iteration found the port taken.
func check896(t *testing.T, label string, n, workers int, load bool, p probe896) {
	t.Helper()
	r := runRelisten896(t, n, workers, load, p)
	t.Logf("%s, %s probe: %d relistens, port still taken at Listen's return in %d (%.0f%%); "+
		"slowest to come free %v (never %d)", label, p, n, r.bad, 100*float64(r.bad)/float64(n), r.maxFree, r.neverFree)
	if r.bad != 0 {
		t.Fatalf("%s: Listen returned with its port still taken in %d of %d relistens (%s probe)", label, r.bad, n, p)
	}
}

// The regression assertions: after Listen returns the port is free, at once,
// 200 times. Idle engine, then clients connecting through the shutdown (accepts
// completing while the cancel is made), each with both probes.

func TestListenReleasesThePortBeforeItReturnsBind896(t *testing.T) {
	check896(t, "idle", iterations896, 2, false, probeBind)
}

func TestListenReleasesThePortBeforeItReturnsDial896(t *testing.T) {
	check896(t, "idle", iterations896, 2, false, probeDial)
}

func TestListenReleasesThePortUnderLoadBind896(t *testing.T) {
	check896(t, "under load", iterations896, 2, true, probeBind)
}

func TestListenReleasesThePortUnderLoadDial896(t *testing.T) {
	check896(t, "under load", iterations896, 2, true, probeDial)
}

// TestListenReleasesThePortWithManyWorkers896: every worker closes its own
// socket, and Listen returns after all of them. Needs memlock for 4 workers.
func TestListenReleasesThePortWithManyWorkers896(t *testing.T) {
	if maxW := MaxWorkersForMemlock(); maxW >= 0 && maxW < 4 {
		skipOrFail656(t, "RLIMIT_MEMLOCK funds %d io_uring workers, the test needs 4", maxW)
	}
	check896(t, "4 workers", iterations896/2, 4, false, probeBind)
}

// TestPauseAcceptReleasesThePort896 is the sibling check of the pause: the
// listen sockets a PauseAccept closes (closeListenerAfterDrain cancels their
// accept first) are gone from the port when it returns, 200 times, as seen by
// a plain bind made at once. It passes on main as well; it pins that the
// shutdown path now behaves as the pause path always did.
func TestPauseAcceptReleasesThePort896(t *testing.T) {
	busy := 0
	var slowest time.Duration
	for i := range iterations896 {
		addr := freeAddr639(t)
		e := start896Config(t, addr, 2, true)
		if err := e.e.PauseAccept(); err != nil {
			t.Fatalf("iteration %d: PauseAccept: %v", i, err)
		}
		returned := time.Now()
		if perr := plainBind896(addr); perr != nil {
			busy++
			slowest = max(slowest, freeAfter896(addr, returned, time.Second))
		}
		if _, err := e.stop(); err != nil {
			t.Fatalf("iteration %d: Listen returned %v", i, err)
		}
	}
	t.Logf("pause: %d PauseAccept calls, port still taken at return in %d (%.0f%%), slowest to come free %v",
		iterations896, busy, 100*float64(busy)/float64(iterations896), slowest)
	if busy != 0 {
		t.Fatalf("PauseAccept returned with its port still taken in %d of %d pauses", busy, iterations896)
	}
}

// TestFailedStartReleasesThePort896: a Listen that fails because one worker's
// ring setup failed, after its sibling started, shuts the sibling down; the
// port must be free when Listen returns the error. Needs two workers.
func TestFailedStartReleasesThePort896(t *testing.T) {
	const n = 50
	busy := 0
	for range n {
		addr := freeAddr639(t)
		e := newEngine656(t, addr, 2)
		failRingSetup656(t, func(call int32) bool { return call == 1 })
		listenExpectingInjectedFailure(t, e)
		if perr := plainBind896(addr); perr != nil {
			busy++
		}
	}
	t.Logf("failed start: %d Listen failures, port still taken at return in %d", n, busy)
	if busy != 0 {
		t.Fatalf("a failed Listen returned with its port still taken in %d of %d starts", busy, n)
	}
}

// TestShutdownWaitForTheAcceptIsBounded896 (RULE 10): a kernel that never
// completes the cancel must not hold shutdown open. The ring wait is
// answered with nothing ever submitted, so no CQE arrives; shutdown gives up
// at the drain's bound, counts it, and Listen returns.
func TestShutdownWaitForTheAcceptIsBounded896(t *testing.T) {
	orig := shutdownRingWait
	shutdownRingWait = func(_ *Ring, d time.Duration) error { time.Sleep(d); return nil }
	t.Cleanup(func() { shutdownRingWait = orig })

	run := start896(t, freeAddr639(t), 2)
	t0 := time.Now()
	returned, err := run.stop()
	took := returned.Sub(t0)
	if err != nil {
		t.Fatalf("Listen returned %v", err)
	}
	gaveUp := run.e.metrics.handoffLoss.shutdownListenCancelGaveUp.Load()
	t.Logf("shutdown with a ring that never answers: Listen returned after %v, give-ups counted %d (bound %v)",
		took, gaveUp, time.Duration(shutdownFDDrainNanos))
	if gaveUp == 0 {
		t.Fatal("shutdown did not count the accept it could not wait for")
	}
	if took < time.Duration(shutdownFDDrainNanos)/2 {
		t.Fatalf("Listen returned after %v: shutdown did not wait for the accept at all", took)
	}
	if took > time.Duration(shutdownFDDrainNanos)+3*time.Second {
		t.Fatalf("Listen took %v: the wait for the accept is not bounded by %v", took, time.Duration(shutdownFDDrainNanos))
	}
}
