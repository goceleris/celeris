//go:build linux

package sse_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/middleware/sse"
	"github.com/goceleris/celeris/probe"
)

// sseNativeEngineKinds enumerates the native engines under test, mirroring
// middleware/websocket's engineKinds: epoll always, io_uring only when the
// probe reports a tier that actually works (High+ with provided buffers —
// see the inclusion policy in websocket/engine_linux_test.go).
func sseNativeEngineKinds(t *testing.T) []celeris.EngineType {
	t.Helper()
	kinds := []celeris.EngineType{celeris.Epoll}
	p := probe.Probe()
	if p.IOUringTier >= celerisengine.High && p.ProvidedBuffers {
		kinds = append(kinds, celeris.IOUring)
		t.Logf("io_uring tier=%s kernel=%s — including in test matrix", p.IOUringTier.String(), p.KernelVersion)
	} else {
		t.Logf("io_uring tier=%s kernel=%s — skipping IOUring sub-tests", p.IOUringTier.String(), p.KernelVersion)
	}
	return kinds
}

// sseCell is one engine × AsyncHandlers × transport configuration of the
// matrix. h2c cells drive the same handler over prior-knowledge HTTP/2
// instead of HTTP/1.1.
type sseCell struct {
	name   string
	engine celeris.EngineType
	async  bool
	h2c    bool
}

// sseDisconnectCells returns every available native cell plus std as the
// control (std detects a dead peer through net/http's write error, so it
// is the cell that passes on main and pins the expected behaviour).
//
// Each native engine also gets an h2c cell (celeris#498). H2 needs no
// AsyncHandlers row: the H2 processor picks inline-vs-pooled dispatch per
// stream from the route's own async flag, not from Config.AsyncHandlers
// (which on the native engines only governs the per-conn H1 dispatch
// goroutine), so the two rows would be the same server. The h2c cell marks
// /events async explicitly — see startSSEServer.
func sseDisconnectCells(t *testing.T) []sseCell {
	t.Helper()
	var cells []sseCell
	for _, kind := range sseNativeEngineKinds(t) {
		cells = append(cells,
			sseCell{name: kind.String() + "/async", engine: kind, async: true},
			sseCell{name: kind.String() + "/sync", engine: kind, async: false},
			sseCell{name: kind.String() + "/h2c", engine: kind, async: true, h2c: true},
		)
	}
	cells = append(cells,
		sseCell{name: "std/async", engine: celeris.Std, async: true},
		sseCell{name: "std/sync", engine: celeris.Std, async: false},
	)
	return cells
}

// startSSEServer boots celeris with the given engine on a fresh loopback
// listener, mounts h at /events and returns the address plus a shutdown
// closure. Same shape as middleware/websocket's startNativeServer.
//
// routeAsync marks /events .Async(). It is what the h2c cells need: the H2
// processor runs an END_STREAM GET inline on the event-loop thread unless
// the matched route opted into async dispatch, and an SSE handler blocks
// until its client goes away — inline it would wedge the loop for every
// other connection on that worker. Marking the route async puts the stream
// on the H2 worker pool, which is also the flagAsyncRunning path
// celeris#498 is about. The H1 cells leave it off: there the per-conn
// dispatch goroutine is chosen by Config.AsyncHandlers.
func startSSEServer(tb testing.TB, engine celeris.EngineType, async, routeAsync bool, h celeris.HandlerFunc) (string, func()) {
	tb.Helper()
	s := celeris.New(celeris.Config{
		Engine:          engine,
		AsyncHandlers:   async,
		ShutdownTimeout: 2 * time.Second,
	})
	r := s.GET("/events", h)
	if routeAsync {
		r.Async()
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	serverCtx, serverCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.StartWithListenerAndContext(serverCtx, ln) }()

	// Readiness poll: the native engines rebind the port via SO_REUSEPORT,
	// so the listener we passed in is not the one that ends up serving.
	deadline := time.Now().Add(30 * time.Second)
	var addr string
	for addr == "" {
		if time.Now().After(deadline) {
			select {
			case err := <-done:
				if err != nil {
					msg := err.Error()
					if strings.Contains(msg, "io_uring") || strings.Contains(msg, "not available") {
						tb.Skipf("engine unavailable on this runner: %v", err)
					}
					tb.Fatalf("server start: %v", err)
				}
			default:
			}
			tb.Fatal("server not ready within 30s")
		}
		if a := s.Addr(); a != nil {
			c, err := net.DialTimeout("tcp", a.String(), 100*time.Millisecond)
			if err == nil {
				_ = c.Close()
				addr = a.String()
			}
		}
		if addr == "" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	// Let the SO_REUSEPORT close-and-rebind settle before driving real
	// clients (a probe connection can be accepted and RST during the flip).
	time.Sleep(300 * time.Millisecond)
	return addr, func() {
		serverCancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			tb.Log("server goroutine did not exit within 10s")
		}
	}
}

