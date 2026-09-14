//go:build linux

package adaptive

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/probe"
	"github.com/goceleris/celeris/resource"
)

// preBoundAdaptive starts an adaptive engine exactly the way the validation
// reference apps (and any Server.StartWithListener caller) do it: the operator
// asks for "<host>:0", the caller binds that itself with net.Listen and hands
// the resulting listener to celeris, so Config.Addr still says port 0 while
// Config.Listener is already bound to a concrete port.
//
// It returns the engine, the address the SUPPLIED LISTENER owns (which is the
// address the engine must end up serving on), and a stop func.
func preBoundAdaptive(t *testing.T, workers int) (*Engine, string, func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-bind listener: %v", err)
	}
	want := ln.Addr().String()

	cfg := resource.Config{
		Addr:      "127.0.0.1:0",
		Listener:  ln,
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: workers},
	}

	e, err := New(cfg, respHandler{}, nil)
	if err != nil {
		_ = ln.Close()
		t.Fatalf("adaptive.New with a pre-bound listener (%s): %v", want, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()

	for dl := time.Now().Add(10 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		cancel()
		if lerr := <-done; lerr != nil {
			t.Fatalf("adaptive engine never bound; Listen: %v", lerr)
		}
		t.Fatal("adaptive engine never bound")
	}
	return e, want, func() {
		cancel()
		<-done
	}
}

// getOnce issues one HTTP/1.1 request on its own connection and returns the
// status code.
func getOnce(t *testing.T, addr string) int {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write %s: %v", addr, err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("read %s: %v", addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestAdaptiveStartsWithPreBoundListener is the celeris#614 regression: the
// DEFAULT engine could not start at all when the caller supplied a pre-bound
// listener, because New() ran resolvePort on Config.Addr — opening a listener
// on a brand-new ephemeral port and writing THAT port into Addr — and the
// sub-engine constructor then rejected the config it had just been handed
// ("ambiguous configuration: Addr=... but Listener is bound to ...").
//
// The engine must start, and it must serve on the SUPPLIED LISTENER's port,
// not on one it invented.
func TestAdaptiveStartsWithPreBoundListener(t *testing.T) {
	e, want, stop := preBoundAdaptive(t, 2)
	defer stop()

	if got := e.Addr().String(); got != want {
		t.Fatalf("adaptive bound %s, want the pre-bound listener's own address %s", got, want)
	}
	if code := getOnce(t, want); code != 200 {
		t.Fatalf("GET %s = %d, want 200", want, code)
	}
	t.Logf("adaptive started on the pre-bound listener's address %s and served a request", want)
}

// TestAdaptiveRejectsNonTCPListener pins the one listener shape adaptive
// cannot serve. Two sub-engines sharing one address is an SO_REUSEPORT
// arrangement, which is TCP-only; a Unix-socket listener would otherwise
// start fine on the start engine and only surface as a 5-second standby bind
// timeout on the first promotion, minutes later. The failure has to land at
// New().
func TestAdaptiveRejectsNonTCPListener(t *testing.T) {
	sock := t.TempDir() + "/adaptive.sock"
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix listener unavailable: %v", err)
	}
	defer func() { _ = ln.Close() }()

	_, err = New(resource.Config{Addr: "127.0.0.1:0", Listener: ln, Protocol: engine.HTTP1}, respHandler{}, nil)
	if err == nil {
		t.Fatal("adaptive.New accepted a unix listener; both sub-engines cannot bind it with SO_REUSEPORT")
	}
	if !strings.Contains(err.Error(), "needs a TCP listener") {
		t.Fatalf("error must name the TCP requirement, got: %v", err)
	}
	t.Logf("rejected at New(): %v", err)
}

// TestAdaptiveSwitchesWithPreBoundListener is the other half of celeris#614:
// starting is not the point of the adaptive engine, the handover is. Only one
// listener arrives but both sub-engines need the same port, so this drives the
// controller past its promotion threshold (24 conns/worker sustained over two
// one-second ticks; Workers=2 makes that 48 connections) and asserts that the
// lazily-built io_uring standby really did join the SO_REUSEPORT group on the
// pre-bound listener's address — and that requests keep succeeding across the
// switch.
//
// The supplied listener is a plain net.Listen listener with no SO_REUSEPORT
// option of its own; it works because the start sub-engine CLOSES it and
// rebinds its own SO_REUSEPORT sockets on the same address, so the start
// engine is the group's first member and the standby can join it later.
func TestAdaptiveSwitchesWithPreBoundListener(t *testing.T) {
	if testing.Short() {
		t.Skip("switch integration test")
	}
	if !probe.Probe().IOUringTier.Available() {
		t.Skip("io_uring unavailable: the switch needs both sub-engines")
	}

	e, want, stop := preBoundAdaptive(t, 2)
	defer stop()

	if code := getOnce(t, want); code != 200 {
		t.Fatalf("pre-switch GET %s = %d, want 200", want, code)
	}

	// 64 keep-alive conns over 2 workers = 32 conns/worker: above the 24
	// up-threshold (so the two-tick sustain path fires) and below the 48
	// high-watermark, so this exercises the sustained promotion, not the
	// single-tick fast snap.
	const conns = 64
	p := &rampPool{addr: want, client: h1Client}
	defer p.stopAll()
	p.rampTo(conns)

	deadline := time.Now().Add(30 * time.Second)
	for e.Metrics().AdaptiveSwitches == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	switches := e.Metrics().AdaptiveSwitches
	if switches < 1 {
		t.Fatalf("no engine switch after 30s at %d conns / %d workers: AdaptiveSwitches=%d (epoll conns=%d)",
			conns, 2, switches, aconns(e.primary))
	}
	t.Logf("AdaptiveSwitches=%d after promoting at %d conns", switches, conns)

	// The port must not have moved, and the freshly-built standby must be
	// listening on the SAME address the caller pre-bound — that is the whole
	// SO_REUSEPORT contract the adaptive switch rests on.
	if got := e.Addr().String(); got != want {
		t.Errorf("after the switch the engine reports %s, want %s", got, want)
	}
	e.mu.Lock()
	standby := e.secondary
	e.mu.Unlock()
	if standby == nil {
		t.Fatal("switch recorded but the io_uring sub-engine was never built")
	}
	if standby.Addr() == nil {
		t.Fatal("io_uring standby has no bound address after the switch")
	}
	if got := standby.Addr().String(); got != want {
		t.Errorf("io_uring standby bound %s, want the pre-bound listener's address %s", got, want)
	}

	// New connections land on the promoted engine and are served.
	for i := range 5 {
		if code := getOnce(t, want); code != 200 {
			t.Fatalf("post-switch GET #%d %s = %d, want 200", i, want, code)
		}
	}
	if got := aconns(standby); got == 0 && standby.Metrics().RequestCount == 0 {
		t.Errorf("io_uring standby served nothing after the switch (requests=%d)", standby.Metrics().RequestCount)
	}

	// The held keep-alive conns must have kept working across the handover.
	okBefore := p.ok.Load()
	time.Sleep(1500 * time.Millisecond)
	okAfter := p.ok.Load()
	p.stopAll()
	dumpErrSamples(t)
	t.Logf("held conns: ok %d -> %d, err=%d, iouring reqs=%d",
		okBefore, okAfter, p.errc.Load(), standby.Metrics().RequestCount)
	if okAfter <= okBefore {
		t.Errorf("held connections stopped being served after the switch: ok %d -> %d", okBefore, okAfter)
	}
	if p.errc.Load() > 0 {
		t.Errorf("held connections saw %d request errors across the switch (want 0)", p.errc.Load())
	}
}
