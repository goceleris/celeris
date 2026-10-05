//go:build linux

package session

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/probe"
	"github.com/goceleris/celeris/middleware/store"
)

// These tests pin celeris#731 and the rest of the session IDs the session
// middleware keeps past a request.
//
// On epoll and io_uring, c.Cookie, c.Header and c.Query return views of the
// engine's receive buffer, and the engine receives the connection's next
// request (or, once the connection has closed, another connection's bytes)
// into that buffer. The extractor's result was kept as the ID of a loaded
// session, so every consumer that reads the ID after the handler has
// returned read whatever the buffer held by then:
//
//   - the write-behind worker, which writes the session under the ID it was
//     queued with, after the handler has returned (celeris#731);
//   - a detached stream (SSE, a handler that calls Context.Detach), which
//     keeps the Context and so the Session stored in it, with its ID and the
//     ID the request arrived with (presentedID);
//   - a Session returned by Handler.GetByID, which kept the ID it was passed.
//
// Each test runs on std (where the strings are copies: the control), and on
// epoll and io_uring with sync and async handlers.

// c731Arm is one server configuration a test runs on.
type c731Arm struct {
	name   string
	engine celeris.EngineType
	async  bool
}

// c731Arms returns the std, epoll and io_uring arms. With
// CELERIS_REQUIRE_IOURING_WORKERS=1 a host without a usable io_uring ring
// fails the test instead of dropping the io_uring arms.
func c731Arms(t *testing.T) []c731Arm {
	t.Helper()
	arms := []c731Arm{
		{"std", celeris.Std, false},
		{"epoll", celeris.Epoll, false},
		{"epoll-async", celeris.Epoll, true},
	}
	if ok, p := c731ProbeIOUring(); ok {
		arms = append(arms, c731Arm{"io_uring", celeris.IOUring, false}, c731Arm{"io_uring-async", celeris.IOUring, true})
	} else if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
		t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids dropping the io_uring arms", p.IOUringTier, p.KernelVersion)
	} else {
		t.Logf("io_uring tier=%s kernel=%s: io_uring arms not run", p.IOUringTier, p.KernelVersion)
	}
	return arms
}

// c731SessionIDs returns two valid session IDs for connection i, laid out so
// the second request on a connection puts its ID at the same offset of the
// receive buffer as the first.
func c731SessionIDs(i int) (string, string) {
	return fmt.Sprintf("a1%062x", i), fmt.Sprintf("b2%062x", i)
}

// c731Payload is a stored session that loads: it carries a fresh absolute
// expiry timestamp.
func c731Payload(t *testing.T) []byte {
	t.Helper()
	buf, err := store.EncodeJSON(map[string]any{absExpKey: time.Now().UnixNano(), "user": "u"})
	if err != nil {
		t.Fatal(err)
	}
	return buf
}

// c731GateKV is a store.KV whose Set blocks until the test releases it, and
// then reports the key it was handed, read at that moment.
type c731GateKV struct {
	mu   sync.Mutex
	data map[string][]byte
	gate chan struct{}
	keys chan string
	once sync.Once
}

func newC731GateKV(capacity int) *c731GateKV {
	return &c731GateKV{
		data: map[string][]byte{},
		gate: make(chan struct{}),
		keys: make(chan string, capacity),
	}
}

