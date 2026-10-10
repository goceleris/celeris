//go:build linux

package celeris_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// c733Get sends one GET for path on conn, with an Authorization header, and
// returns the response body.
func c733Get(conn net.Conn, br *bufio.Reader, path, auth string) (string, error) {
	if _, err := conn.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: x\r\nAuthorization: " + auth + "\r\n\r\n")); err != nil {
		return "", err
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return "", err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d, body %q", resp.StatusCode, body)
	}
	return string(body), nil
}

// c733DialWorker dials addr until the connection is served by the worker
// whose ID is want (GET /w answers with c.WorkerID()), and returns it. A
// worker ID of -1 (std) accepts the first connection. The connections that
// land on another worker are closed.
func c733DialWorker(t *testing.T, addr string, want int, auth string) (net.Conn, *bufio.Reader) {
	t.Helper()
	for try := 0; try < 64; try++ {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
		br := bufio.NewReader(conn)
		body, err := c733Get(conn, br, "/w", auth)
		if err != nil {
			t.Fatalf("GET /w: %v", err)
		}
		if got, _ := strconv.Atoi(body); got == want || want < 0 {
			return conn, br
		}
		_ = conn.Close()
	}
	t.Fatalf("64 connections, none served by worker %d", want)
	return nil, nil
}

const c733Secret = "SECRETSECRETSECRETSECRETSECRETSECRET"

// TestHijackKeepsRequestViews pins celeris#733.
//
// On epoll and io_uring the request strings a handler reads from the Context
// (the path, the route params, the headers) are views of the connection's
// receive buffer. Context.Hijack hands the connection to the handler, and the
// engine gave the connection's state, receive buffer included, back to its
// pool: epoll inside the Hijack call, io_uring once the cancelled receive had
// completed. The next connection the worker accepted took that state and
// received its request into the same buffer, so the strings the hijacking
// handler kept for the goroutine that serves the connection read the other
// connection's bytes: a kept header read another client's Authorization
// value.
//
// Each round, connection A's handler keeps its param, header and path and
// hijacks; then connection B, a new connection that is served by A's worker
// (it is redialled until it is), sends requests whose Authorization value
// lies over the bytes A's strings view. After that, the kept strings must
// still read A's values. std serves the request from copies and is the
// control, as is epoll with the route marked Async: its handler, and so
// Hijack, runs on the connection's dispatch goroutine, and epoll never pools
// the state of a connection hijacked there (celeris#668). io_uring refuses
// Hijack on an async worker (celeris#539), so it has no async arm.
func TestHijackKeepsRequestViews(t *testing.T) {
	type arm struct {
		name       string
		engine     celeris.EngineType
		asyncRoute bool // mark /hj Async: the handler runs on the dispatch goroutine
	}
	arms := []arm{
		{"std", celeris.Std, false},
		{"epoll", celeris.Epoll, false},
		{"epoll-async", celeris.Epoll, true},
	}
	if ok, p := c714ProbeIOUring(); ok {
		arms = append(arms, arm{"io_uring", celeris.IOUring, false})
	} else if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
		t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids dropping the io_uring arm", p.IOUringTier, p.KernelVersion)
	} else {
		t.Logf("io_uring tier=%s kernel=%s: io_uring arm not run", p.IOUringTier, p.KernelVersion)
	}

	type kept struct{ param, header, path string }
	type hijacked struct {
		k      kept
		worker int
		conn   net.Conn
		err    error
	}
	for _, a := range arms {
		t.Run(a.name, func(t *testing.T) {
			got := make(chan hijacked, 1)
			addr, stopServer := startC714DetachServer(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: a.engine, Workers: 2})
				rt := srv.GET("/hj/:id", func(c *celeris.Context) error {
					k := kept{param: c.Param("id"), header: c.Header("x-token"), path: c.Path()}
					w := c.WorkerID()
					conn, err := c.Hijack()
					got <- hijacked{k: k, worker: w, conn: conn, err: err}
					return nil
				})
				if a.asyncRoute {
					rt.Async()
				}
				srv.GET("/w", func(c *celeris.Context) error { return c.String(200, "%d", c.WorkerID()) })
				return srv
			})
			defer stopServer()

			const rounds = 20
			wrong, leaked := 0, 0
			var samples []string
			for i := 0; i < rounds; i++ {
				id := fmt.Sprintf("id%06d", i)
				want := kept{param: id, header: "token-" + id, path: "/hj/" + id}
				ca, err := net.DialTimeout("tcp", addr, 2*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ca.Write([]byte("GET /hj/" + id + " HTTP/1.1\r\nHost: x\r\nX-Token: token-" + id + "\r\n\r\n")); err != nil {
					t.Fatal(err)
				}
				var h hijacked
				select {
				case h = <-got:
				case <-time.After(10 * time.Second):
					t.Fatalf("round %d: the handler never hijacked", i)
				}
				if h.err != nil {
					t.Fatalf("round %d: Hijack: %v", i, h.err)
				}

				// B: a new connection on A's worker, served while A's handler
				// keeps its strings. Its Authorization value covers the
				// offsets A's strings view.
				b, br := c733DialWorker(t, addr, h.worker, c733Secret)
				if _, err := c733Get(b, br, "/w", c733Secret); err != nil {
					t.Fatalf("round %d: B: %v", i, err)
				}
				_ = b.Close()

				// What the hijacked session's goroutine would read now.
				if h.k != want {
					wrong++
					if strings.Contains(h.k.param+h.k.header+h.k.path, "SECRET") {
						leaked++
					}
					if len(samples) < 4 {
						samples = append(samples, fmt.Sprintf("want %q got %q", want, h.k))
					}
				}
				_ = h.conn.Close()
				_ = ca.Close()
			}
			t.Logf("C733HIJACK arm=%s rounds=%d kept strings wrong=%d, holding B's Authorization bytes=%d", a.name, rounds, wrong, leaked)
			if wrong > 0 {
				t.Errorf("strings a hijacking handler kept changed after another connection was served: %d/%d (%d with its Authorization bytes); samples %q",
					wrong, rounds, leaked, samples)
			}
		})
	}
}

