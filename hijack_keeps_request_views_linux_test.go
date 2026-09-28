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

// TestHijackCopiesRequestValuesUnderMultishotRecv pins the Context half of
// celeris#733. In io_uring's opt-in multishot receive mode the request is
// received into a buffer of the worker's provided-buffer ring, which the
// engine hands back to the kernel when the handler returns, hijacked or not.
// The engine cannot keep that buffer, so strings read from the Context
// before Hijack are views the kernel will write into, and only copies made
// at Hijack survive. Context.Hijack copies the request values the Context
// holds, as Context.Detach does, so what the handler reads from the Context
// after Hijack must still read its request once the ring has cycled: B, a
// connection on A's worker, sends more requests than the ring has buffers,
// one receive each, so every buffer, A's included, is written again.
//
// The strings read before Hijack are the rig's witness: they are views of
// A's ring buffer, so they must have changed, or the ring did not cycle and
// the test has not shown anything.
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
		srv.GET("/w", func(c *celeris.Context) error { return c.String(200, "%d", c.WorkerID()) })
		return srv
	})
	defer stopServer()

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
	// buffers (engine/iouring resolveBufRingCount). Twice that many requests
	// cycle it twice.
	b, br := c733DialWorker(t, addr, h.worker, c733Secret)
	defer func() { _ = b.Close() }()
	const requests = 2048
	for i := 0; i < requests; i++ {
		if _, err := c733Get(b, br, "/w", c733Secret); err != nil {
			t.Fatalf("B request %d: %v", i, err)
		}
	}

	// Copies, taken before the server stops: the ring's memory is unmapped
	// then, and the views read before Hijack point into it.
	pre := strings.Clone(h.pre.param + "|" + h.pre.header + "|" + h.pre.path)
	post := strings.Clone(h.post.param + "|" + h.post.header + "|" + h.post.path)
	wantS := want.param + "|" + want.header + "|" + want.path
	t.Logf("C733MSHOT worker=%d requests=%d before-Hijack %q after-Hijack %q", h.worker, requests, pre, post)
	if pre == wantS {
		t.Fatalf("witness: the strings read before Hijack still read A's request after %d requests on B, so the ring did not cycle (is multishot receive on?) and this test shows nothing", requests)
	}
	if post != wantS {
		t.Errorf("strings read from the Context after Hijack changed once the ring cycled: %q, want %q: Hijack did not copy the request values (celeris#733)", post, wantS)
	}
}