func (g *c731GateKV) Get(_ context.Context, key string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	v, ok := g.data[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (g *c731GateKV) Set(_ context.Context, key string, _ []byte, _ time.Duration) error {
	<-g.gate
	select {
	case g.keys <- strings.Clone(key):
	default:
	}
	return nil
}

func (g *c731GateKV) Delete(context.Context, string) error { return nil }

// release lets exactly one blocked Set through and returns its key.
func (g *c731GateKV) release(t *testing.T) string {
	t.Helper()
	select {
	case g.gate <- struct{}{}:
	case <-time.After(10 * time.Second):
		t.Fatal("no store write is waiting: the write-behind worker did not write")
	}
	select {
	case k := <-g.keys:
		return k
	case <-time.After(10 * time.Second):
		t.Fatal("the store write did not report its key")
	}
	return ""
}

// open lets every later Set through, so Close can drain the queue.
func (g *c731GateKV) open() { g.once.Do(func() { close(g.gate) }) }

// TestWriteBehindWritesUnderTheRequestsOwnSessionID pins celeris#731. With
// Config.WriteBehind the post-handler store write is queued to a worker that
// runs after the handler has returned. For a loaded session the queued ID
// was the extractor's view of the Cookie header. Each connection sends two
// requests, for sessions S1 and S2, whose cookies sit at the same offset of
// the receive buffer; the store holds every write until the test releases
// it, after both responses. Request 1's write must be keyed S1 and request
// 2's S2. On epoll and io_uring request 1's write carried S2: request 1's
// session data was written into another session.
func TestWriteBehindWritesUnderTheRequestsOwnSessionID(t *testing.T) {
	for _, a := range c731Arms(t) {
		t.Run(a.name, func(t *testing.T) {
			const n = 20
			payload := c731Payload(t)
			kv := newC731GateKV(2*n + 8)
			for i := 0; i < n; i++ {
				s1, s2 := c731SessionIDs(i)
				kv.data[s1] = payload
				kv.data[s2] = payload
			}
			mw, closer := NewWithCloser(Config{Store: kv, WriteBehind: true})
			// Close drains the queue, which needs the gate open.
			defer func() { kv.open(); _ = closer.Close() }()

			addr, stop := c731StartServer(t, a, func(s *celeris.Server) {
				s.GET("/touch", mw, func(c *celeris.Context) error {
					FromContext(c).Set("n", 1)
					return c.String(200, "ok")
				})
			})
			defer stop()

			wrong := 0
			var samples []string
			for i := 0; i < n; i++ {
				s1, s2 := c731SessionIDs(i)
				conn, br := c731Dial(t, addr)
				for _, sid := range []string{s1, s2} {
					c731RoundTrip(t, conn, br, "GET /touch HTTP/1.1\r\nHost: x\r\nCookie: celeris_session="+sid+"\r\n\r\n")
				}
				// Both writes are queued behind the gate. Release them in
				// order, while the connection is still open.
				for k, want := range []string{s1, s2} {
					if got := kv.release(t); got != want {
						wrong++
						if len(samples) < 4 {
							samples = append(samples, fmt.Sprintf("conn %d request %d: key %q, want %q", i, k+1, got, want))
						}
					}
				}
				_ = conn.Close()
			}
			t.Logf("C731WB arm=%s conns=%d writes=%d wrong_key=%d", a.name, n, 2*n, wrong)
			if wrong > 0 {
				t.Errorf("%d of %d write-behind saves went to another session's ID; samples %q", wrong, 2*n, samples)
			}
		})
	}
}

// TestDetachedStreamKeepsTheLoadedSessionID pins the Session a detached
// stream keeps. The middleware stores the Session in the Context
// (Context.Set), and a stream that detaches keeps the Context, so it reads
// the Session after the engine has received more bytes into the buffer the
// request came from. Its ID (Session.ID) and the ID the request arrived with
// (presentedID, which decides whether a cookie the stream re-sends counts as
// dropped) must still be the cookie the client sent. The handler also copies
// the ID before it detaches: that copy is the control.
func TestDetachedStreamKeepsTheLoadedSessionID(t *testing.T) {
	type reading struct{ start, id, presented string }
	for _, a := range c731Arms(t) {
		t.Run(a.name, func(t *testing.T) {
			n := 20
			wait := 3 * time.Second
			if a.engine == celeris.Std {
				// std holds copies, and does not end a detached stream when
				// the peer sends bytes: a few short streams are the control.
				n, wait = 3, 300*time.Millisecond
			}
			payload := c731Payload(t)
			kv := NewMemoryStore()
			defer kv.Close()
			for i := 0; i < n; i++ {
				s1, _ := c731SessionIDs(i)
				if err := kv.Set(context.Background(), s1, payload, time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			got := make(chan reading, 1)
			addr, stop := c731StartServer(t, a, func(s *celeris.Server) {
				s.GET("/stream", New(Config{Store: kv}), func(c *celeris.Context) error {
					sess := FromContext(c)
					start := strings.Clone(sess.ID())
					closed := make(chan struct{})
					var once sync.Once
					end := func() { once.Do(func() { close(closed) }) }
					c.SetWSErrorHandler(func(error) { end() })
					c.SetWSDetachClose(end)
					done := c.Detach()
					sw := c.StreamWriter()
					_ = sw.WriteHeader(200, [][2]string{{"content-type", "text/plain"}})
					_ = sw.Flush()
					finish := func() {
						defer done()
						select {
						case <-closed:
						case <-time.After(wait):
						}
						got <- reading{start: start, id: sess.ID(), presented: sess.presentedID}
						_ = sw.Close()
					}
					if c.EngineSupportsAsyncDetach() {
						go finish()
						return nil
					}
					finish()
					return nil
				})
			})
			defer stop()

			var wrongStart, wrongID, wrongPresented int
			var samples []string
			for i := 0; i < n; i++ {
				sid, _ := c731SessionIDs(i)
				req := "GET /stream HTTP/1.1\r\nHost: x\r\nCookie: celeris_session=" + sid + "\r\n\r\n"
				conn, br := c731Dial(t, addr)
				if _, err := conn.Write([]byte(req)); err != nil {
					t.Fatal(err)
				}
				c731ReadHead(t, br)
				// More bytes than the whole request, so every request byte in
				// the receive buffer is overwritten. The native engines then
				// close the detached connection.
				_, _ = conn.Write([]byte(strings.Repeat("Z", 4*len(req))))
				_ = conn.SetReadDeadline(time.Now().Add(wait))
				_, _ = io.Copy(io.Discard, br)
				_ = conn.Close()
				var r reading
				select {
				case r = <-got:
				case <-time.After(10 * time.Second):
					t.Fatalf("stream %d: the detached goroutine did not report", i)
				}
				for _, f := range []struct {
					v     string
					count *int
					name  string
				}{{r.start, &wrongStart, "copy before Detach"}, {r.id, &wrongID, "ID()"}, {r.presented, &wrongPresented, "presentedID"}} {
					if f.v != sid {
						*f.count++
						if len(samples) < 6 {
							samples = append(samples, fmt.Sprintf("stream %d %s: %q, want %q", i, f.name, f.v, sid))
						}
					}
				}
			}
			t.Logf("C731DETACH arm=%s streams=%d wrong: start=%d id=%d presented=%d", a.name, n, wrongStart, wrongID, wrongPresented)
			if wrongStart+wrongID+wrongPresented > 0 {
				t.Errorf("a detached stream's Session does not hold the session ID the client sent (of %d streams: copy before Detach %d, ID() %d, presentedID %d wrong); samples %q",
					n, wrongStart, wrongID, wrongPresented, samples)
			}
		})
	}
}

// TestGetByIDSessionKeepsItsID pins Handler.GetByID: the Session it returns
// kept the ID it was passed, and GetByID is meant for code outside the
// middleware (admin tools, WebSocket handlers) that holds the Session. Here
// a handler looks up the session named by the request's cookie and keeps
// the result; each connection sends two requests whose cookies sit at the
// same offset of the receive buffer. After both, the Session from request 1
// must still report S1.
func TestGetByIDSessionKeepsItsID(t *testing.T) {
	for _, a := range c731Arms(t) {
		t.Run(a.name, func(t *testing.T) {
			const n = 20
			payload := c731Payload(t)
			kv := NewMemoryStore()
			defer kv.Close()
			for i := 0; i < n; i++ {
				s1, s2 := c731SessionIDs(i)
				for _, sid := range []string{s1, s2} {
					if err := kv.Set(context.Background(), sid, payload, time.Hour); err != nil {
						t.Fatal(err)
					}
				}
			}
			h := NewHandler(Config{Store: kv})
			kept := make(chan *Session, 2)
			addr, stop := c731StartServer(t, a, func(s *celeris.Server) {
				s.GET("/peek", func(c *celeris.Context) error {
					sid, _ := c.Cookie("celeris_session")
					sess, err := h.GetByID(c.Context(), sid)
					if err != nil || sess == nil {
						return c.String(500, "lookup failed")
					}
					kept <- sess
					return c.String(200, "ok")
				})
			})
			defer stop()

			wrong := 0
			var samples []string
			for i := 0; i < n; i++ {
				s1, s2 := c731SessionIDs(i)
				conn, br := c731Dial(t, addr)
				for _, sid := range []string{s1, s2} {
					c731RoundTrip(t, conn, br, "GET /peek HTTP/1.1\r\nHost: x\r\nCookie: celeris_session="+sid+"\r\n\r\n")
				}
				for k, want := range []string{s1, s2} {
					if got := (<-kept).ID(); got != want {
						wrong++
						if len(samples) < 4 {
							samples = append(samples, fmt.Sprintf("conn %d request %d: ID %q, want %q", i, k+1, got, want))
						}
					}
				}
				_ = conn.Close()
			}
			t.Logf("C731GETBYID arm=%s conns=%d sessions=%d wrong_id=%d", a.name, n, 2*n, wrong)
			if wrong > 0 {
				t.Errorf("%d of %d sessions returned by GetByID changed their ID after the request; samples %q", wrong, 2*n, samples)
			}
		})
	}
}

// c731StartServer starts a server on a fresh loopback listener with the
// routes routes adds, and returns its address and a shutdown closure.
//
// An io_uring start that fails only with ENOMEM is retried for up to 10 s.
// The kernel charges ring memory to RLIMIT_MEMLOCK per UID and gives it back
// 12-23 ms after a ring closes (internal/engine/iouring/ring_budget_linux_test.go),
// so at the CI runner's 8 MiB a start made right after the previous arm
// stopped, or while another package's test binary holds rings, can fail
// although nothing leaked.
func c731StartServer(t *testing.T, a c731Arm, routes func(*celeris.Server)) (string, func()) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for tries := 1; ; tries++ {
		s := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async, ShutdownTimeout: 2 * time.Second})
		routes(s)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
		addr, err := c731WaitReady(s, done)
		if err == nil {
			if tries > 1 {
				t.Logf("server start retried on ring ENOMEM: %d tries", tries)
			}
			return addr, func() {
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("server did not stop within 10s")
				}
			}
		}
		cancel()
		_ = ln.Close()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		t.Fatalf("server did not start: %v", err)
	}
}

