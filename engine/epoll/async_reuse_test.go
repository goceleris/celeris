//go:build linux

package epoll

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// benchHandlerStream returns a tiny canned response — matches the handler
// shape the test needs without pulling in the full router machinery.
type asyncReuseHandler struct{}

func (h *asyncReuseHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}},
		[]byte("ok"))
}

// startAsyncEngine spins up an epoll engine with AsyncHandlers=true on a
// random port and returns the bound address plus a cleanup func.
func startAsyncEngine(t *testing.T) (string, func()) {
	t.Helper()
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
			Workers: 2, // Config.Validate requires Workers >= 2 when set
		},
		AsyncHandlers: true,
	}
	e, err := New(cfg, &asyncReuseHandler{})
	if err != nil {
		t.Skipf("epoll engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()

	// Wait until listener is up.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a := e.Addr(); a != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		cancel()
		<-errCh
		t.Fatal("engine did not bind in time")
	}
	return e.Addr().String(), func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
		}
	}
}

// TestAsyncHandlerGoroutineReuse asserts that the async dispatch
// goroutine stays parked across keep-alive requests instead of
// spawning a fresh goroutine per batch. Proof: the Go goroutine
// count after N serial requests on one conn with gaps >= Cond wake
// latency should grow by ~1 (one dispatch G for the conn), not by N.
//
// Regression guard for the sync.Cond-based reuse model. If runAsyncHandler
// reverts to spawning per-batch, the delta here climbs to N-ish.
func TestAsyncHandlerGoroutineReuse(t *testing.T) {
	addr, cleanup := startAsyncEngine(t)
	defer cleanup()

	// Probe the engine is reachable.
	c0, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial probe: %v", err)
	}
	_, _ = c0.Write([]byte("GET /ping HTTP/1.1\r\nHost: x\r\n\r\n"))
	br0 := bufio.NewReader(c0)
	if _, err := br0.ReadString('\n'); err != nil {
		_ = c0.Close()
		t.Fatalf("probe read: %v", err)
	}
	_ = c0.Close()

	// Open the keep-alive conn we will exercise.
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	// First request to establish the dispatch goroutine.
	if _, err := conn.Write([]byte("GET /warm HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("warm write: %v", err)
	}
	br := bufio.NewReader(conn)
	if err := discardResponse(br); err != nil {
		t.Fatalf("warm read: %v", err)
	}

	// Let the dispatch goroutine park on asyncCond.Wait before sampling.
	time.Sleep(20 * time.Millisecond)
	runtime.GC()
	baseline := runtime.NumGoroutine()

	const N = 100
	for i := 0; i < N; i++ {
		path := fmt.Sprintf("/r%d", i)
		req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: x\r\n\r\n", path)
		if _, err := conn.Write([]byte(req)); err != nil {
			t.Fatalf("write req %d: %v", i, err)
		}
		if err := discardResponse(br); err != nil {
			t.Fatalf("read req %d: %v", i, err)
		}
		// Gap > Cond.Wait wake latency so the goroutine parks
		// between requests; this is the case the reuse fix targets.
		time.Sleep(500 * time.Microsecond)
	}

	time.Sleep(20 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()

	// Tolerance: allow a few extra goroutines for go runtime internals
	// (finalizer, GC mark workers), but not N of them. The old
	// spawn-per-batch implementation would have left this delta >> 10
	// transiently, often nonzero even after GC because some of the
	// per-batch Gs are still in the scheduler queue when we sample.
	if delta := after - baseline; delta > 5 {
		t.Fatalf("goroutine count grew by %d after %d requests; expected reuse (delta ≤ 5)", delta, N)
	}
}

// TestAsyncHandlerCloseWakesGoroutine asserts that closing the conn
// wakes the parked dispatch goroutine via asyncCond.Broadcast. If the
// broadcast is missing, the goroutine leaks indefinitely.
func TestAsyncHandlerCloseWakesGoroutine(t *testing.T) {
	addr, cleanup := startAsyncEngine(t)
	defer cleanup()

	before := runtime.NumGoroutine()

	// Open, send one request, close.
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		_ = conn.Close()
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(conn)
	if err := discardResponse(br); err != nil {
		_ = conn.Close()
		t.Fatalf("read: %v", err)
	}
	_ = conn.Close()

	// Give the engine time to observe the close, tear down the conn,
	// broadcast the Cond, and let the dispatch goroutine exit.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if after := runtime.NumGoroutine(); after <= before+2 {
			return // goroutine released back under tolerance
		}
		time.Sleep(50 * time.Millisecond)
	}
	after := runtime.NumGoroutine()
	t.Fatalf("dispatch goroutine leaked after close: before=%d after=%d", before, after)
}

// discardResponse reads one complete HTTP/1.1 response (headers + body
// up to Content-Length) off br. Sufficient for the canned "ok" bodies
// the handler returns.
func discardResponse(br *bufio.Reader) error {
	var cl int
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			_, _ = fmt.Sscanf(line[len("content-length:"):], "%d", &cl)
		}
	}
	if cl == 0 {
		return nil
	}
	buf := make([]byte, cl)
	for off := 0; off < cl; {
		n, err := br.Read(buf[off:])
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			return err
		}
		off += n
	}
	return nil
}
