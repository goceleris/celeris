//go:build linux

package iouring

// SCRATCH, not committed (celeris panic-detachMu side check, 2026-09-28).
//
// runAsyncHandler takes cs.detachMu before conn.ProcessH1 and its deferred
// recover() does not release it. A panic inside ProcessH1 therefore unwinds with
// the lock held; the recover enqueues the conn as asyncClosed, and the worker's
// drainDetachQueue -> closeConn -> cs.detachMu.Lock() never returns. The engines
// refuse Workers: 1 (">= 2 if set"), so the engine gets 2 workers (1 io_uring worker
// under an 8 MiB memlock) and /boom is sent BOOMS times on fresh connections, so
// every worker takes one w.h.p. (SO_REUSEPORT spreads connections). Prediction: no
// later request is answered and Listen does not return after its context is cancelled.
//
// Arms (each run as its own process, -test.run anchored):
//   AsyncPanic  : the async route panics                      -> predicted FAIL (wedge)
//   AsyncError  : the async route returns an error (CONTROL)  -> predicted PASS: same
//                 asyncClosed -> detachQueue -> closeConn teardown, lock released
//   AsyncGoexit : the async route calls runtime.Goexit        -> recover() is nil, lock leaks
//   SyncPanic   : the route runs inline on the worker; the engine has no recover on
//                 that path, so the process dies (checked in a child process)

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

type zzPDHandler struct {
	mode  string // panic | error | goexit
	async bool
}

func (h zzPDHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.Path == "/boom" {
		switch h.mode {
		case "panic":
			panic("zz handler panic")
		case "error":
			return errors.New("zz handler error")
		case "goexit":
			runtime.Goexit()
		}
	}
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}
func (h zzPDHandler) RouteAsync(_, path string) bool { return h.async && path == "/boom" }
func (h zzPDHandler) HasAsyncRoutes() bool           { return h.async }

const zzPDBooms = 12

func zzPDErrClass(err error) string {
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, io.EOF):
		return "EOF"
	case strings.Contains(err.Error(), "reset"):
		return "reset"
	}
	return err.Error()
}

func zzPDGet(addr, path string, dl time.Duration) (string, error) {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(dl))
	if _, err := c.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line), err
}

// zzPDBlocked returns every goroutine whose stack is parked in sync.(*Mutex).Lock
// with a frame from this engine package.
func zzPDBlocked(pkgFrag string) []string {
	buf := make([]byte, 8<<20)
	n := runtime.Stack(buf, true)
	dump := string(buf[:n])
	if dir := os.Getenv("ZZ_DUMP_DIR"); dir != "" {
		_ = os.WriteFile(dir+"/"+strings.ReplaceAll(os.Getenv("ZZ_ARM"), "/", "_")+".goroutines.txt", buf[:n], 0o644)
	}
	var out []string
	for _, g := range strings.Split(dump, "\n\n") {
		if strings.Contains(g, "sync.(*Mutex).Lock") && strings.Contains(g, pkgFrag) {
			out = append(out, g)
		}
	}
	return out
}

func zzPDRun(t *testing.T, mode string, async bool) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, err := New(resource.Config{Addr: addr, Protocol: engine.HTTP1, Resources: resource.Resources{Workers: 2},
		AsyncHandlers: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, zzPDHandler{mode: mode, async: async})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	for dl := time.Now().Add(8 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("ZZPD mode=%s async=%v workers=%d GOARCH=%s", mode, async, e.NumWorkers(), runtime.GOARCH)
	// Warm-up: the worker answers before the fault.
	if r, err := zzPDGet(addr, "/ok", time.Second); err != nil || !strings.HasPrefix(r, "HTTP/1.1 200") {
		t.Fatalf("warm-up not answered: %q %v", r, err)
	}
	outcomes := map[string]int{}
	for range zzPDBooms {
		r, err := zzPDGet(addr, "/boom", time.Second)
		k := r
		if err != nil {
			k = r + " err=" + zzPDErrClass(err)
		}
		outcomes[k]++
	}
	t.Logf("ZZPD /boom x%d -> %v", zzPDBooms, outcomes)
	time.Sleep(300 * time.Millisecond)
	blocked := zzPDBlocked("/engine/iouring.")
	t.Logf("ZZPD goroutines parked in sync.(*Mutex).Lock with an engine/iouring frame: %d", len(blocked))
	for _, g := range blocked {
		t.Logf("ZZPD BLOCKED\n%s", g)
	}
	ok, lost := 0, 0
	for i := range 16 {
		r, err := zzPDGet(addr, "/ok", time.Second)
		if err == nil && strings.HasPrefix(r, "HTTP/1.1 200") {
			ok++
		} else {
			lost++
			if lost <= 2 {
				t.Logf("ZZPD /ok %d: %q err=%v", i, r, err)
			}
		}
	}
	t.Logf("ZZPD after /boom: answered=%d lost=%d", ok, lost)
	cancel()
	stopped := false
	select {
	case <-done:
		stopped = true
	case <-time.After(3 * time.Second):
	}
	t.Logf("ZZPD engine stopped within 3s of cancel: %v", stopped)
	if lost > 0 || !stopped || len(blocked) > 0 {
		t.Fatalf("worker wedged after /boom (mode=%s): %d of 16 lost, stopped=%v, blocked goroutines=%d", mode, lost, stopped, len(blocked))
	}
}

func TestZZPanicDetachMuAsyncPanic(t *testing.T)  { zzPDRun(t, "panic", true) }
func TestZZPanicDetachMuAsyncError(t *testing.T)  { zzPDRun(t, "error", true) }
func TestZZPanicDetachMuAsyncGoexit(t *testing.T) { zzPDRun(t, "goexit", true) }

// The sync arm: the same panic on a route that runs inline on the worker. The
// engine has no recover on that path (celeris.Server's routerAdapter owns it),
// so the process is expected to die; run it in a child and report how.
func TestZZPanicDetachMuSyncChild(t *testing.T) {
	if os.Getenv("ZZ_PD_CHILD") != "1" {
		t.Skip("child only")
	}
	zzPDRun(t, "panic", false)
}

func TestZZPanicDetachMuSyncPanic(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestZZPanicDetachMuSyncChild$", "-test.v", "-test.timeout=120s")
	cmd.Env = append(os.Environ(), "ZZ_PD_CHILD=1")
	out, err := cmd.CombinedOutput()
	s := string(out)
	exit := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		exit = ee.ExitCode()
	} else if err == nil {
		exit = 0
	}
	died := strings.Contains(s, "panic: zz handler panic")
	lines := strings.Split(s, "\n")
	for _, l := range lines {
		if strings.Contains(l, "ZZPD") || strings.HasPrefix(l, "panic:") || strings.HasPrefix(l, "--- ") ||
			strings.HasPrefix(l, "goroutine ") && strings.Contains(l, "running") {
			t.Logf("child: %s", l)
		}
	}
	for i, l := range lines {
		if strings.HasPrefix(l, "panic: zz handler panic") {
			end := min(i+40, len(lines))
			t.Logf("child panic stack:\n%s", strings.Join(lines[i:end], "\n"))
			break
		}
	}
	t.Logf("ZZPD sync child exit=%d process_died_with_handler_panic=%v", exit, died)
}
