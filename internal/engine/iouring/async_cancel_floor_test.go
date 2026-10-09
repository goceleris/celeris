//go:build linux

package iouring

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/probe"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#682: every cancel the engine submits sets IORING_ASYNC_CANCEL
// flags, which exist from Linux 5.19; through 5.18 the kernel fails each one
// with -EINVAL and leaves its target running. So a kernel whose cancels fail
// gets no io_uring engine: New refuses it as unavailable, the adaptive engine
// starts on epoll, and an explicit io_uring engine fails to start with the
// kernel requirement in its error. The tests in this file use only what the
// package had before the gate, so they can be run against a tree without it.

// TestNewRefusesAKernelThatRejectsAsyncCancelFlags gives New the answer a
// kernel before 5.19 gives the probe, on any kernel (the runAsyncCancelProbe
// seam). New must build no engine and say why: its error is the unavailable
// error the adaptive engine falls back on, and names the kernel requirement.
func TestNewRefusesAKernelThatRejectsAsyncCancelFlags(t *testing.T) {
	r, err := NewRing(8, 0, 0)
	if err != nil {
		skipOrFail656(t, "io_uring unavailable: %v", err)
	}
	_ = r.Close()
	saved := runAsyncCancelProbe
	runAsyncCancelProbe = func() (asyncCancelProbe, string) {
		return asyncCancelRejected, "IORING_ASYNC_CANCEL flags rejected: cqe.res=-22 (EINVAL); the kernel predates Linux 5.19"
	}
	resetAsyncCancelProbeCache()
	t.Cleanup(func() {
		runAsyncCancelProbe = saved
		resetAsyncCancelProbeCache() // the next New probes the kernel again
	})
	var logs lockedBuffer
	e, err := New(resource.Config{
		Addr:     "127.0.0.1:0",
		Protocol: engine.HTTP1,
		Logger:   slog.New(slog.NewJSONHandler(&logs, nil)),
	}, transplantTestHandler{})
	if err == nil {
		t.Fatalf("New built an io_uring engine (async_cancel_flags=%v) for a kernel that rejects the "+
			"IORING_ASYNC_CANCEL flags: every close, pause and unregister cancel it submits would fail "+
			"with -EINVAL and leave its target running (celeris#682)", e.asyncCancelFlags)
	}
	if e != nil {
		t.Errorf("New returned an engine with its error %q", err)
	}
	msg := err.Error()
	t.Logf("celeris682 New's error: %s", msg)
	if !strings.HasPrefix(msg, "io_uring not available on this system") || !strings.Contains(msg, "5.19") ||
		!strings.Contains(msg, "EINVAL") {
		t.Errorf("New's error %q: want it to say io_uring is not available (the adaptive engine's fallback and "+
			"the tests that tolerate an unavailable kernel read that), name Linux 5.19 and carry the probe's reason", msg)
	}
	var probeRecs int
	for _, rec := range logs.records(t) {
		m, _ := rec["msg"].(string)
		if strings.HasPrefix(m, "async cancel flags") {
			probeRecs++
		}
		if strings.HasPrefix(m, "io_uring engine selected") {
			t.Errorf("New logged %q for a kernel it refuses", m)
		}
	}
	if probeRecs != 1 {
		t.Errorf("New logged %d async-cancel-flags probe records, want 1: the probe's answer is logged "+
			"when io_uring is refused too", probeRecs)
	}
}

// cancelForm682 is one of the four ASYNC_CANCEL forms the engine builds,
// aimed at a pending recv.
type cancelForm682 struct {
	name string
	prep func(sqe unsafe.Pointer, recvUD uint64, fd int)
}

var cancelForms682 = []cancelForm682{
	{"close path, by user_data, skip success (cancelConnOps)", func(sqe unsafe.Pointer, ud uint64, _ int) {
		prepCancelUserDataSkipSuccess(sqe, ud)
	}},
	{"by user_data, reported (WebSocket pause, hand-off reap)", func(sqe unsafe.Pointer, ud uint64, _ int) {
		prepCancelUserDataReported(sqe, ud)
	}},
	{"by fd, skip success (PauseAccept)", func(sqe unsafe.Pointer, _ uint64, fd int) {
		prepCancelFDSkipSuccess(sqe, fd)
	}},
	{"by fd, reported (driver unregister)", func(sqe unsafe.Pointer, _ uint64, fd int) {
		prepCancelFDDriver(sqe, fd)
	}},
}

