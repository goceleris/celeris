//go:build linux

package celeris_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/probe"
)

// TestDetachedContextKeepsRequestValues pins celeris#718, the Context.Detach
// twin of celeris#714.
//
// On epoll and io_uring every request string, and the request body, is a
// view of the engine's receive buffer, and the engine keeps receiving into
// that buffer after a handler detaches. Detach therefore copies what it knows
// refers to the buffer. It copied the headers, method, path and raw query,
// but not the route params, the parsed query and cookie caches, the Host it
// serves from Stream.Authority, the strings middleware store on the Context
// (request ID, client-IP/host/scheme overrides, SetString values), the
// response headers middleware set from request headers (the requestid and
// cors echoes, which a detached goroutine serializes when it writes the
// response head), or the body (and so a form parsed from it after the
// call). The handler below sets each of those the way the in-tree middleware
// does, detaches, and reads them back from its goroutine after the peer has
// sent more bytes (on which the native engines close the connection). Odd
// streams carry an X-Forwarded-Host, so Host is served from the SetHost
// override there and from Stream.Authority on even streams. Path, RawQuery
// and Header were already copied and are the controls.
func TestDetachedContextKeepsRequestValues(t *testing.T) {
	type arm struct {
		name   string
		engine celeris.EngineType
		async  bool
	}
	arms := []arm{
		{"std", celeris.Std, false},
		{"epoll", celeris.Epoll, false},
		{"epoll-async", celeris.Epoll, true},
	}
	if ok, p := c714ProbeIOUring(); ok {
		arms = append(arms, arm{"io_uring", celeris.IOUring, false}, arm{"io_uring-async", celeris.IOUring, true})
	} else if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
		t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids dropping the io_uring arms", p.IOUringTier, p.KernelVersion)
	} else {
		t.Logf("io_uring tier=%s kernel=%s: io_uring arms not run", p.IOUringTier, p.KernelVersion)
	}
	fields := []string{"param", "query", "cookie", "host", "scheme", "requestid", "clientip", "setstring",
		"resp-requestid", "resp-cors", "resp-key", "body", "form", "path", "rawquery", "header"}

	for _, a := range arms {
		t.Run(a.name, func(t *testing.T) {
			got := make(chan map[string]string, 1)
			// std does not end a detached stream when the peer sends bytes,
			// so its handler stops waiting sooner; it holds copies anyway.
			wait := 3 * time.Second
			if a.engine == celeris.Std {
				wait = 300 * time.Millisecond
			}
			handler := func(c *celeris.Context) error {
				// What an ordinary middleware chain leaves on the Context
				// before a streaming handler detaches.
				_ = c.QueryParams()                                            // a query validator
				_, _ = c.Cookie("sid")                                         // session
				c.SetRequestID(c.Header("x-request-id"))                       // requestid
				c.SetHeaderTrust("x-request-id", c.Header("x-request-id"))     // requestid echo
				c.SetHeader("access-control-allow-origin", c.Header("origin")) // cors echo
				c.SetHeader(c.Header("x-echo-header"), "echo")                 // a key taken from the request
				c.SetClientIP(c.Header("x-real-ip"))                           // proxy
				c.SetScheme(c.Header("x-forwarded-proto"))                     // proxy
				c.SetString("principal", c.Header("x-api-key"))                // keyauth
				if h := c.Header("x-forwarded-host"); h != "" {
					c.SetHost(h) // proxy
				}

				closed := make(chan struct{})
				var once sync.Once
				stop := func() { once.Do(func() { close(closed) }) }
				c.SetWSErrorHandler(func(error) { stop() })
				c.SetWSDetachClose(stop)
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
					got <- detachedView(c)
					_ = sw.Close()
				}
				if c.EngineSupportsAsyncDetach() {
					go finish()
					return nil
				}
				finish()
				return nil
			}
			addr, stopServer := startC714DetachServer(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async})
				srv.POST("/d/:id", handler)
				return srv
			})
			defer stopServer()

			n := 20
			if a.engine == celeris.Std {
				n = 3 // copies on std; a few streams are enough for the control
			}
			wrong := map[string]int{}
			var samples []string
			for i := 0; i < n; i++ {
				want := expectedDetachedView(i)
				fwd := ""
				if want["fwdhost"] != "" {
					fwd = "X-Forwarded-Host: " + want["fwdhost"] + "\r\n"
				}
				form := "f=" + want["form"]
				req := fmt.Sprintf("POST /d/%s?q=%s HTTP/1.1\r\nHost: %s\r\n%sCookie: sid=%s\r\nX-Request-Id: %s\r\nOrigin: %s\r\nX-Echo-Header: %s\r\nX-Real-Ip: %s\r\nX-Forwarded-Proto: %s\r\nX-Api-Key: %s\r\nX-Id: %s\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: %d\r\n\r\n%s",
					want["param"], want["query"], want["authority"], fwd, want["cookie"], want["requestid"], want["resp-cors"], want["resp-key"], want["clientip"], want["scheme"], want["setstring"], want["header"], len(form), form)
				conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				if _, err := conn.Write([]byte(req)); err != nil {
					t.Fatal(err)
				}
				br := bufio.NewReader(conn)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						t.Fatalf("read response head: %v", err)
					}
					if line == "\r\n" {
						break
					}
				}
				// More bytes than the whole request: every request byte in
				// the receive buffer is overwritten.
				_, _ = conn.Write([]byte(strings.Repeat("Z", 4*len(req))))
				_ = conn.SetReadDeadline(time.Now().Add(wait))
				_, _ = io.Copy(io.Discard, br)
				_ = conn.Close()
				var view map[string]string
				select {
				case view = <-got:
				case <-time.After(10 * time.Second):
					t.Fatalf("conn %d: detached goroutine did not report", i)
				}
				for _, f := range fields {
					if view[f] != want[f] {
						wrong[f]++
						if len(samples) < 6 {
							samples = append(samples, fmt.Sprintf("%s: want %q got %q", f, want[f], view[f]))
						}
					}
				}
			}
			var tally []string
			bad := false
			for _, f := range fields {
				tally = append(tally, fmt.Sprintf("%s=%d", f, wrong[f]))
				bad = bad || wrong[f] > 0
			}
			t.Logf("C714DETACH arm=%s streams=%d wrong: %s", a.name, n, strings.Join(tally, " "))
			if bad {
				t.Errorf("a detached Context does not return the request's values once the peer has sent more bytes (wrong per field over %d streams: %s); samples %q",
					n, strings.Join(tally, " "), samples)
			}
		})
	}
}

