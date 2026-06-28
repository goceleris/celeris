//go:build linux

package iouring

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"runtime"
	"runtime/debug"
	"strings"
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
// was hardened to fix the SIGSEGV observed in the v1.4.1 strict matrix
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
// the gold-standard regression detection is the adversarial cluster
// matrix in goceleris/probatorium (churn-close × celeris-iouring-*-async
// cells) and the standalone loadgen reproducer it ships.
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
	// Skip on resource-constrained runners that can't allocate the io_uring
	// rings: the test exercises a regression that requires a live engine,
	// and there's no point reporting it as a celeris failure when the
	// kernel said "cannot allocate memory" before our code even ran.
	dl := time.Now().Add(30 * time.Second)
	for time.Now().Before(dl) && e.Addr() == nil {
		select {
		case err := <-errCh:
			if err != nil && (strings.Contains(err.Error(), "cannot allocate memory") ||
				strings.Contains(err.Error(), "io_uring_setup") ||
				strings.Contains(err.Error(), "tier")) {
				t.Skipf("io_uring unavailable on this runner: %v", err)
			}
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

// TestAsyncChurnCloseWithStragglerData is the regression test for the
// v1.4.15/7beebb9 bench heap corruption: fatal "s.allocCount !=
// s.nelems" ~10-13 min into the bench POST cell, twice, on
// iouring-h1-async × chain-api.
//
// Root cause: every async H1 conn has a single-shot recv SQE armed into
// cs.buf (a Go-heap array). The close paths closed the fd WITHOUT
// cancelling that recv — unix.Close does not complete a pending io_uring
// recv — and held the connState only pendingReleaseHoldNanos = 100 ms
// before it became GC-garbage. TCP's RTO_MIN is 200 ms, so a
// retransmitted / straggler POST segment landed AFTER the release and the
// kernel DMA'd the payload into freed heap pages; on Go 1.26 (Green Tea
// GC keeps alloc/mark bits inside the span) that deterministically
// corrupts span metadata.
//
// The fix (cancel-then-release): close paths ASYNC_CANCEL the armed recv
// by its generation-tagged user_data, and the connState is not released
// until every kernel-held op delivers its terminal CQE (per-conn
// kernelInflight counter, the driverConn.inflightOps pattern), with the
// wall-clock hold (raised to 5 s) demoted to an anomaly backstop. The
// precise release-gating assertions live in pending_release_test.go;
// this test drives the real engine through the trigger sequence: POST +
// Connection: close churn where the peer writes MORE data after the
// server initiated the close, on delays straddling RTO_MIN, under
// aggressive GC so any released-too-early cs.buf gets reclaimed inside
// the straggler window. On HEAD~ (no cancel, 100 ms hold) this crashes
// the runtime; with the fix it runs clean.
//
// Gated on testing.Short() like TestAsyncChurnNoUseAfterFree.
func TestAsyncChurnCloseWithStragglerData(t *testing.T) {
	if testing.Short() {
		t.Skip("straggler-data UAF regression test requires sustained churn; -short skips it")
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

	dl := time.Now().Add(30 * time.Second)
	for time.Now().Before(dl) && e.Addr() == nil {
		select {
		case err := <-errCh:
			if err != nil && (strings.Contains(err.Error(), "cannot allocate memory") ||
				strings.Contains(err.Error(), "io_uring_setup") ||
				strings.Contains(err.Error(), "tier")) {
				t.Skipf("io_uring unavailable on this runner: %v", err)
			}
			t.Fatalf("engine.Listen returned early: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind in time")
	}
	target := e.Addr().String()

	const (
		concurrency = 128
		duration    = 30 * time.Second
	)

	// Same GC hammer as TestAsyncChurnNoUseAfterFree: compress the
	// (release) → (reclaim) → (kernel write) window from hours to seconds.
	prevGC := debug.SetGCPercent(1)
	defer debug.SetGCPercent(prevGC)
	gcStop := make(chan struct{})
	go func() {
		tk := time.NewTicker(500 * time.Microsecond)
		defer tk.Stop()
		for {
			select {
			case <-gcStop:
				return
			case <-tk.C:
				runtime.GC()
			}
		}
	}()
	defer close(gcStop)

	// Straggler delays straddle the old 100 ms hold and TCP RTO_MIN
	// (200 ms): 0 exercises the immediate-straggler race with the cancel,
	// the ≥120 ms entries land squarely after the old release point.
	stragglerDelays := []time.Duration{
		0, 50 * time.Millisecond, 120 * time.Millisecond, 250 * time.Millisecond,
	}

	deadline := time.Now().Add(duration)
	var totalOK atomic.Int64
	var totalFailed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for n := 0; time.Now().Before(deadline); n++ {
				delay := stragglerDelays[(id+n)%len(stragglerDelays)]
				if err := doOnePostWithStraggler(target, 2*time.Second, delay); err != nil {
					totalFailed.Add(1)
				} else {
					totalOK.Add(1)
				}
			}
		}(i)
	}
	wg.Wait()

	// The dominant signal is "did the server crash": under the UAF the
	// runtime aborts (allocCount fatal / stackalloc SIGSEGV) and the whole
	// test binary dies. Request failures under GC pressure are normal.
	totalReq := totalOK.Load() + totalFailed.Load()
	if totalReq < 500 {
		t.Fatalf("too few requests completed (ok=%d failed=%d) — server may have crashed early",
			totalOK.Load(), totalFailed.Load())
	}
	// Liveness epilogue: the engine must still serve a plain request.
	var liveErr error
	for attempt := 0; attempt < 5; attempt++ {
		if liveErr = doOneRequest(target, 2*time.Second); liveErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if liveErr != nil {
		t.Fatalf("engine not serving after straggler churn: %v", liveErr)
	}
	t.Logf("ok=%d failed=%d (%.2f%% pass) over %s",
		totalOK.Load(), totalFailed.Load(),
		100*float64(totalOK.Load())/float64(totalReq), duration)
}

// doOnePostWithStraggler drives the v1.4.15/7beebb9 corruption trigger sequence: a 4 KiB POST
// with Connection: close, full response read (the server initiates close
// once the response is flushed — at which point a single-shot recv SQE
// was armed on the conn), then — after delay — MORE bytes written into
// the closed conn. Without cancel-then-release, those bytes complete the
// stale recv and the kernel writes them wherever cs.buf's memory went.
// Write errors on the straggler are expected (RST once the server's FIN
// is followed by our data) and ignored; only the request/response part
// reports failure.
func doOnePostWithStraggler(target string, timeout, delay time.Duration) error {
	c, err := net.DialTimeout("tcp", target, timeout)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(timeout + delay))

	body := make([]byte, 4096)
	for i := range body {
		body[i] = 'x'
	}
	req := fmt.Sprintf("POST /chain HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(body))
	if _, err := c.Write(append([]byte(req), body...)); err != nil {
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
	respBody := make([]byte, 2)
	if _, err := br.Read(respBody); err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	// Server has flushed the response and is tearing the conn down.
	// Straggle: write another 4 KiB after the delay. This is the
	// retransmitted/late-segment stand-in — errors are expected and fine.
	if delay > 0 {
		time.Sleep(delay)
	}
	_, _ = c.Write(body)
	return nil
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
