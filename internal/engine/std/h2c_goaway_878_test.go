package std

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// pathHandler records the path of every request it is handed, holds a path
// that starts with "/held" until released, and answers everything else at once.
type pathHandler struct {
	entered chan string
	started chan struct{} // closed when "/held" is entered
	release chan struct{}

	startOnce, releaseOnce sync.Once
}

func newPathHandler() *pathHandler {
	return &pathHandler{
		entered: make(chan string, 64),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (h *pathHandler) Release() { h.releaseOnce.Do(func() { close(h.release) }) }

func (h *pathHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	var path string
	for _, kv := range s.GetHeaders() {
		if kv[0] == ":path" {
			path = kv[1]
		}
	}
	h.entered <- path
	if strings.HasPrefix(path, "/held") {
		h.startOnce.Do(func() { close(h.started) })
		select {
		case <-h.release:
		case <-time.After(20 * time.Second):
			// Safety valve: a failing run must still end.
		}
	}
	return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}}, []byte("ok"))
}

// snapshot returns what the handler has recorded since the last enteredPaths
// or snapshot, without losing it for the next one (it re-queues what it read).
func (h *pathHandler) snapshot() []string {
	got := h.enteredPaths()
	for _, p := range got {
		h.entered <- p
	}
	return got
}

// enteredPaths drains what the handler has recorded so far.
func (h *pathHandler) enteredPaths() []string {
	var out []string
	for {
		select {
		case p := <-h.entered:
			out = append(out, p)
		default:
			return out
		}
	}
}

// startH2CShutdownEngine boots an H2C std engine on a loopback listener and
// returns it with its address. Listen runs until the test ends. wrap, if not
// nil, wraps the engine's http.Handler (h2c front end included) before it
// serves.
func startH2CShutdownEngine(t *testing.T, h stream.Handler, wrap func(http.Handler) http.Handler) (*Engine, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	e, err := New(resource.Config{
		Listener:     ln,
		Engine:       engine.Std,
		Protocol:     engine.H2C,
		ReadTimeout:  -1,
		WriteTimeout: -1,
	}, h)
	if err != nil {
		_ = ln.Close()
		t.Fatalf("New: %v", err)
	}
	if wrap != nil {
		e.server.Handler = wrap(e.server.Handler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.Listen(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	for e.Addr() == nil {
		time.Sleep(2 * time.Millisecond)
	}
	return e, e.Addr().String()
}

// h2Event is one frame the raw client saw, reduced to what the tests read.
type h2Event struct {
	typ      http2.FrameType
	stream   uint32
	code     http2.ErrCode // GOAWAY
	lastID   uint32        // GOAWAY
	status   string        // HEADERS
	endsConn bool          // the read side ended: EOF or reset
}

// rawH2 is an HTTP/2 prior-knowledge client over a bare TCP conn: no
// http2.Transport, so nothing reacts to GOAWAY on the test's behalf and the
// frames the server sends can be read as they are.
type rawH2 struct {
	t      *testing.T
	conn   net.Conn
	fr     *http2.Framer
	events chan h2Event
	wmu    sync.Mutex
}

// dialRawH2 connects and, unless lazy, sends the client preface and SETTINGS.
func dialRawH2(t *testing.T, addr string, lazy bool) *rawH2 {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	r := &rawH2{t: t, conn: c, fr: http2.NewFramer(c, c), events: make(chan h2Event, 256)}
	t.Cleanup(func() { _ = c.Close() })
	if !lazy {
		r.preface()
	}
	return r
}

// dialUpgradeRawH2 opens a connection the RFC 7540 3.2 way: an HTTP/1.1 GET of
// path with Upgrade: h2c, then, after the 101, the HTTP/2 preface. The request
// itself is stream 1.
func dialUpgradeRawH2(t *testing.T, addr, path string) *rawH2 {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	req := "GET " + path + " HTTP/1.1\r\nHost: std.test\r\n" +
		"Connection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: \r\n\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}
	// Read the 101 a byte at a time: whatever follows it is HTTP/2.
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var head []byte
	one := make([]byte, 1)
	for !bytes.HasSuffix(head, []byte("\r\n\r\n")) {
		if _, err := c.Read(one); err != nil {
			t.Fatalf("read the upgrade response: %v (got %q)", err, head)
		}
		head = append(head, one[0])
	}
	_ = c.SetReadDeadline(time.Time{})
	if !bytes.HasPrefix(head, []byte("HTTP/1.1 101")) {
		t.Fatalf("upgrade answered %q, want 101", head)
	}
	r := &rawH2{t: t, conn: c, fr: http2.NewFramer(c, c), events: make(chan h2Event, 256)}
	r.preface()
	return r
}

func (r *rawH2) preface() {
	r.t.Helper()
	r.wmu.Lock()
	defer r.wmu.Unlock()
	if _, err := io.WriteString(r.conn, http2.ClientPreface); err != nil {
		r.t.Fatalf("write preface: %v", err)
	}
	if err := r.fr.WriteSettings(); err != nil {
		r.t.Fatalf("write settings: %v", err)
	}
	go r.readLoop()
}

func (r *rawH2) readLoop() {
	dec := hpack.NewDecoder(4096, nil)
	for {
		f, err := r.fr.ReadFrame()
		if err != nil {
			r.events <- h2Event{endsConn: true}
			return
		}
		ev := h2Event{typ: f.Header().Type, stream: f.Header().StreamID}
		switch f := f.(type) {
		case *http2.GoAwayFrame:
			ev.code, ev.lastID = f.ErrCode, f.LastStreamID
		case *http2.HeadersFrame:
			if fields, derr := dec.DecodeFull(f.HeaderBlockFragment()); derr == nil {
				for _, hf := range fields {
					if hf.Name == ":status" {
						ev.status = hf.Value
					}
				}
			}
		}
		r.events <- ev
	}
}

// get sends a GET request on the given (odd, increasing) stream id.
func (r *rawH2) get(id uint32, path string) {
	r.t.Helper()
	var buf bytes.Buffer
	enc := hpack.NewEncoder(&buf)
	for _, kv := range [][2]string{
		{":method", "GET"}, {":scheme", "http"}, {":authority", "std.test"}, {":path", path},
	} {
		_ = enc.WriteField(hpack.HeaderField{Name: kv[0], Value: kv[1]})
	}
	r.wmu.Lock()
	defer r.wmu.Unlock()
	if err := r.fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID: id, BlockFragment: buf.Bytes(), EndStream: true, EndHeaders: true,
	}); err != nil {
		// The server may have closed the connection after its GOAWAY.
		r.t.Logf("write HEADERS on stream %d: %v", id, err)
	}
}

// waitFor reads events until pred holds for one, or d passes; it returns the
// matching event and every event read on the way.
func (r *rawH2) waitFor(d time.Duration, pred func(h2Event) bool) (h2Event, []h2Event, bool) {
	var seen []h2Event
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case ev := <-r.events:
			seen = append(seen, ev)
			if pred(ev) {
				return ev, seen, true
			}
			if ev.endsConn {
				return ev, seen, false
			}
		case <-timer.C:
			return h2Event{}, seen, false
		}
	}
}

