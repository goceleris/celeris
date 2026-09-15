//go:build linux

package iouring

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// revertResolverHandler is an AsyncRouteResolver whose async classification is
// flipped at runtime, simulating celeris#356 route promotion (true) and the
// celeris#364 TTL de-promotion (false) without waiting on the real router TTL.
type revertResolverHandler struct{ async atomic.Bool }

func (h *revertResolverHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}
func (h *revertResolverHandler) RouteAsync(_, _ string) bool { return h.async.Load() }
func (h *revertResolverHandler) HasAsyncRoutes() bool        { return true }

func startRevertEngine(t *testing.T, h stream.Handler) *Engine {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	e, err := New(resource.Config{
		Addr:          addr,
		Protocol:      engine.HTTP1,
		Resources:     resource.Resources{Workers: 4},
		AsyncHandlers: true,
	}, h)
	if err != nil {
		t.Skipf("iouring engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
		}
	})
	dl := time.Now().Add(30 * time.Second)
	for time.Now().Before(dl) && e.Addr() == nil {
		select {
		case err := <-errCh:
			if err != nil && (strings.Contains(err.Error(), "cannot allocate memory") ||
				strings.Contains(err.Error(), "io_uring_setup") || strings.Contains(err.Error(), "tier")) {
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
	return e
}

// sendKeepAlive issues one keep-alive GET on an existing conn and reads the
// full 2-byte "ok" response, leaving the conn open for the next request.
func sendKeepAlive(c net.Conn, br *bufio.Reader, timeout time.Duration) error {
	_ = c.SetDeadline(time.Now().Add(timeout))
	if _, err := c.Write([]byte("GET /x HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		return fmt.Errorf("write: %w", err)
	}
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
	body := make([]byte, 2)
	if _, err := io.ReadFull(br, body); err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if string(body) != "ok" {
		return fmt.Errorf("bad body: %q", body)
	}
	return nil
}

// TestAsyncConnRevertsOnRouteDepromotion is the celeris#364 conn-level revert
// regression: a keep-alive conn promoted to async dispatch must return to the
// inline fast path once its route de-promotes (RouteAsync flips false), proven
// by its ability to RE-promote afterwards (a still-async conn cannot re-promote
// — it never re-runs the inline ErrAsyncDispatch gate).
func TestAsyncConnRevertsOnRouteDepromotion(t *testing.T) {
	h := &revertResolverHandler{}
	h.async.Store(true)
	e := startRevertEngine(t, h)
	target := e.Addr().String()

	c, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	send := func(phase string) {
		if err := sendKeepAlive(c, br, 2*time.Second); err != nil {
			t.Fatalf("%s request failed: %v", phase, err)
		}
	}

	// Phase A — RouteAsync=true: the conn promotes to async dispatch.
	for i := 0; i < 20; i++ {
		send("promote")
	}
	p1 := e.Metrics().AsyncPromotedConns
	if p1 == 0 {
		t.Fatal("conn never promoted under RouteAsync=true")
	}

	// Phase B — RouteAsync=false: the dispatch goroutine must revert the conn
	// to inline at its next idle point. Requests must keep succeeding.
	h.async.Store(false)
	for i := 0; i < 30; i++ {
		send("revert")
		time.Sleep(time.Millisecond)
	}

	// Phase C — RouteAsync=true again: a reverted (now inline) conn re-promotes;
	// a conn still stuck async would NOT, so AsyncPromotedConns must increase.
	h.async.Store(true)
	for i := 0; i < 30; i++ {
		send("repromote")
	}
	p2 := e.Metrics().AsyncPromotedConns
	if p2 <= p1 {
		t.Fatalf("conn did not revert+re-promote (still stuck async): promoted before=%d after=%d", p1, p2)
	}
	t.Logf("promotions: after-A=%d after-C=%d (revert confirmed by re-promotion)", p1, p2)
}

// TestAsyncConnRevertRace hammers the promote/feed/revert/re-promote interaction
// across many keep-alive conns with RouteAsync flipping continuously. Run under
// -race it validates that the worker recv path and the dispatch goroutine never
// race on cs.asyncPromoted / asyncInBuf during a revert. Gated on -short.
func TestAsyncConnRevertRace(t *testing.T) {
	if testing.Short() {
		t.Skip("revert race test needs sustained toggling load; -short skips it")
	}
	h := &revertResolverHandler{}
	h.async.Store(true)
	e := startRevertEngine(t, h)
	target := e.Addr().String()

	const (
		concurrency = 64
		duration    = 10 * time.Second
	)
	stop := make(chan struct{})
	// Toggler: flip the route's async classification fast enough that conns are
	// constantly promoting and reverting.
	go func() {
		tk := time.NewTicker(3 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				h.async.Store(!h.async.Load())
			}
		}
	}()

	deadline := time.Now().Add(duration)
	var ok, failed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.DialTimeout("tcp", target, 2*time.Second)
			if err != nil {
				failed.Add(1)
				return
			}
			defer func() { _ = c.Close() }()
			br := bufio.NewReader(c)
			for time.Now().Before(deadline) {
				if err := sendKeepAlive(c, br, 2*time.Second); err != nil {
					failed.Add(1)
					return // conn broken; a corrupted response would surface here
				}
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	close(stop)

	if ok.Load() < 1000 {
		t.Fatalf("too few successful requests (ok=%d failed=%d) — server may have stalled", ok.Load(), failed.Load())
	}
	// Liveness epilogue.
	h.async.Store(false)
	c, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		t.Fatalf("engine not serving after revert churn: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := sendKeepAlive(c, bufio.NewReader(c), 2*time.Second); err != nil {
		t.Fatalf("engine not serving after revert churn: %v", err)
	}
	t.Logf("ok=%d failed=%d over %s", ok.Load(), failed.Load(), duration)
}

// routeTableResolver is an AsyncRouteResolver with a REAL route table: it
// answers from the path it is HANDED, and records every path it was asked
// about. revertResolverHandler above cannot do that — its RouteAsync ignores
// both arguments — which is why the celeris#364 revert test could not see
// celeris#631: the value the revert decision is made on never reached its
// oracle.
type routeTableResolver struct {
	mu     sync.Mutex
	routes map[string]bool // path → async
	seen   map[string]int  // path → times RouteAsync was asked about it
}

func newRouteTableResolver(routes map[string]bool) *routeTableResolver {
	return &routeTableResolver{routes: routes, seen: map[string]int{}}
}

func (h *routeTableResolver) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}

func (h *routeTableResolver) RouteAsync(_, path string) bool {
	h.mu.Lock()
	// CLONED before it is recorded, for the same reason the engine has to
	// clone it (celeris#631): the path handed to a resolver is a zero-copy
	// view into the caller's buffer and is only valid for this call. A map
	// key is a string HEADER, not a copy of the bytes, so recording the
	// argument as-is makes every key mutate under the map when the buffer is
	// reused — the first cut of this oracle did exactly that and reported
	// map[/async:10] for ten calls that were not all about /async.
	h.seen[strings.Clone(path)]++
	async := h.routes[path]
	h.mu.Unlock()
	return async
}

func (h *routeTableResolver) HasAsyncRoutes() bool { return true }

// seenPaths returns every path the resolver was asked about, with counts.
func (h *routeTableResolver) seenPaths() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]int{}
	for p, n := range h.seen {
		out[p] = n
	}
	return out
}