// c731WaitReady waits until s accepts connections, or its start returns.
func c731WaitReady(s *celeris.Server, done <-chan error) (string, error) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			if err == nil {
				err = errors.New("start returned before the server was ready")
			}
			return "", err
		default:
		}
		if a := s.Addr(); a != nil {
			if c, err := net.DialTimeout("tcp", a.String(), 100*time.Millisecond); err == nil {
				_ = c.Close()
				return a.String(), nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "", errors.New("server not ready within 30s")
}

// c731ProbeIOUring probes the kernel's io_uring support. With
// CELERIS_REQUIRE_IOURING_WORKERS=1 a probe that finds no usable ring is
// retried for up to 10 s before the io_uring arms count as missing: the
// probe's ring can fail with ENOMEM against RLIMIT_MEMLOCK while the rings
// of engines stopped moments ago, or of another test binary run by the same
// user, are still charged.
func c731ProbeIOUring() (usable bool, p celerisengine.CapabilityProfile) {
	p = probe.Probe()
	usable = p.IOUringTier >= celerisengine.High && p.ProvidedBuffers
	if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") != "1" {
		return usable, p
	}
	for deadline := time.Now().Add(10 * time.Second); !usable && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		p = probe.Probe()
		usable = p.IOUringTier >= celerisengine.High && p.ProvidedBuffers
	}
	return usable, p
}

func c731Dial(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	return conn, bufio.NewReader(conn)
}

// c731ReadHead reads a response head and returns its status code and
// Content-Length (0 when absent).
func c731ReadHead(t *testing.T, br *bufio.Reader) (int, int) {
	t.Helper()
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	fields := strings.Fields(status)
	if len(fields) < 2 {
		t.Fatalf("bad status line %q", status)
	}
	code, _ := strconv.Atoi(fields[1])
	n := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read response head: %v", err)
		}
		if line == "\r\n" {
			return code, n
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "content-length") {
			n, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
}

// c731RoundTrip writes one request and reads its Content-Length response,
// which must be a 200.
func c731RoundTrip(t *testing.T, conn net.Conn, br *bufio.Reader, req string) {
	t.Helper()
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	code, n := c731ReadHead(t, br)
	if _, err := br.Discard(n); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if code != 200 {
		t.Fatalf("status %d", code)
	}
}