// openSSEStream dials addr, issues the SSE GET and consumes the response
// head, returning the conn and a reader positioned at the first body line.
func openSSEStream(tb testing.TB, addr string) (net.Conn, *bufio.Reader) {
	tb.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		tb.Fatalf("dial: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	req := "GET /events HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Accept: text/event-stream\r\n" +
		"Connection: keep-alive\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		tb.Fatalf("write request: %v", err)
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		tb.Fatalf("read status: %v", err)
	}
	if !strings.HasPrefix(status, "HTTP/1.1 200") {
		_ = conn.Close()
		tb.Fatalf("status line %q, want 200", strings.TrimSpace(status))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			_ = conn.Close()
			tb.Fatalf("read headers: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			return conn, br
		}
	}
}

// openSSEStreamH2C is openSSEStream over prior-knowledge h2c. It builds a
// transport per client so every stream gets its own TCP connection (an
// http2.Transport would otherwise multiplex all 32 onto one, and killing
// that conn would prove nothing about a single stream), and hands back the
// raw conn the dialer produced so the caller can RST or half-close it the
// same way as an H1 client. The returned closer shuts the transport's idle
// connections down so its read/write goroutines do not count against the
// leak assertion.
//
// The open is bounded by a request context, NOT by a read deadline on the raw
// conn: http2.Transport keeps a persistent read loop over that socket for the
// life of the stream, so an absolute deadline would reset the stream from the
// client side and the server would cancel the handler through handleRSTStream
// — a different path from the one under test, and a false pass or a flake
// depending on timing.
func openSSEStreamH2C(tb testing.TB, addr string) (net.Conn, *bufio.Reader, func()) {
	tb.Helper()
	var raw net.Conn
	tr := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, _ string, _ *tls.Config) (net.Conn, error) {
			c, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, network, addr)
			if err == nil {
				raw = c
			}
			return c, err
		},
	}
	// Bound the OPEN with a request context, never with a read deadline on
	// the raw conn. http2.Transport runs a persistent read loop over that
	// socket for the life of the stream, so an absolute SetReadDeadline
	// kills the connection N seconds after dial no matter how healthy it
	// is -- and an SSE stream is deliberately held open far longer than
	// that. It survived on a fast host and failed on a slower CI runner,
	// where the deadline expired inside RoundTrip itself.
	//
	// The context is cancelled at test cleanup rather than when this
	// function returns: cancelling on return would tear down the very
	// stream the caller is about to assert on.
	reqCtx, cancelReq := context.WithCancel(context.Background())
	tb.Cleanup(cancelReq)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://"+addr+"/events", nil)
	if err != nil {
		tb.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		tr.CloseIdleConnections()
		tb.Fatalf("h2c round trip: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		tr.CloseIdleConnections()
		tb.Fatalf("h2c status %d, want 200", resp.StatusCode)
	}
	if raw == nil {
		_ = resp.Body.Close()
		tr.CloseIdleConnections()
		tb.Fatal("h2c dialer never ran — no raw conn to kill")
	}
	return raw, bufio.NewReader(resp.Body), func() {
		_ = resp.Body.Close()
		tr.CloseIdleConnections()
	}
}

// readTicks consumes body lines until n "event: tick" lines were seen.
func readTicks(tb testing.TB, br *bufio.Reader, n int) {
	tb.Helper()
	for seen := 0; seen < n; {
		line, err := br.ReadString('\n')
		if err != nil {
			tb.Fatalf("read after %d ticks: %v", seen, err)
		}
		if strings.HasPrefix(line, "event: tick") {
			seen++
		}
	}
}