func isGoAway(ev h2Event) bool { return ev.typ == http2.FrameGoAway }

// TestH2CShutdownSendsGoAwayAndRefusesNewStreams pins celeris#878.
//
// net/http hands an h2c connection over and stops tracking it, so
// http.Server.Shutdown neither closes nor GOAWAYs it. Nothing else did
// either: the connection went on accepting streams during the drain's wait
// (so steady traffic could consume the whole budget) and after Shutdown had
// returned and the OnShutdown hooks had run.
//
// One stream is held in its handler, Shutdown starts, and the client must be
// sent GOAWAY (NO_ERROR) at the start of the drain, not at its end; a stream
// it opens after that is not entered, nor is one it opens after Shutdown has
// returned, which is when the held stream's handler returns and its response
// is delivered.
func TestH2CShutdownSendsGoAwayAndRefusesNewStreams(t *testing.T) {
	h := newPathHandler()
	t.Cleanup(h.Release)
	e, addr := startH2CShutdownEngine(t, h, nil)

	c := dialRawH2(t, addr, false)
	c.get(1, "/held")
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("stream 1 was not entered within 5s")
	}
	h.enteredPaths() // "/held"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shut := make(chan error, 1)
	go func() { shut <- e.Shutdown(ctx) }()

	ga, seen, ok := c.waitFor(3*time.Second, isGoAway)
	if !ok {
		t.Fatalf("no GOAWAY within 3s of Shutdown beginning, with stream 1 still in its handler; saw %+v (celeris#878)", seen)
	}
	if ga.code != http2.ErrCodeNo {
		t.Errorf("GOAWAY code %v, want NO_ERROR: a graceful drain", ga.code)
	}
	if ga.lastID != 1 {
		t.Errorf("GOAWAY last stream id %d, want 1: the stream in flight is the last one served", ga.lastID)
	}
	select {
	case err := <-shut:
		t.Fatalf("Shutdown returned %v while stream 1 was still in its handler", err)
	default:
	}

	// Steady traffic after the GOAWAY: none of it is served, and none of it
	// can keep the drain from ending.
	c.get(3, "/after-goaway-3")
	c.get(5, "/after-goaway-5")
	time.Sleep(300 * time.Millisecond)
	if got := h.enteredPaths(); len(got) != 0 {
		t.Errorf("streams opened after the GOAWAY were handed to the handler: %v (celeris#878 item 1 and 4)", got)
	}

	h.Release()
	select {
	case err := <-shut:
		if err != nil {
			t.Errorf("Shutdown: %v, want nil: stream 1 finished well within the budget", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown had not returned 5s after the held stream was released: the streams opened after the GOAWAY kept the drain going")
	}
	if _, seen, ok := c.waitFor(3*time.Second, func(ev h2Event) bool { return ev.typ == http2.FrameHeaders && ev.stream == 1 }); !ok {
		t.Errorf("the response of stream 1 was not delivered; saw %+v", seen)
	}

	// After Shutdown has returned (the hooks run now): nothing is served.
	c.get(7, "/after-shutdown")
	time.Sleep(300 * time.Millisecond)
	if got := h.enteredPaths(); len(got) != 0 {
		t.Errorf("a stream opened after Shutdown returned was handed to the handler: %v (celeris#878)", got)
	}
}

// preambleGate holds an h2c preamble request, "PRI * HTTP/2.0", after net/http
// has read it and before h2cHandler takes the connection over, until the test
// lets it go. net/http closes a connection whose first request it reads after
// Shutdown has begun, so the only way a connection reaches the HTTP/2 server
// after the drain's GOAWAY is a handover that was already under way when the
// drain began; this holds one there, which no client can do on its own.
type preambleGate struct {
	next http.Handler
	held chan struct{} // receives once per preamble held

	mu   sync.Mutex
	hold chan struct{} // nil: not armed; a preamble waits for it to close
}

func newPreambleGate() *preambleGate { return &preambleGate{held: make(chan struct{}, 8)} }

// Arm makes the next preambles wait; Release lets the waiting ones go and
// disarms the gate.
func (g *preambleGate) Arm() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.hold == nil {
		g.hold = make(chan struct{})
	}
}

