//go:build linux

package epoll

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// A loop with async dispatch on (Config.AsyncHandlers, or any route marked
// Async) runs a connection's requests inline, in InlineMode, until one of
// them reaches an async route. A handler that runs inline there and calls
// Hijack takes hijackConn's inline branch, which returns the connState to the
// pool inside the Hijack call, h1State included (nil after release). drainRead
// then cleared InlineMode through cs.h1State, before it looked at
// ErrHijacked: a nil dereference on the loop goroutine, which took the whole
// process down on the first such request. On a loop whose released connState
// had already been taken by another worker's accept, the same line wrote
// that other connection's parser state.

// inlineHijackHandler: /hj hijacks and answers on the raw conn, /async is the
// async route (when asyncRoutes is set), anything else answers normally.
type inlineHijackHandler struct {
	asyncRoutes bool
	hijacked    *atomic.Int64
}

func (h inlineHijackHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	if s.Path != "/hj" {
		return s.ResponseWriter.WriteResponse(s, 200,
			[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
	}
	hj, ok := s.ResponseWriter.(stream.Hijacker)
	if !ok {
		return fmt.Errorf("response writer %T cannot hijack", s.ResponseWriter)
	}
	c, err := hj.Hijack(s)
	if err != nil {
		return err
	}
	h.hijacked.Add(1)
	_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhj")
	return c.Close()
}
func (h inlineHijackHandler) RouteAsync(_, path string) bool {
	return h.asyncRoutes && path == "/async"
}
func (h inlineHijackHandler) HasAsyncRoutes() bool { return h.asyncRoutes }

// startInlineHijackEngine starts an epoll engine with AsyncHandlers on and
// the given number of workers, and waits for it to bind. stop cancels it and
// waits for Listen to return; it is safe to call twice, and reports whether
// the engine stopped within 5 s. A test that reads state the loop wrote does
// so after stop has returned true, which is what orders those writes before
// the reads.
func startInlineHijackEngine(t *testing.T, workers int, h stream.Handler) (e *Engine, addr string, stop func() bool) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr = ln.Addr().String()
	_ = ln.Close()
	e, err = New(resource.Config{
		Addr:          addr,
		Protocol:      engine.HTTP1,
		Resources:     resource.Resources{Workers: workers},
		AsyncHandlers: true,
	}, h)
	if err != nil {
		t.Fatalf("epoll engine: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	var once sync.Once
	var stopped bool
	stop = func() bool {
		once.Do(func() {
			cancel()
			select {
			case <-errCh:
				stopped = true
			case <-time.After(5 * time.Second):
			}
		})
		return stopped
	}
	t.Cleanup(func() { stop() })
	for dl := time.Now().Add(10 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind")
	}
	if !e.loops[0].async {
		t.Fatal("precondition: the loop is not in async mode")
	}
	return e, addr, stop
}

// inlineHijackRequest sends req on a new connection to addr and returns the
// response body.
func inlineHijackRequest(addr, req string) (string, error) {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, req); err != nil {
		return "", err
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return string(b), err
}

func TestInlineHijackOnAsyncLoop(t *testing.T) {
	for _, tc := range []struct {
		name        string
		asyncRoutes bool
	}{
		// Config.AsyncHandlers with no route marked: no resolver is wired,
		// and every request runs inline in InlineMode.
		{"async-loop-no-async-routes", false},
		// A server with one Async route: /hj is a sync route, run inline.
		{"async-loop-sync-route", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hijacked atomic.Int64
			e, addr, _ := startInlineHijackEngine(t, 2, inlineHijackHandler{asyncRoutes: tc.asyncRoutes, hijacked: &hijacked})

			get := func(path string) (string, error) {
				return inlineHijackRequest(addr, fmt.Sprintf("GET %s HTTP/1.1\r\nHost: x\r\n\r\n", path))
			}

			const n = 20
			for i := 0; i < n; i++ {
				if got, err := get("/hj"); err != nil || got != "hj" {
					t.Fatalf("hijack %d: body %q, err %v", i, got, err)
				}
				if got, err := get("/ok"); err != nil || got != "ok" {
					t.Fatalf("request after hijack %d: body %q, err %v", i, got, err)
				}
			}
			if got := hijacked.Load(); got != n {
				t.Errorf("hijacked %d connections, want %d", got, n)
			}
			// /hj ran inline: no connection was promoted to a dispatch
			// goroutine. Hijacked connections that served their session
			// do not show that by themselves: a /hj that started on a
			// dispatch goroutine would hijack just as well, through the
			// other branch of hijackConn, and this test would no longer
			// cover the inline one. Neither subtest requests /async, so
			// not even the async route promoted a connection. drainRead
			// has a second promotion site (inline bytes that end in the
			// middle of a request, HasPendingData): a red here with a
			// nonzero count and a /hj that did run inline would be a
			// request split across two recvs, not a regression of the
			// RouteAsync wiring. The clients write each request in one
			// Write, so this does not happen on loopback.
			if got := e.Metrics().AsyncPromotedConns; got != 0 {
				t.Errorf("AsyncPromotedConns = %d, want 0: /hj did not run inline on the event loop", got)
			}
		})
	}
}

// The tests below force what TestInlineHijackOnAsyncLoop leaves to chance:
// another accept taking the connState hijackConn's inline branch has just
// released. testHookHijackReleased runs between that release and the
// handler's return from Hijack, on the loop's goroutine, and takes the
// released state out of the pool there, which is what the next accept on the
// worker does. The pool hands back the state its own P put last, so it is
// the released one, except when sync.Pool dropped it (under -race Put drops
// one in four at random) or held an older one before it. A miss hands the
// state back, and the tests count the hits and require them.

// ownsLoop reports whether l is one of e's loops: the hook is global, and a
// loop of another engine in the process is none of the test's business.
func ownsLoop(e *Engine, l *Loop) bool {
	for _, x := range e.loops {
		if x == l {
			return true
		}
	}
	return false
}

// takeReleasedConnState is the hook body shared by the tests: when the state
// acquireConnState hands back is the one hijackConn has just released, it
// returns that state, taken out of the pool; otherwise it returns nil and
// leaves the pool as it found it.
func takeReleasedConnState(l *Loop, cs *connState) *connState {
	got := acquireConnState(context.Background(), -1, l.resolved.BufferSize, l.async)
	if got != cs {
		releaseConnState(got)
		return nil
	}
	return got
}

// TestInlineHijackLeavesReissuedConnStateAlone pins the half of celeris#769
// that TestInlineHijackOnAsyncLoop cannot reach by waiting (celeris#885 item
// 6). drainRead used to clear InlineMode through cs.h1State after ProcessH1
// returned, and an inline hijack has released cs inside that call. With no
// other accept in between h1State was nil and the write crashed the loop; the
// guard the fix replaced would have been a nil check, which passes there, and
// still writes the parser state of the connection that took cs from the pool
// in the meantime.
//
// The hook takes the released state, gives it an h1State with InlineMode true
// (as an accept that went through initProtocol and the first inline request
// does), and keeps it. Once the engine has stopped, every state the hook took
// must still have InlineMode true: nothing wrote to it from the hijacked
// connection's request. The hook runs on the loop's own thread, so the take is
// normally the pool access the next accept on that loop makes (same P, unless
// a preemption moved the goroutine; the test counts its hits). (Two workers:
// the engine refuses one.)
func TestInlineHijackLeavesReissuedConnStateAlone(t *testing.T) {
	var hijacked atomic.Int64
	e, addr, stop := startInlineHijackEngine(t, 2, inlineHijackHandler{hijacked: &hijacked})

	var reused atomic.Int64
	var mu sync.Mutex
	var taken []*connState
	hook := func(l *Loop, cs *connState) {
		if !ownsLoop(e, l) {
			return
		}
		got := takeReleasedConnState(l, cs)
		if got == nil {
			return
		}
		got.h1State = conn.NewH1State()
		got.h1State.InlineMode = true
		reused.Add(1)
		mu.Lock()
		taken = append(taken, got)
		mu.Unlock()
	}
	testHookHijackReleased.Store(&hook)
	defer testHookHijackReleased.Store(nil)

	const n = 50
	for i := 0; i < n; i++ {
		if got, err := inlineHijackRequest(addr, "GET /hj HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil || got != "hj" {
			t.Fatalf("hijack %d: body %q, err %v", i, got, err)
		}
	}
	if !stop() {
		t.Fatal("the engine did not stop")
	}
	if got := hijacked.Load(); got != n {
		t.Errorf("hijacked %d connections, want %d", got, n)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Logf("EPTSTHIJACK reuse=%d of %d hijacks", reused.Load(), n)
	if reused.Load() < 1 {
		t.Fatalf("witness: the pool handed the released connState back 0 times in %d hijacks, so no accept could have reissued it and this test shows nothing", n)
	}
	if len(taken) != int(reused.Load()) {
		t.Fatalf("the hook took %d states but counted %d reuses", len(taken), reused.Load())
	}
	cleared := 0
	for _, st := range taken {
		if !st.h1State.InlineMode {
			cleared++
		}
	}
	if cleared > 0 {
		t.Errorf("InlineMode was cleared on %d of %d reissued connStates after their previous owner's hijack: drainRead wrote the parser state of a connection that is not the hijacked one", cleared, len(taken))
	}
}

// viewsHijackHandler answers /hj/... by keeping its path and its X-Token
// value, which are views of the connection's receive buffer, hijacking, and
// reporting what it kept once Hijack has returned. Any other path answers
// normally.
type viewsHijackHandler struct {
	got chan [2]string
}

func (h viewsHijackHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	if !strings.HasPrefix(s.Path, "/hj/") {
		return s.ResponseWriter.WriteResponse(s, 200,
			[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
	}
	path := s.Path
	s.MaterializeHeaders()
	token := ""
	for _, kv := range s.Headers {
		if kv[0] == "x-token" {
			token = kv[1]
		}
	}
	hj, ok := s.ResponseWriter.(stream.Hijacker)
	if !ok {
		return fmt.Errorf("response writer %T cannot hijack", s.ResponseWriter)
	}
	c, err := hj.Hijack(s)
	if err != nil {
		return err
	}
	h.got <- [2]string{path, token}
	_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhj")
	return c.Close()
}

// TestInlineHijackKeepsRequestViewsWhenItsStateIsReused is the state-reuse
// witness of celeris#890 item 16 for the epoll engine. The root test
// TestHijackKeepsRequestViews serves a second connection B on A's worker and
// hopes B's accept takes A's recycled connState, which it cannot observe;
// were a pool change to stop that from happening, the test would pass and
// show nothing.
//
// Here the reuse is forced and counted. A hijacks on an async loop's inline
// path, keeping its path and X-Token value, which view cs.buf (celeris#733).
// In the hook, B's accept is made: the pool's state is taken, and when it is
// A's recycled one, its receive buffer is filled with B's bytes, as B's
// request would. A's strings, read after Hijack returned, must still read A's
// request. Rounds go on until the pool has handed A's state to B five times,
// at most 200; if it does not, the test fails, so the pool can no longer
// make it vacuous. The hook runs on the hijacking loop's own thread, which is
// where B's accept would be made if B's connection landed on that loop. (Two
// workers: the engine refuses one.)
//
// This is epoll only: io_uring never pools a hijacked connection's state
// (worker.go, queuePendingReleaseDetached), so no reuse exists to witness.
func TestInlineHijackKeepsRequestViewsWhenItsStateIsReused(t *testing.T) {
	const (
		secret    = "SECRETSECRETSECRETSECRETSECRETSECRET"
		wantReuse = 5
		maxRounds = 200
	)
	got := make(chan [2]string, 1)
	e, addr, stop := startInlineHijackEngine(t, 2, viewsHijackHandler{got: got})

	var reused atomic.Int64
	var mu sync.Mutex
	var taken []*connState // kept off the pool: each round's B
	hook := func(l *Loop, cs *connState) {
		if !ownsLoop(e, l) {
			return
		}
		b := takeReleasedConnState(l, cs)
		if b == nil {
			return
		}
		// B's request, received into the buffer B's accept was handed.
		fill := strings.Repeat(secret, len(b.buf)/len(secret)+1)
		copy(b.buf, fill)
		reused.Add(1)
		mu.Lock()
		taken = append(taken, b)
		mu.Unlock()
	}
	testHookHijackReleased.Store(&hook)
	defer testHookHijackReleased.Store(nil)

	rounds, wrong := 0, 0
	var samples []string
	for ; rounds < maxRounds && reused.Load() < wantReuse; rounds++ {
		id := fmt.Sprintf("id%06d", rounds)
		want := [2]string{"/hj/" + id, "token-" + id}
		req := "GET /hj/" + id + " HTTP/1.1\r\nHost: x\r\nX-Token: token-" + id + "\r\n\r\n"
		if body, err := inlineHijackRequest(addr, req); err != nil || body != "hj" {
			t.Fatalf("round %d: body %q, err %v", rounds, body, err)
		}
		var k [2]string
		select {
		case k = <-got:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: the handler reported nothing", rounds)
		}
		if k != want {
			wrong++
			if len(samples) < 4 {
				samples = append(samples, fmt.Sprintf("want %q got %q", want, k))
			}
		}
	}
	t.Logf("EPTSTHIJACK rounds=%d reuse=%d (want >= %d within %d rounds) kept strings wrong=%d", rounds, reused.Load(), wantReuse, maxRounds, wrong)
	if reused.Load() < wantReuse {
		t.Fatalf("witness: the pool handed A's released connState back %d times in %d rounds, want %d: B's accept never met the defect's conditions and this test shows nothing", reused.Load(), rounds, wantReuse)
	}
	if wrong > 0 {
		t.Errorf("strings a hijacking handler kept changed after B took A's connState: %d of %d rounds; samples %q", wrong, rounds, samples)
	}
	if !stop() {
		t.Fatal("the engine did not stop")
	}
	// The states B's accepts took stay out of the pool until here, so that
	// none of them is handed to a later round's A.
	mu.Lock()
	defer mu.Unlock()
	if len(taken) != int(reused.Load()) {
		t.Fatalf("the hook took %d states but counted %d reuses", len(taken), reused.Load())
	}
}