// TestClientDisconnectCancelsStream is the end-to-end celeris#494 pin.
//
// On epoll/io_uring an SSE stream never learned that its client went away:
// c.Context() is context.Background() for every H1 stream and the H1
// StreamWriter returns nil unconditionally (the engine's post-Detach guard
// silently drops bytes after closeConn), so client.Context() was never
// cancelled and Send never errored. Every killed stream leaked three
// goroutines — the user handler, the sse heartbeat goroutine
// (sse.New.func1.1.2) and celeris.(*routerAdapter).recoverAndRelease.func1
// parked on <-c.detachDone — plus the detached celeris.Context: 5,988
// streams × 3 = 17,964 goroutines in 40 min on the soak cell.
//
// Two handler shapes, both of which the fix must cover:
//
//   - "default": heartbeat on, handler ticks every 10 ms via Send and
//     returns on ctx.Done or a Send error (the soak refapp shape; the
//     three-goroutine signature).
//   - "ctx-only": heartbeat disabled, handler sends three ticks and then
//     parks on client.Context().Done() (the bench refapp shape). Nothing
//     but engine-driven cancellation can wake it, so this mode proves the
//     engine → Client.Context() binding rather than write-error luck.
//     On std the signal is net/http's request context, exposed through
//     the same SetWSDetachClose hook by engine/std/bridge.go, so std
//     runs this mode as well.
//
// Half the clients close with SO_LINGER 0 (RST — exercises OnError via
// ECONNRESET), half half-close with shutdown(SHUT_WR) via CloseWrite (FIN —
// exercises the recv-EOF path: iouring Res==0 / epoll errPeerClosed). The
// FIN half deliberately does NOT use a plain Close: the clients stop
// reading after two ticks while the handler keeps writing, so by the time
// the kill loop runs every conn has unread bytes in its receive queue and
// Linux tcp_close() then emits RST, not FIN (RFC 2525 §2.17). CloseWrite
// sends FIN regardless of unread data and leaves the receive side open, so
// those clients can additionally observe the server's FIN: the engine
// reaps a Detached conn inline on recv-EOF, so the client must read EOF
// shortly after its handler returned. Every handler must return within
// 3 s and the goroutine count must fall back to baseline within 5 s,
// sampled BEFORE shutdown (Engine.Shutdown on the native engines returns
// immediately and never cancels SSE streams, so it cannot mask a leak).
//
// The h2c cells extend the pin to HTTP/2 (celeris#498). H2 has the same
// hole for a different reason: an H2 stream's Context IS cancellable, but
// closeConn → CloseH2 → Manager.Close used to skip streams whose handler
// was still running on the H2 worker pool — precisely the SSE ones — so
// nothing ever cancelled them. Both modes leaked all 32 handlers, and
// because the H2 pool is process-global the parked handlers went on to
// starve every later H2 connection in the same binary.
func TestClientDisconnectCancelsStream(t *testing.T) {
	const clients = 32
	modes := []struct {
		name      string
		heartbeat time.Duration
		ctxOnly   bool
	}{
		{"default", 25 * time.Millisecond, false},
		{"ctx-only", -1, true},
	}
	cells := sseDisconnectCells(t)
	for _, mode := range modes {
		for _, cell := range cells {
			t.Run(mode.name+"/"+cell.name, func(t *testing.T) {
				runDisconnectCell(t, cell, mode.heartbeat, mode.ctxOnly, clients)
			})
		}
	}
}