func (g *preambleGate) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.hold != nil {
		close(g.hold)
		g.hold = nil
	}
}

func (g *preambleGate) wrap(next http.Handler) http.Handler { g.next = next; return g }

func (g *preambleGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "PRI" {
		g.mu.Lock()
		hold := g.hold
		g.mu.Unlock()
		if hold != nil {
			g.held <- struct{}{}
			select {
			case <-hold:
			case <-time.After(20 * time.Second):
			}
		}
	}
	g.next.ServeHTTP(w, r)
}

// TestH2CConnHandedOverDuringDrainIsSentGoAway covers a connection that
// registers with the HTTP/2 server after the GOAWAY Shutdown sent at the start
// of the drain, while the drain is still waiting (a held stream on another
// connection). It is sent GOAWAY too, by the first response it gets, and a
// stream it opens after that is not served.
func TestH2CConnHandedOverDuringDrainIsSentGoAway(t *testing.T) {
	h := newPathHandler()
	t.Cleanup(h.Release)
	gate := newPreambleGate()
	t.Cleanup(gate.Release)
	e, addr := startH2CShutdownEngine(t, h, gate.wrap)

	held := dialRawH2(t, addr, false)
	held.get(1, "/held")
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("stream 1 was not entered within 5s")
	}
	h.enteredPaths()

	// A second connection, stopped after its preamble was read.
	gate.Arm()
	late := dialRawH2(t, addr, true)
	late.preface()
	select {
	case <-gate.held:
	case <-time.After(5 * time.Second):
		t.Fatal("the second preamble did not reach the gate within 5s")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shut := make(chan error, 1)
	go func() { shut <- e.Shutdown(ctx) }()
	if _, seen, ok := held.waitFor(3*time.Second, isGoAway); !ok {
		t.Fatalf("no GOAWAY on the held connection; saw %+v", seen)
	}

	gate.Release() // the second connection reaches the HTTP/2 server only now
	// Stream 1 stays in its handler, so the connection is never idle; stream
	// 3 is answered at once. The answer to stream 3 has to carry the GOAWAY:
	// net/http's HTTP/2 server would otherwise wait for the connection to go
	// idle, which it does not until stream 1 ends.
	late.get(1, "/held-late")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if len(h.snapshot()) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream 1 of the second connection was not entered within 5s")
		}
	}
	late.get(3, "/quick-late")
	if _, seen, ok := late.waitFor(3*time.Second, isGoAway); !ok {
		t.Fatalf("no GOAWAY to a connection handed over after the drain began, with a long stream open on it; saw %+v (celeris#878)", seen)
	}
	late.get(5, "/late-after-goaway")
	time.Sleep(300 * time.Millisecond)
	for _, p := range h.snapshot() {
		if p == "/late-after-goaway" {
			t.Errorf("a stream opened after that connection's GOAWAY was handed to the handler")
		}
	}

	h.Release()
	select {
	case err := <-shut:
		if err != nil {
			t.Errorf("Shutdown: %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown had not returned 5s after the held stream was released")
	}
}