// unknownPaths returns every path the resolver was asked about that is not in
// its route table — i.e. every path the engine invented.
func (h *routeTableResolver) unknownPaths() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]int{}
	for p, n := range h.seen {
		if _, ok := h.routes[p]; !ok {
			out[p] = n
		}
	}
	return out
}

// sendKeepAlivePath is sendKeepAlive with the request target as a parameter,
// so one conn can alternate between an async and a sync route.
func sendKeepAlivePath(c net.Conn, br *bufio.Reader, path string, timeout time.Duration) error {
	_ = c.SetDeadline(time.Now().Add(timeout))
	if _, err := c.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		return fmt.Errorf("write: %w", err)
	}
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
	body := make([]byte, 2)
	if _, err := io.ReadFull(br, body); err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if string(body) != "ok" {
		return fmt.Errorf("bad body: %q", body)
	}
	return nil
}

// TestAsyncConnPromotedRouteSurvivesTheNextRequest is the celeris#631
// regression: the route recorded at promotion (cs.promotedMethod /
// cs.promotedPath, the celeris#364 revert's only input) must still say what
// it said when it was written, after later requests have reused the parser
// buffer it was read from.
//
// h1.Request.Path is an UnsafeString view into that buffer (internPath,
// "safe because H1 handlers run synchronously before the buffer is reused"),
// so storing it on the conn outlives its validity. Measured before the fix on
// one keep-alive conn alternating /async and /sync: the conn promoted on
// "/async", and at the dispatch goroutine's next park its promotedPath read
// back as the first six bytes of the "/sync" request line. RouteAsync was
// then asked about a path that was never registered, said false, and the conn
// was de-promoted and re-promoted once per request pair.
//
// Two oracles, both on values the engine itself produced: no path outside the
// route table ever reaches the resolver, and the conn is promoted exactly
// once.
func TestAsyncConnPromotedRouteSurvivesTheNextRequest(t *testing.T) {
	h := newRouteTableResolver(map[string]bool{"/async": true, "/sync": false})
	e := startRevertEngine(t, h)

	// The celeris#364 revert this guards only runs on the single-shot recv
	// model (canRevertToInline gates on w.bufRing == nil). With provided
	// buffers the revert never fires, promotedPath is never even recorded,
	// and the assertions below would hold vacuously — so say so instead.
	e.mu.Lock()
	workers := append([]*Worker(nil), e.workers...)
	e.mu.Unlock()
	if len(workers) == 0 {
		t.Fatal("engine reports no workers")
	}
	for _, w := range workers {
		if w.bufRing != nil {
			t.Skip("provided-buffer recv in use: the celeris#364 revert path (and so the celeris#631 alias) is not reachable here")
		}
	}

	c, err := net.DialTimeout("tcp", e.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)

	if err := sendKeepAlivePath(c, br, "/async", 2*time.Second); err != nil {
		t.Fatalf("promote request failed: %v", err)
	}
	// Give the dispatch goroutine time to park at least once, which is where
	// the revert decision is taken.
	time.Sleep(50 * time.Millisecond)
	promoted := e.Metrics().AsyncPromotedConns
	if promoted != 1 {
		t.Fatalf("conn must promote exactly once on the async route: AsyncPromotedConns=%d", promoted)
	}

	for i := 0; i < 10; i++ {
		for _, p := range []string{"/sync", "/async"} {
			if err := sendKeepAlivePath(c, br, p, 2*time.Second); err != nil {
				t.Fatalf("request %d %s failed: %v", i, p, err)
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	t.Logf("RouteAsync was asked about %v", h.seenPaths())
	if unknown := h.unknownPaths(); len(unknown) > 0 {
		t.Errorf("RouteAsync was asked about %v, none of which is a registered route: the promoting route was read back "+
			"from a reused parser buffer (celeris#631)", unknown)
	}
	if got := e.Metrics().AsyncPromotedConns; got != 1 {
		t.Errorf("conn re-promoted %d times over 21 requests on ONE conn (want 1): the celeris#364 revert fired on a route "+
			"that is still async", got-1)
	}
}