func runDisconnectCell(t *testing.T, cell sseCell, heartbeat time.Duration, ctxOnly bool, clients int) {
	t.Helper()
	var started, returned atomic.Int32
	handler := sse.New(sse.Config{
		HeartbeatInterval: heartbeat,
		Handler: func(client *sse.Client) {
			started.Add(1)
			defer returned.Add(1)
			ctx := client.Context()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			n := 0
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					n++
					if err := client.Send(sse.Event{Event: "tick", Data: fmt.Sprintf("%d", n)}); err != nil {
						return
					}
					if ctxOnly && n == 3 {
						// Park until the engine cancels the context.
						// Nothing else can wake this handler.
						<-ctx.Done()
						return
					}
				}
			}
		},
	})

	addr, shutdown := startSSEServer(t, cell.engine, cell.async, cell.h2c, handler)
	defer shutdown()

	runtime.GC()
	baseline := runtime.NumGoroutine()

	conns := make([]net.Conn, 0, clients)
	var closers []func()
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
		for _, cl := range closers {
			cl()
		}
	}()
	for i := 0; i < clients; i++ {
		var conn net.Conn
		var br *bufio.Reader
		if cell.h2c {
			var closeClient func()
			conn, br, closeClient = openSSEStreamH2C(t, addr)
			closers = append(closers, closeClient)
		} else {
			conn, br = openSSEStream(t, addr)
		}
		conns = append(conns, conn)
		readTicks(t, br, 2)
		if cell.h2c {
			// Head read — drop the open deadline before the kill so it
			// cannot fire during the assertions (see openSSEStreamH2C).
			_ = conn.SetReadDeadline(time.Time{})
		}
	}
	if got := started.Load(); got != int32(clients) {
		t.Fatalf("started=%d handlers, want %d", got, clients)
	}

	// Kill the clients: even = RST (SO_LINGER 0 + Close), odd = FIN
	// (CloseWrite; see the doc comment for why not Close). The conns stay
	// in the slice so the deferred loop closes the half-open ones after
	// the assertions below have read the server's FIN from them.
	for i, c := range conns {
		tc := c.(*net.TCPConn)
		if i%2 == 0 {
			_ = tc.SetLinger(0)
			_ = tc.Close()
		} else if err := tc.CloseWrite(); err != nil {
			t.Fatalf("client %d CloseWrite: %v", i, err)
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for returned.Load() < int32(clients) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := returned.Load(); got != int32(clients) {
		t.Errorf("%d/%d SSE handlers still running 3s after their clients closed: "+
			"client.Context() was never cancelled and Send never errored — celeris#494",
			clients-int(got), clients)
	}

	// Goroutine count must return to baseline (+ a small pad for engine
	// housekeeping). Sampled before shutdown, see the doc comment.
	const pad = 8
	deadline = time.Now().Add(5 * time.Second)
	var n int
	for {
		runtime.GC()
		n = runtime.NumGoroutine()
		if n <= baseline+pad || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n > baseline+pad {
		t.Errorf("goroutines: baseline=%d now=%d (delta=%d, pad=%d) 5s after %d clients closed — "+
			"expected leaked frames per stream: the sse Handler, sse.New.func1.1.2 (heartbeat) and "+
			"celeris.(*routerAdapter).recoverAndRelease.func1 (<-c.detachDone) — celeris#494",
			baseline, n, n-baseline, pad, clients)
	}

	// The half-closed (FIN) clients must see the server's FIN: on the
	// native engines recv-EOF on a Detached conn runs closeConn inline
	// (epoll errPeerClosed / io_uring Res==0), so the fd is gone as soon
	// as the engine observed the peer's FIN. std is not pinned here:
	// net/http's connection lifecycle after a half-close is its own
	// business and not what celeris#494 is about. Nor is h2c: an H2 conn
	// is never Detached, so it is the ordinary H2 read loop that reaps it
	// on recv-EOF, which is not the behaviour under test.
	if cell.engine == celeris.Std || cell.h2c {
		return
	}
	for i, c := range conns {
		if i%2 == 0 {
			continue
		}
		if err := expectEOF(c, 2*time.Second); err != nil {
			t.Errorf("client %d (FIN): server did not close the connection after its handler returned: %v", i, err)
		}
	}
}

// expectEOF drains c until EOF and returns nil, or the read error (a
// timeout when the server never closed its side) otherwise.
func expectEOF(c net.Conn, within time.Duration) error {
	_ = c.SetReadDeadline(time.Now().Add(within))
	_, err := io.Copy(io.Discard, c)
	return err
}

// TestStreamEndClosesConnection pins the second celeris#494 commit: after
// Detach the native engines apply none of their configured timeouts to a
// connection and never resume parsing it, so a finished SSE stream used to
// leave the TCP connection open (fd + connState) until the peer closed it.
// runStream's defer now arms a 1 ns idle deadline so the next sweep runs
// closeConn and the server sends FIN right after the terminal chunk.
//
// The client here stays fully open and reads: the handler ends on its own
// after three ticks, and the client must observe the chunked terminator
// followed by EOF within 2 s. Only the native engines are exercised — the
// idle-deadline hook is a no-op on std, whose keep-alive conn legitimately
// stays open for the next request.
func TestStreamEndClosesConnection(t *testing.T) {
	for _, kind := range sseNativeEngineKinds(t) {
		for _, async := range []bool{true, false} {
			name := kind.String() + "/sync"
			if async {
				name = kind.String() + "/async"
			}
			t.Run(name, func(t *testing.T) {
				runStreamEndCell(t, kind, async)
			})
		}
	}
}

func runStreamEndCell(t *testing.T, engine celeris.EngineType, async bool) {
	t.Helper()
	handler := sse.New(sse.Config{
		HeartbeatInterval: -1,
		Handler: func(client *sse.Client) {
			for n := 1; n <= 3; n++ {
				if err := client.Send(sse.Event{Event: "tick", Data: fmt.Sprintf("%d", n)}); err != nil {
					return
				}
			}
		},
	})
	addr, shutdown := startSSEServer(t, engine, async, false, handler)
	defer shutdown()

	const clients = 8
	for i := 0; i < clients; i++ {
		conn, br := openSSEStream(t, addr)
		readTicks(t, br, 3)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		tail, err := io.ReadAll(br)
		if err != nil {
			_ = conn.Close()
			t.Fatalf("client %d: no server FIN within 2s after the SSE handler returned (read %d trailing bytes, err=%v) — "+
				"the finished Detached conn was never reaped (celeris#494, SetWSIdleDeadline)", i, len(tail), err)
		}
		if !strings.HasSuffix(string(tail), "0\r\n\r\n") {
			t.Errorf("client %d: stream did not end with the chunked terminator before FIN; tail=%q", i, tail)
		}
		_ = conn.Close()
	}
}