// runCancelForm682 asks the running kernel what one of the engine's cancel
// forms does, on a private ring, independently of the engine's probe: it
// arms a recv on one end of a socket pair, submits the cancel, and waits up
// to 500 ms for the recv's own completion. The recv is cancelled when that
// completion is -ECANCELED. Otherwise the cancel's own result says why (a
// kernel before 5.19 answers -EINVAL and the recv stays pending).
func runCancelForm682(t *testing.T, f cancelForm682) (cancelled bool, cancelRes int32, cancelSeen bool) {
	t.Helper()
	const recvUD, cancelUD = uint64(0x682_0001), uint64(0x682_0002)
	ring, err := NewRing(8, 0, 0)
	if err != nil {
		t.Fatalf("NewRing: %v", err)
	}
	defer func() { _ = ring.Close() }()
	sp, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	defer func() { _ = unix.Close(sp[0]); _ = unix.Close(sp[1]) }()
	buf := make([]byte, 64)
	sqe := ring.GetSQE()
	prepRecv(sqe, sp[0], buf)
	setSQEUserData(sqe, recvUD)
	if _, err := ring.Submit(); err != nil {
		t.Fatalf("submit the recv: %v", err)
	}
	sqe = ring.GetSQE()
	f.prep(sqe, recvUD, sp[0])
	setSQEUserData(sqe, cancelUD)
	if _, err := ring.Submit(); err != nil {
		t.Fatalf("submit the cancel: %v", err)
	}
	recvRes, recvSeen := int32(0), false
	for deadline := time.Now().Add(500 * time.Millisecond); !recvSeen && time.Now().Before(deadline); {
		if err := ring.SubmitAndWaitTimeout(time.Until(deadline)); err != nil {
			t.Fatalf("wait: %v", err)
		}
		head, tail := ring.BeginCQ()
		for ; head != tail; head++ {
			c := ring.cqeAt(head)
			switch c.UserData {
			case recvUD:
				recvRes, recvSeen = c.Res, true
			case cancelUD:
				cancelRes, cancelSeen = c.Res, true
			}
		}
		ring.EndCQ(head)
	}
	if !recvSeen {
		// Let the recv complete before the ring and the buffer go.
		_, _ = unix.Write(sp[1], []byte("x"))
		_ = ring.SubmitAndWaitTimeout(100 * time.Millisecond)
	}
	runtime.KeepAlive(buf)
	return recvSeen && recvRes == -int32(unix.ECANCELED), cancelRes, cancelSeen
}