// expectedDetachedView returns what the detached goroutine must read on
// stream i, plus the two request-only entries "authority" (the Host header)
// and "fwdhost" (the X-Forwarded-Host header, odd streams only).
func expectedDetachedView(i int) map[string]string {
	id := fmt.Sprintf("id%06d", i)
	v := map[string]string{
		"param":          id,
		"query":          "q" + id,
		"cookie":         "sid" + id,
		"authority":      "h" + id + ".example",
		"host":           "h" + id + ".example",
		"scheme":         "https",
		"requestid":      "rid" + id,
		"clientip":       fmt.Sprintf("10.0.%d.%d", i/250, i%250+1),
		"setstring":      "key" + id,
		"resp-requestid": "rid" + id,
		"resp-cors":      "https://o" + id + ".example",
		"resp-key":       "x-echo-" + id,
		"body":           "f=form" + id,
		"form":           "form" + id,
		"path":           "/d/" + id,
		"rawquery":       "q=q" + id,
		"header":         id,
	}
	if i%2 == 1 {
		v["fwdhost"] = "fh" + id + ".example"
		v["host"] = v["fwdhost"]
	}
	return v
}

func detachedView(c *celeris.Context) map[string]string {
	cookie, _ := c.Cookie("sid")
	principal, _ := c.GetString("principal")
	v := map[string]string{
		"param":     c.Param("id"),
		"query":     c.Query("q"),
		"cookie":    cookie,
		"host":      c.Host(),
		"scheme":    c.Scheme(),
		"requestid": c.RequestID(),
		"clientip":  c.ClientIP(),
		"setstring": principal,
		// The body is read, and the form parsed from it, only here, after
		// the peer has sent more bytes: a streaming handler that binds its
		// input in the goroutine it starts.
		"body":     string(c.Body()),
		"form":     c.FormValue("f"),
		"path":     c.Path(),
		"rawquery": c.RawQuery(),
		"header":   c.Header("x-id"),
	}
	// What the goroutine would serialize if it wrote the response head now
	// (Blob, NoContent, or StreamWriter.WriteHeader(code, c.ResponseHeaders())).
	for _, h := range c.ResponseHeaders() {
		switch {
		case h[0] == "x-request-id":
			v["resp-requestid"] = h[1]
		case h[0] == "access-control-allow-origin":
			v["resp-cors"] = h[1]
		case h[1] == "echo":
			v["resp-key"] = h[0]
		}
	}
	return v
}

// startC714DetachServer starts the server mk builds on a fresh loopback
// listener and returns its address and a shutdown closure.
//
// An io_uring start that fails only with ENOMEM is retried, with a new
// server, for up to 10 s. The kernel charges ring memory to RLIMIT_MEMLOCK
// per UID and gives it back 12-23 ms after a ring closes
// (engine/iouring/ring_budget_linux_test.go), so at the CI runner's 8 MiB a
// start made right after the previous arm stopped, or while another
// package's test binary holds rings, can fail although nothing leaked.
func startC714DetachServer(t *testing.T, mk func() *celeris.Server) (string, func()) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for tries := 1; ; tries++ {
		s := mk()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
		addr, err := c714WaitReady(s, done)
		if err == nil {
			if tries > 1 {
				t.Logf("server start retried on ring ENOMEM: %d tries", tries)
			}
			return addr, func() { cancel(); <-done }
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

// c714WaitReady waits until s accepts connections, or its start returns.
func c714WaitReady(s *celeris.Server, done <-chan error) (string, error) {
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

// c714ProbeIOUring probes the kernel's io_uring support. With
// CELERIS_REQUIRE_IOURING_WORKERS=1 a probe that finds no usable ring is
// retried for up to 10 s before the io_uring arms count as missing: the
// probe's ring can fail with ENOMEM against RLIMIT_MEMLOCK while the rings
// of engines stopped moments ago, or of another test binary run by the same
// user, are still charged (engine/iouring/ring_budget_linux_test.go).
func c714ProbeIOUring() (usable bool, p celerisengine.CapabilityProfile) {
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