// TestH2CConnHandedOverAfterShutdownReturnedIsRefusedAndSentGoAway is the
// other half: the handover that was under way when the drain began completes
// only after Shutdown has returned (its budget ran out waiting for the
// request that is being handed over). Its stream is not served, the hooks
// having run: it is answered 503, and the connection is sent GOAWAY.
func TestH2CConnHandedOverAfterShutdownReturnedIsRefusedAndSentGoAway(t *testing.T) {
	h := newPathHandler()
	t.Cleanup(h.Release)
	gate := newPreambleGate()
	t.Cleanup(gate.Release)
	e, addr := startH2CShutdownEngine(t, h, gate.wrap)

	gate.Arm()
	c := dialRawH2(t, addr, true)
	c.preface()
	select {
	case <-gate.held:
	case <-time.After(5 * time.Second):
		t.Fatal("the preamble did not reach the gate within 5s")
	}

	// The request being handed over is active as far as net/http can tell,
	// so the drain waits for it until its budget runs out.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := e.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown: %v, want DeadlineExceeded: the handover in progress holds the drain", err)
	}

	gate.Release()
	c.get(1, "/late")
	// The GOAWAY and the 503 are queued by the same response, in either order.
	var status string
	var goAway bool
	var seen []h2Event
	c.waitFor(3*time.Second, func(ev h2Event) bool {
		seen = append(seen, ev)
		switch {
		case ev.typ == http2.FrameGoAway:
			goAway = true
		case ev.typ == http2.FrameHeaders && ev.stream == 1:
			status = ev.status
		}
		return goAway && status != ""
	})
	if status != "503" {
		t.Errorf("status %q, want 503: no request is served once Shutdown has returned (celeris#878); saw %+v", status, seen)
	}
	if !goAway {
		t.Errorf("no GOAWAY to the connection handed over after Shutdown returned; saw %+v", seen)
	}
	if got := h.enteredPaths(); len(got) != 0 {
		t.Errorf("the handler was entered after Shutdown returned: %v", got)
	}
}

// TestH2CUpgradedConnGetsGoAwayAtDrainStart is the first test's counterpart for
// a connection that began as an HTTP/1.1 Upgrade (RFC 7540 3.2), which takes
// a different way into the HTTP/2 server.
func TestH2CUpgradedConnGetsGoAwayAtDrainStart(t *testing.T) {
	h := newPathHandler()
	t.Cleanup(h.Release)
	e, addr := startH2CShutdownEngine(t, h, nil)

	c := dialUpgradeRawH2(t, addr, "/held") // stream 1 is the held request
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the upgrade request was not entered within 5s")
	}
	h.enteredPaths()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shut := make(chan error, 1)
	go func() { shut <- e.Shutdown(ctx) }()
	if ga, seen, ok := c.waitFor(3*time.Second, isGoAway); !ok {
		t.Fatalf("no GOAWAY on the upgraded connection within 3s of Shutdown beginning; saw %+v (celeris#878)", seen)
	} else if ga.code != http2.ErrCodeNo {
		t.Errorf("GOAWAY code %v, want NO_ERROR", ga.code)
	}
	c.get(3, "/after-goaway")
	time.Sleep(300 * time.Millisecond)
	if got := h.enteredPaths(); len(got) != 0 {
		t.Errorf("a stream opened after the GOAWAY was handed to the handler: %v", got)
	}
	h.Release()
	select {
	case err := <-shut:
		if err != nil {
			t.Errorf("Shutdown: %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown had not returned 5s after the held request was released")
	}
}
