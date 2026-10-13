//go:build linux

package celeris_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/internal/engine/iouring"
	"github.com/goceleris/celeris/internal/probe"
)

// The io_uring engine is refused where the kernel rejects the
// IORING_ASYNC_CANCEL flags (every kernel before Linux 5.19, celeris#682). CI
// runs on a kernel that accepts them, so nothing there reaches the refusal:
// these tests force the probe's answer (iouring.ForceAsyncCancelProbe) and
// check what a Server does with it (celeris#886 item 3).

// refusedProbe886 forces the rejection for one test. It skips (fails under
// CELERIS_REQUIRE_IOURING_WORKERS=1, the repo's pattern for tests that need
// io_uring) when the probed tier is none: New refuses io_uring before it
// probes there, and the test would pass without the forced answer.
func refusedProbe886(t *testing.T) {
	t.Helper()
	if p := probe.Probe(); !p.IOUringTier.Available() {
		if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
			t.Fatalf("io_uring tier=%s kernel=%s: the async-cancel refusal is never reached, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids skipping", p.IOUringTier, p.KernelVersion)
		}
		t.Skipf("io_uring tier=%s kernel=%s: New refuses io_uring before the async-cancel probe", p.IOUringTier, p.KernelVersion)
	}
	t.Cleanup(iouring.ForceAsyncCancelProbe(iouring.AsyncCancelRejected,
		"IORING_ASYNC_CANCEL flags rejected: cqe.res=-22 (EINVAL); forced by the test"))
}

type lockedBuf886 struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf886) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf886) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestServerIOUringFailsToStartWhenAsyncCancelRejected886: Engine: IOUring on
// a kernel that rejects the flags fails at Start, with the io_uring engine's
// "not available" error naming the 5.19 requirement, and binds nothing. The
// start runs under a deadline: a build that succeeded would serve until
// Shutdown.
func TestServerIOUringFailsToStartWhenAsyncCancelRejected886(t *testing.T) {
	refusedProbe886(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	var logs lockedBuf886
	s := celeris.New(celeris.Config{
		Engine: celeris.IOUring,
		Addr:   addr,
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.StartWithContext(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Start returned nil for an io_uring engine the kernel's probe refused: want an error")
		}
		msg := err.Error()
		t.Logf("celeris886 Engine: IOUring Start error: %s", msg)
		if !strings.Contains(msg, "io_uring not available on this system") || !strings.Contains(msg, "5.19") {
			t.Errorf("Start error = %q, want the io_uring engine's \"not available\" refusal naming Linux 5.19", msg)
		}
	case <-time.After(5 * time.Second):
		cancel()
		_ = s.Shutdown(context.Background())
		<-done
		t.Fatal("Start did not return within 5s: the io_uring engine was built although the probe rejected the IORING_ASYNC_CANCEL flags (celeris#682)")
	}
	if a := s.Addr(); a != nil {
		t.Errorf("the failed Start left the server addressed at %v", a)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = c.Close()
		t.Errorf("%s accepts a connection after the failed Start", addr)
	}
}

// TestServerAdaptiveStartsOnEpollWhenIOUringRefused886: Engine: Adaptive with
// CELERIS_ADAPTIVE_START=iouring on a kernel that rejects the flags starts on
// epoll rather than failing, warns with the refusal, and serves.
func TestServerAdaptiveStartsOnEpollWhenIOUringRefused886(t *testing.T) {
	refusedProbe886(t)
	t.Setenv("CELERIS_ADAPTIVE_START", "iouring")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	var logs lockedBuf886
	s := celeris.New(celeris.Config{
		Engine:  celeris.Adaptive,
		Addr:    addr,
		Workers: 2,
		Logger:  slog.New(slog.NewTextHandler(&logs, nil)),
	})
	s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.StartWithContext(ctx) }()
	defer func() {
		cancel()
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		_ = s.Shutdown(sctx)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Start did not return after Shutdown")
		}
	}()

	client := &http.Client{Timeout: 300 * time.Millisecond}
	if !waitReady(t, client, "http://"+addr+"/ping", done) {
		t.Fatalf("adaptive server with a refused io_uring start engine never became ready on %s; log:\n%s", addr, logs.String())
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + "/ping")
	if err != nil {
		t.Fatalf("GET /ping: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("GET /ping = %d %q, want 200 \"ok\"", resp.StatusCode, body)
	}
	out := logs.String()
	if !strings.Contains(out, "io_uring start engine unavailable, falling back to epoll start") ||
		!strings.Contains(out, "io_uring not available on this system") {
		t.Errorf("no fallback warning naming the refusal in the log:\n%s", out)
	}
}