// TestIOUringOnTheRunningKernel682 checks New's decision against what the
// running kernel does with the engine's own cancels, not against its version
// or the probe (celeris#682). Each of the four cancel forms the engine builds
// is submitted against a pending recv on a private ring. Where any of them
// leaves the recv running, New must refuse io_uring with the kernel
// requirement in its error; where all of them cancel it, New must build the
// engine from 5.19. Below 5.19 New refuses on the version alone even then
// (celeris#872: a kernel whose four forms all work there is a full vendor
// backport, and the floor still excludes it), which the test logs and
// accepts. When an engine is built it is also run: keep-alive connections the
// engine closes at their idle timeout must each get a FIN, which a close
// whose cancel failed does not send (the recv still holds the socket).
func TestIOUringOnTheRunningKernel682(t *testing.T) {
	r, err := NewRing(8, 0, 0)
	if err != nil {
		skipOrFail656(t, "io_uring unavailable: %v", err)
	}
	_ = r.Close()
	probeRes, probeWhy := probeAsyncCancelFlagsCached()
	var summary []string
	failing := 0
	for _, f := range cancelForms682 {
		cancelled, res, seen := runCancelForm682(t, f)
		what := "cancelled the recv"
		if !cancelled {
			failing++
			what = "left the recv pending"
			if seen {
				what += fmt.Sprintf(", cancel res=%d (%s)", res, errnoName(-res))
			}
		}
		summary = append(summary, f.name+": "+what)
	}
	t.Logf("celeris682 kernel=%s probe=%v (%q); engine cancel forms that fail: %d of %d: %s",
		kernelRelease(), probeRes, probeWhy, failing, len(cancelForms682), strings.Join(summary, "; "))

	var logs lockedBuffer
	cfg := resource.Config{
		Addr:        "127.0.0.1:0",
		Protocol:    engine.HTTP1,
		Resources:   resource.Resources{Workers: 2},
		IdleTimeout: 300 * time.Millisecond,
		Logger:      slog.New(slog.NewJSONHandler(&logs, nil)),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	cfg.Addr = ln.Addr().String()
	_ = ln.Close()
	e, err := New(cfg, transplantTestHandler{})
	t.Logf("celeris682 New: engine built=%v err=%v", e != nil, err)
	if failing > 0 {
		if err == nil {
			t.Errorf("New built an io_uring engine on kernel %s, where %d of the engine's %d cancel forms leave "+
				"their target running: every close, pause and unregister cancel would fail (celeris#682)",
				kernelRelease(), failing, len(cancelForms682))
		} else if !strings.Contains(err.Error(), "5.19") {
			t.Errorf("New refused io_uring with %q, want the kernel requirement (Linux 5.19) named", err)
		}
	} else if err != nil {
		if strings.Contains(err.Error(), "5.19") {
			// Below 5.19 New refuses on the version alone (celeris#872): a
			// kernel whose four cancel forms all work there is a full vendor
			// backport, and the floor still excludes it.
			if kv := probe.Probe(); kv.KernelMajor > 5 || (kv.KernelMajor == 5 && kv.KernelMinor >= 19) {
				t.Fatalf("New refused io_uring on kernel %s, where every cancel form works: %v", kernelRelease(), err)
			}
			t.Logf("celeris682 kernel %s predates 5.19: New refuses on the version although every cancel form works: %v",
				kernelRelease(), err)
			return
		}
		skipOrFail656(t, "iouring engine unavailable: %v", err)
	}
	if err != nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("engine did not stop within 10s")
		}
	})
	const conns = 4
	fins := 0
	var outcomes []string
	for i := 0; i < conns; i++ {
		outcome, fin := idleCloseOutcome682(t, cfg.Addr)
		if fin {
			fins++
		}
		outcomes = append(outcomes, outcome)
	}
	t.Logf("celeris682 idle closes that sent a FIN: %d of %d (%s)", fins, conns, strings.Join(outcomes, ", "))
	if fins != conns {
		t.Errorf("%d of %d keep-alive connections the engine closed at their idle timeout got no FIN within 3 s: "+
			"the close's cancel left the recv holding the socket open (kernel %s, %d of %d cancel forms fail)",
			conns-fins, conns, kernelRelease(), failing, len(cancelForms682))
	}
}

// idleCloseOutcome682 serves one request on a keep-alive connection, then
// waits up to 3 s, sending nothing, for the engine to close the connection
// at its idle timeout: fin is true when the client reads EOF.
func idleCloseOutcome682(t *testing.T, addr string) (outcome string, fin bool) {
	t.Helper()
	var c net.Conn
	var err error
	for dl := time.Now().Add(5 * time.Second); ; {
		if c, err = net.DialTimeout("tcp", addr, time.Second); err == nil || time.Now().After(dl) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, " 200 ") {
		t.Fatalf("response status line %q, err %v", status, err)
	}
	var clen int
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("response headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "Content-Length") {
			_, _ = fmt.Sscanf(strings.TrimSpace(v), "%d", &clen)
		}
	}
	if _, err := io.CopyN(io.Discard, br, int64(clen)); err != nil {
		t.Fatalf("response body: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := br.Read(make([]byte, 1))
	switch {
	case errors.Is(err, io.EOF):
		return "EOF", true
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "no FIN in 3s", false
	default:
		return fmt.Sprintf("read n=%d err=%v", n, err), false
	}
}