// TestHijackCopiesRequestValuesUnderMultishotRecv pins both halves of
// celeris#733 and #868 under io_uring's opt-in multishot receive mode.
//
// In that mode the request is received into a buffer of the worker's
// provided-buffer ring. The Context half (celeris#733): Context.Hijack copies
// the request values the Context holds, as Context.Detach does, so what the
// handler reads from the Context after Hijack survives. The engine half
// (celeris#868): the strings the handler read BEFORE Hijack are views of that
// buffer, and a hijacking handler typically hands exactly those to the
// goroutine that serves the connection. The engine used to push the buffer
// back to the kernel when the handler returned, hijacked or not, so those
// strings read other connections' bytes. It now keeps the hijacked request's
// buffer and gives the ring a fresh entry in its place, as epoll gives up
// the receive buffer of a hijacked connection.
//
// B, a connection on A's worker, sends more requests than the ring has
// buffers, one receive each, so every buffer, A's included, is written
// again. Then the strings A's handler read before Hijack, and after it, must
// still read A's request.
//
// The witness that the ring cycled is a control, not A's own strings: a
// request on A's worker whose handler does NOT hijack, and keeps the same
// kind of view. Its view must have changed, or the buffers were not
// rewritten and this test has not shown anything.
//
// The views are read once more after the server has stopped. Stopping
// closes the worker's buffer ring and unmaps its memory, and the buffer a
// hijacked request was read into is kept for as long as the process lives
// (a fault here would be the retained buffer unmapped under the hijacker).
func TestHijackCopiesRequestValuesUnderMultishotRecv(t *testing.T) {
	ok, p := c714ProbeIOUring()
	if !ok || !p.MultishotRecv {
		if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
			t.Fatalf("io_uring tier=%s kernel=%s multishotRecv=%t, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids skipping", p.IOUringTier, p.KernelVersion, p.MultishotRecv)
		}
		t.Skipf("io_uring tier=%s kernel=%s multishotRecv=%t: no multishot receive", p.IOUringTier, p.KernelVersion, p.MultishotRecv)
	}
	t.Setenv("CELERIS_IOURING_MULTISHOT_RECV", "1")

	type kept struct{ param, header, path string }
	type hijacked struct {
		pre, post kept
		worker    int
		conn      net.Conn
		err       error
	}
	got := make(chan hijacked, 1)
	ctlView := make(chan string, 1)
	addr, stopServer := startC714DetachServer(t, func() *celeris.Server {
		srv := celeris.New(celeris.Config{Engine: celeris.IOUring, Workers: 2})
		srv.GET("/hj/:id", func(c *celeris.Context) error {
			pre := kept{param: c.Param("id"), header: c.Header("x-token"), path: c.Path()}
			w := c.WorkerID()
			conn, err := c.Hijack()
			post := kept{param: c.Param("id"), header: c.Header("x-token"), path: c.Path()}
			got <- hijacked{pre: pre, post: post, worker: w, conn: conn, err: err}
			return nil
		})
		// The control: the same view of the request, a handler that does not
		// hijack, so the engine gives the buffer back as it always did.
		srv.GET("/ctl/:id", func(c *celeris.Context) error {
			ctlView <- c.Param("id")
			return c.String(200, "ok")
		})
		srv.GET("/w", func(c *celeris.Context) error { return c.String(200, "%d", c.WorkerID()) })
		return srv
	})
	stop := sync.OnceFunc(stopServer)
	defer stop()

	const id = "id733733"
	want := kept{param: id, header: "token-" + id, path: "/hj/" + id}
	ca, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ca.Close() }()
	if _, err := ca.Write([]byte("GET /hj/" + id + " HTTP/1.1\r\nHost: x\r\nX-Token: token-" + id + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	var h hijacked
	select {
	case h = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler never hijacked")
	}
	if h.err != nil {
		t.Fatalf("Hijack: %v", h.err)
	}
	defer func() { _ = h.conn.Close() }()

	// B: one keep-alive connection on A's worker, one request per round
	// trip, so one receive, and one ring buffer, per request.
	// CELERIS_IOURING_PBUF_COUNT is unset, so the worker sizes the ring to
	// twice its default conns per worker, raised to bufRingCountMin: 1024
	// buffers (internal/engine/iouring resolveBufRingCount). Twice that many requests
	// cycle it twice.
	b, br := c733DialWorker(t, addr, h.worker, c733Secret)
	defer func() { _ = b.Close() }()

	// The control request, on A's worker, before the ring cycles.
	const ctlID = "idctl868"
	if _, err := c733Get(b, br, "/ctl/"+ctlID, c733Secret); err != nil {
		t.Fatalf("control request: %v", err)
	}
	var ctl string
	select {
	case ctl = <-ctlView:
	case <-time.After(10 * time.Second):
		t.Fatal("the control handler never ran")
	}
	if ctl != ctlID {
		t.Fatalf("control view read %q right after its own request, want %q: the rig is wrong", ctl, ctlID)
	}

	const requests = 2048
	for i := 0; i < requests; i++ {
		if _, err := c733Get(b, br, "/w", c733Secret); err != nil {
			t.Fatalf("B request %d: %v", i, err)
		}
	}

	// Copies, taken before the server stops: the ring's memory is unmapped
	// then, and the control's view points into it.
	ctlAfter := strings.Clone(ctl)
	pre := strings.Clone(h.pre.param + "|" + h.pre.header + "|" + h.pre.path)
	post := strings.Clone(h.post.param + "|" + h.post.header + "|" + h.post.path)
	wantS := want.param + "|" + want.header + "|" + want.path
	t.Logf("C733MSHOT worker=%d requests=%d control %q before-Hijack %q after-Hijack %q", h.worker, requests, ctlAfter, pre, post)
	if ctlAfter == ctlID {
		t.Fatalf("witness: the view of a request that did not hijack still reads %q after %d requests on B, so the ring did not cycle (is multishot receive on?) and this test shows nothing", ctlID, requests)
	}
	if pre != wantS {
		t.Errorf("strings read BEFORE Hijack changed once the ring cycled: %q, want %q: the engine gave the hijacked request's receive buffer back to the kernel (celeris#868)", pre, wantS)
	}
	if post != wantS {
		t.Errorf("strings read from the Context after Hijack changed once the ring cycled: %q, want %q: Hijack did not copy the request values (celeris#733)", post, wantS)
	}

	// The worker stops and unmaps its ring. What the hijacker kept must
	// still be readable, and still be A's request. Not after a failure
	// above: the views then point into memory that is no longer the
	// hijacker's, and reading it after the unmap is a fault.
	if t.Failed() {
		return
	}
	stop()
	preStopped := strings.Clone(h.pre.param + "|" + h.pre.header + "|" + h.pre.path)
	if preStopped != wantS {
		t.Errorf("strings read before Hijack, read again after the server stopped: %q, want %q (celeris#868)", preStopped, wantS)
	}
}

// TestMultishotRingSurvivesHijacks pins the other half of celeris#868's
// fix: a hijacked request's buffer is kept, and the ring gets a fresh entry
// in its place. A fix that simply did not return the buffer would shrink the
// ring by one buffer per hijack. Hijacking more connections than the ring has
// buffers would then leave it empty, and the next connection's receive would
// wait for a buffer that never comes.
func TestMultishotRingSurvivesHijacks(t *testing.T) {
	ok, p := c714ProbeIOUring()
	if !ok || !p.MultishotRecv {
		if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
			t.Fatalf("io_uring tier=%s kernel=%s multishotRecv=%t, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids skipping", p.IOUringTier, p.KernelVersion, p.MultishotRecv)
		}
		t.Skipf("io_uring tier=%s kernel=%s multishotRecv=%t: no multishot receive", p.IOUringTier, p.KernelVersion, p.MultishotRecv)
	}
	t.Setenv("CELERIS_IOURING_MULTISHOT_RECV", "1")

	var hijacks atomic.Int64
	addr, stopServer := startC714DetachServer(t, func() *celeris.Server {
		srv := celeris.New(celeris.Config{Engine: celeris.IOUring, Workers: 2})
		srv.GET("/hj/:id", func(c *celeris.Context) error {
			conn, err := c.Hijack()
			if err != nil {
				return err
			}
			hijacks.Add(1)
			_ = conn.Close()
			return nil
		})
		srv.GET("/w", func(c *celeris.Context) error { return c.String(200, "ok") })
		return srv
	})
	defer stopServer()

	// Each worker's ring has 1024 buffers (see the test above), and the
	// connections spread over the workers (one on a memlock-capped host, two
	// otherwise): 3200 hijacks leave each worker more than 1024, with
	// overwhelming probability. Then serve three times the ring on one
	// connection.
	const hijackN = 3200
	for i := 0; i < hijackN; i++ {
		ca, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatalf("hijack %d: dial: %v", i, err)
		}
		_ = ca.SetDeadline(time.Now().Add(20 * time.Second))
		if _, err := ca.Write([]byte("GET /hj/" + strconv.Itoa(i) + " HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatalf("hijack %d: %v", i, err)
		}
		// The handler closes the hijacked connection: the read ends in EOF.
		if _, err := io.Copy(io.Discard, ca); err != nil {
			t.Fatalf("hijack %d: reading to EOF: %v", i, err)
		}
		_ = ca.Close()
	}
	if n := hijacks.Load(); n != hijackN {
		t.Fatalf("hijacked %d connections, want %d", n, hijackN)
	}
	b, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	br := bufio.NewReader(b)
	for i := 0; i < 3072; i++ {
		_ = b.SetDeadline(time.Now().Add(20 * time.Second))
		if _, err := c733Get(b, br, "/w", c733Secret); err != nil {
			t.Fatalf("request %d after %d hijacks: %v: the ring ran out of buffers (celeris#868)", i, hijackN, err)
		}
	}
}
