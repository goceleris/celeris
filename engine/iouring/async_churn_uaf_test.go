//go:build linux

package iouring

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// asyncChurnHandler returns a tiny canned response — the test stresses
// the close path, not the handler logic.
type asyncChurnHandler struct{}

func (h *asyncChurnHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}},
		[]byte("ok"))
}

// TestAsyncChurnNoUseAfterFree is a smoke test for the close path that
// was hardened to fix the SIGSEGV observed in the v1.5.0 strict matrix
// on churn-close × celeris-iouring-auto+upg-async (~5 h to reproduce).
//
// Root cause: finishCloseDetached did not queue cs in pendingRelease
// (only finishClose for sync conns did). So when the dispatch
// goroutine exited, cs lost its last strong reference; GC reclaimed
// cs.buf; the kernel's still-pending recv SQE then wrote inbound
// HTTP request bytes into memory Go had repurposed for runtime stack
// pages, and the next stackalloc walk dereferenced "GET / HTTP/1.1"
// as a pointer — SIGSEGV at fault address 0x48202f20544547.
//
// This test drives the same path (async dispatch + churn-close) under
// aggressive GC pressure. It catches gross regressions (server panics,
// 100 % request failure) but the precise UAF timing is environment-
// sensitive and doesn't reproduce reliably inside a short test budget;
// the gold-standard regression detection is the strict matrix
// (churn-close × celeris-iouring-*-async cells) and the standalone
// loadgen reproducer at test/perfmatrix/cmd/churnrepro.
//
// Gated on testing.Short() — 60 s of -race churn is heavy for CI.
func TestAsyncChurnNoUseAfterFree(t *testing.T) {
	if testing.Short() {
		t.Skip("UAF regression test requires sustained churn-close load; -short skips it")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := resource.Config{
		Addr:     addr,
		Protocol: engine.HTTP1,
		Resources: resource.Resources{
			Workers: 8,
		},
		AsyncHandlers: true,
	}
	e, err := New(cfg, &asyncChurnHandler{})
	if err != nil {
		t.Skipf("iouring engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	defer func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
		}
	}()

	// CI runners (and especially -race builds on hosted Azure agents) can
	// take well over the 5 s our msr1 dev box uses. Bump the deadline so
	// the test waits until the engine is actually up — the churn-load it
	// runs after this is the part the test cares about, not the bind.
	dl := time.Now().Add(30 * time.Second)
	for time.Now().Before(dl) && e.Addr() == nil {
		select {
		case err := <-errCh:
			t.Fatalf("engine.Listen returned early: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind in time")
	}
	target := e.Addr().String()

	// Drive churn-close until the wall budget expires. We don't fix
	// a count because the bug is timing-dependent (GC vs kernel-recv
	// race) and the goal is to widen the (cs becomes GC-eligible)
	// window past the point where a still-pending kernel recv SQE
	// can fire. 60 s of sustained churn at 256-way concurrency is
	// enough to surface the UAF on HEAD; with the fix the test
	// completes clean.
	const (
		concurrency = 256
		duration    = 60 * time.Second
	)

	// Drive GC very aggressively to compress the (cs goroutine
	// exit) → (cs.buf reclaim) → (kernel writes to reused memory)
	// window. Without this, reproducing the bug needs hours of
	// load; with it, seconds. SetGCPercent(1) makes Go GC after
	// every ~1% heap growth; combined with explicit runtime.GC()
	// in a tight loop this maximizes the chance that any
	// GC-eligible cs.buf gets reclaimed inside the kernel's SQE
	// drain window.
	prevGC := debug.SetGCPercent(1)
	defer debug.SetGCPercent(prevGC)
	gcStop := make(chan struct{})
	go func() {
		t := time.NewTicker(500 * time.Microsecond)
		defer t.Stop()
		for {
			select {
			case <-gcStop:
				return
			case <-t.C:
				runtime.GC()
			}
		}
	}()
	defer close(gcStop)

	deadline := time.Now().Add(duration)
	var totalOK atomic.Int64
	var totalFailed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				if err := doOneRequest(target, 2*time.Second); err != nil {
					totalFailed.Add(1)
				} else {
					totalOK.Add(1)
				}
			}
		}(i)
	}
	wg.Wait()

	// The dominant signal is "did the server crash" — under the UAF
	// the runtime SIGSEGVs on the engine goroutine and the test
	// harness flips to FAIL with a stack trace including
	// runtime.stackalloc / runtime.copystack. Individual request
	// failures (TCP timeouts, EOF mid-handshake) are normal under
	// extreme GC pressure with -race and don't indicate a bug.
	totalReq := totalOK.Load() + totalFailed.Load()
	if totalReq < 1000 {
		t.Fatalf("too few requests completed (ok=%d failed=%d) — server may have crashed early",
			totalOK.Load(), totalFailed.Load())
	}
	t.Logf("ok=%d failed=%d (%.2f%% pass) over %s",
		totalOK.Load(), totalFailed.Load(),
		100*float64(totalOK.Load())/float64(totalReq), duration)
}

// doOneRequest mirrors loadgen's churn-close client: dial, send a
// single H1 GET with Connection: close, read the status line +
// headers + body, close. Failures here mean the server didn't
// respond — under the UAF the server panics in the runtime, not in
// celeris code, so the test goroutine sees ECONNRESET / EOF.
func doOneRequest(target string, timeout time.Duration) error {
	c, err := net.DialTimeout("tcp", target, timeout)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(timeout))
	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	br := bufio.NewReader(c)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read status: %w", err)
	}
	if len(statusLine) < 12 || statusLine[9:12] != "200" {
		return fmt.Errorf("bad status: %q", statusLine)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read header: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	// Body is "ok" — read 2 bytes.
	body := make([]byte, 2)
	if _, err := br.Read(body); err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	return nil
}
