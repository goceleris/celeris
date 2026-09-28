//go:build linux

package celeris_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// Tests for celeris#761. On epoll and io_uring the per-connection write
// back-pressure cap (4 MiB: maxPendingBytes, maxSendQueueBytes) was held
// against a single response: the write hook that stages a large body counted
// the body itself, so an HTTP/1.1 response whose headers and body passed the
// cap went out as its headers only, the body dropped without an error and the
// connection left open, and a keep-alive client waited for the declared
// Content-Length until its own timeout. The check after the handler closed
// any connection whose backlog was over the cap, which cut off every response
// that path let through (a copied body, a sendfile body, an HTTP/2 stream's
// flow-control window), and epoll closed a connection answering Connection:
// close with whatever the socket had not taken yet unsent. The cap is there
// to stop a peer that does not read from piling up responses; a single
// response is not such a backlog.
//
// A client that stops receiving bytes for idleCap761 fails its case, so a
// dropped body fails in seconds, not at a timeout.

// idleCap761 is how long a client waits for the next byte before it calls the
// response lost: loopback delivers a 64 MiB body in well under a second even
// under -race, and a dropped body never sends one more byte.
const idleCap761 = 5 * time.Second

var engines761 = []struct {
	name string
	eng  celeris.EngineType
}{{"std", celeris.Std}, {"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}, {"adaptive", celeris.Adaptive}}

// bodies761 builds one patterned body per size, so a body that arrives out of
// order or shifted does not compare equal.
func bodies761(sizes ...int) map[int][]byte {
	m := make(map[int][]byte, len(sizes))
	for _, n := range sizes {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i*7 + i>>13)
		}
		m[n] = b
	}
	return m
}

// TestLargeResponseIsDelivered asks for one body of each size on a keep-alive
// connection and then for /ping on the same connection (the framing must
// still be intact), and once more with Connection: close, where the body must
// be followed by EOF. The sizes straddle the old threshold, which was the cap
// minus the header block, so 4 MiB - 1 failed too; 4 MiB - 4 KiB was
// delivered before the fix on a keep-alive connection.
//
// "sync" runs the handler on the connection's worker, where epoll and
// io_uring hand a large body to the engine as a zero-copy slice (the write
// hook that dropped it). "async-loop" is a server with AsyncHandlers and a
// route that is not async, so the handler runs on the worker but the body is
// copied into the write buffer. "async-route" runs the handler on the
// connection's dispatch goroutine.
func TestLargeResponseIsDelivered(t *testing.T) {
	sizes := []int{4<<20 - 4096, 4<<20 - 1, 4 << 20, 4<<20 + 1, 64 << 20}
	bodies := bodies761(sizes...)
	shapes := []struct {
		name                    string
		asyncServer, asyncRoute bool
	}{{"sync", false, false}, {"async-loop", true, false}, {"async-route", false, true}}

	for _, e := range engines761 {
		for _, sh := range shapes {
			t.Run(e.name+"/"+sh.name, func(t *testing.T) {
				addr := startServer761(t, e.eng, sh.asyncServer, func(s *celeris.Server) {
					big := s.GET("/big", func(c *celeris.Context) error {
						n, err := strconv.Atoi(c.Query("n"))
						if err != nil {
							return err
						}
						body, ok := bodies[n]
						if !ok {
							return fmt.Errorf("no body of %d bytes", n)
						}
						return c.Blob(http.StatusOK, "application/octet-stream", body)
					})
					if sh.asyncRoute {
						big.Async()
					}
				})
				for _, n := range sizes {
					for _, keepAlive := range []bool{true, false} {
						mode := "keep-alive"
						if !keepAlive {
							mode = "close"
						}
						t.Run(strconv.Itoa(n)+"/"+mode, func(t *testing.T) {
							desc := fmt.Sprintf("%s/%s body %d %s", e.name, sh.name, n, mode)
							checkH1Response761(t, addr, "/big?n="+strconv.Itoa(n), desc, bodies[n], keepAlive)
						})
					}
				}
			})
		}
	}
}

// TestLargeFileResponseIsDelivered serves a file with c.File. On epoll's
// worker that is sendfile(2), whose backlog the check after the handler held
// against the cap and closed mid-file; elsewhere it is read into memory and
// written like a Blob.
func TestLargeFileResponseIsDelivered(t *testing.T) {
	sizes := []int{4<<20 + 1, 64 << 20}
	bodies := bodies761(sizes...)
	dir := t.TempDir()
	for _, n := range sizes {
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(n)), bodies[n], 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range engines761 {
		for _, route := range []string{"sync", "async-route"} {
			t.Run(e.name+"/"+route, func(t *testing.T) {
				addr := startServer761(t, e.eng, false, func(s *celeris.Server) {
					r := s.GET("/file/:n", func(c *celeris.Context) error {
						return c.File(filepath.Join(dir, c.Param("n")))
					})
					if route == "async-route" {
						r.Async()
					}
				})
				for _, n := range sizes {
					t.Run(strconv.Itoa(n), func(t *testing.T) {
						desc := fmt.Sprintf("%s/%s file %d", e.name, route, n)
						checkH1Response761(t, addr, "/file/"+strconv.Itoa(n), desc, bodies[n], true)
					})
				}
			})
		}
	}
}

// TestLargeResponseIsDeliveredH2 asks for each body over HTTP/2 (h2c, prior
// knowledge). net/http's client opens a 4 MiB stream window, so a larger body
// leaves up to 4 MiB of frames queued behind the socket: io_uring's check
// after the handler closed the connection on that backlog, at exactly 4 MiB.
func TestLargeResponseIsDeliveredH2(t *testing.T) {
	sizes := []int{4<<20 - 4096, 4<<20 + 1, 64 << 20}
	bodies := bodies761(sizes...)
	for _, e := range engines761 {
		for _, route := range []string{"sync", "async-route"} {
			t.Run(e.name+"/"+route, func(t *testing.T) {
				addr := startServer761(t, e.eng, false, func(s *celeris.Server) {
					r := s.GET("/big/:n", func(c *celeris.Context) error {
						n, _ := strconv.Atoi(c.Param("n"))
						body, ok := bodies[n]
						if !ok {
							return fmt.Errorf("no body of %d bytes", n)
						}
						return c.Blob(http.StatusOK, "application/octet-stream", body)
					})
					if route == "async-route" {
						r.Async()
					}
				})
				p := new(http.Protocols)
				p.SetUnencryptedHTTP2(true)
				cl := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
					Protocols: p,
					DialContext: func(ctx context.Context, network, a string) (net.Conn, error) {
						var d net.Dialer
						c, err := d.DialContext(ctx, network, a)
						if err != nil {
							return nil, err
						}
						return idleConn761{c}, nil
					},
				}}
				defer cl.CloseIdleConnections()
				for _, n := range sizes {
					t.Run(strconv.Itoa(n), func(t *testing.T) {
						desc := fmt.Sprintf("%s/%s h2 body %d", e.name, route, n)
						resp, err := cl.Get("http://" + addr + "/big/" + strconv.Itoa(n))
						if err != nil {
							t.Fatalf("%s: %v", desc, err)
						}
						defer func() { _ = resp.Body.Close() }()
						if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusOK {
							t.Fatalf("%s: %s %d", desc, resp.Proto, resp.StatusCode)
						}
						got, err := io.ReadAll(resp.Body)
						if err != nil || !bytes.Equal(got, bodies[n]) {
							t.Fatalf("%s: received %d of %d body bytes, equal=%v, then %s",
								desc, len(got), n, bytes.Equal(got, bodies[n]), describeReadEnd761(err))
						}
					})
				}
			})
		}
	}
}

// TestSplitBodyResponsesKeepTheConnection sends one request after another on
// one connection, each with a body that arrives in two writes, so the rest of
// the body is read straight into the request's body buffer (epoll's
// zero-copy body receive). epoll's flush on that path never brought the
// pending-byte count back down, so it grew by every response, and once the
// responses on the connection added up to the cap the write hook dropped the
// next one and the client waited for it.
func TestSplitBodyResponsesKeepTheConnection(t *testing.T) {
	const (
		requests = 96       // 96 x 64 KiB = 6 MiB of responses, past the 4 MiB cap
		respSize = 64 << 10 // over the 8 KiB zero-copy body threshold
		bodySize = 32 << 10
	)
	resp := bodies761(respSize)[respSize]
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			addr := startServer761(t, e.eng, false, func(s *celeris.Server) {
				s.POST("/upload", func(c *celeris.Context) error {
					if len(c.Body()) != bodySize {
						return fmt.Errorf("body %d bytes, want %d", len(c.Body()), bodySize)
					}
					return c.Blob(http.StatusOK, "application/octet-stream", resp)
				})
			})
			raw, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = raw.Close() }()
			br := bufio.NewReaderSize(idleConn761{raw}, 64<<10)
			body := bytes.Repeat([]byte("b"), bodySize)
			for i := range requests {
				head := fmt.Sprintf("POST /upload HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", bodySize)
				if _, err := io.WriteString(raw, head+string(body[:1024])); err != nil {
					t.Fatalf("%s request %d: %v", e.name, i+1, err)
				}
				time.Sleep(2 * time.Millisecond) // the rest in a later read
				if _, err := raw.Write(body[1024:]); err != nil {
					t.Fatalf("%s request %d: %v", e.name, i+1, err)
				}
				r, err := http.ReadResponse(br, nil)
				if err != nil {
					t.Fatalf("%s: no answer to request %d of %d on the connection (%s)", e.name, i+1, requests, describeReadEnd761(err))
				}
				got, err := io.ReadAll(r.Body)
				_ = r.Body.Close()
				if err != nil || r.StatusCode != http.StatusOK || !bytes.Equal(got, resp) {
					t.Fatalf("%s: request %d answered %d with %d of %d bytes (%s)", e.name, i+1, r.StatusCode, len(got), respSize, describeReadEnd761(err))
				}
			}
		})
	}
}

// TestPipelinedResponsesKeepTheirOrder pins celeris#802: a client that
// pipelines requests must get the responses in request order. The H1 response
// adapter hands a body of 8 KiB or more to epoll and io_uring as a zero-copy
// slice (and a file of 16 KiB or more to epoll as a sendfile), which the
// engine stages outside its write buffer and sends after it; a response
// written behind it in the same flush was appended to the write buffer and
// went out first. The requests mix large bodies, small bodies and files, in
// one packet, and every response is compared byte for byte, in order. The
// total stays under the write cap, so the back-pressure refusal of
// celeris#761 does not apply.
func TestPipelinedResponsesKeepTheirOrder(t *testing.T) {
	bodies := bodies761(64, 16<<10, 1<<20)
	dir := t.TempDir()
	fileBody := bodies761(256 << 10)[256<<10]
	if err := os.WriteFile(filepath.Join(dir, "f"), fileBody, 0o600); err != nil {
		t.Fatal(err)
	}
	type req struct {
		target string
		want   []byte
	}
	seq := []req{
		{"/big?n=1048576", bodies[1<<20]},
		{"/big?n=64", bodies[64]},
		{"/file", fileBody},
		{"/big?n=16384", bodies[16<<10]},
		{"/file", fileBody},
		{"/big?n=1048576", bodies[1<<20]},
		{"/ping", []byte("ok")},
	}
	shapes := []struct {
		name                    string
		asyncServer, asyncRoute bool
	}{{"sync", false, false}, {"async-loop", true, false}, {"async-route", false, true}}
	for _, e := range engines761 {
		for _, sh := range shapes {
			t.Run(e.name+"/"+sh.name, func(t *testing.T) {
				addr := startServer761(t, e.eng, sh.asyncServer, func(s *celeris.Server) {
					big := s.GET("/big", func(c *celeris.Context) error {
						n, _ := strconv.Atoi(c.Query("n"))
						return c.Blob(http.StatusOK, "application/octet-stream", bodies[n])
					})
					file := s.GET("/file", func(c *celeris.Context) error {
						return c.File(filepath.Join(dir, "f"))
					})
					if sh.asyncRoute {
						big.Async()
						file.Async()
					}
				})
				raw, err := net.Dial("tcp", addr)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = raw.Close() }()
				var batch bytes.Buffer
				for _, r := range seq {
					fmt.Fprintf(&batch, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", r.target)
				}
				if _, err := raw.Write(batch.Bytes()); err != nil {
					t.Fatal(err)
				}
				br := bufio.NewReaderSize(idleConn761{raw}, 64<<10)
				for i, r := range seq {
					resp, err := http.ReadResponse(br, nil)
					if err != nil {
						t.Fatalf("%s/%s: response %d of %d (%s): %s", e.name, sh.name, i+1, len(seq), r.target, describeReadEnd761(err))
					}
					got, err := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(got, r.want) {
						t.Fatalf("%s/%s: response %d of %d (%s): status %d, %d bytes (want %d), equal=%v, %s",
							e.name, sh.name, i+1, len(seq), r.target, resp.StatusCode, len(got), len(r.want), bytes.Equal(got, r.want), describeReadEnd761(err))
					}
				}
			})
		}
	}
}

// TestBackloggedPeerIsClosed is the other side of the cap: a peer that sends
// requests without reading the responses must not make the engine buffer
// without bound, nor be left waiting. Four requests for 3 MiB each, in one
// packet, to a client that reads nothing for a while: the native engines stage
// responses until the backlog they find is over the cap, then refuse the
// next write and close the connection once what was staged has gone out. The
// client must get whole responses, in order, followed by either the rest or
// EOF; before celeris#761 the refused response was dropped and the client
// waited for it.
func TestBackloggedPeerIsClosed(t *testing.T) {
	const n = 3 << 20
	const requests = 4
	body := bodies761(n)[n]
	shapes := []struct {
		name                    string
		asyncServer, asyncRoute bool
	}{{"sync", false, false}, {"async-loop", true, false}, {"async-route", false, true}}
	for _, e := range engines761 {
		for _, sh := range shapes {
			t.Run(e.name+"/"+sh.name, func(t *testing.T) {
				addr := startServer761(t, e.eng, sh.asyncServer, func(s *celeris.Server) {
					r := s.GET("/big", func(c *celeris.Context) error {
						return c.Blob(http.StatusOK, "application/octet-stream", body)
					})
					if sh.asyncRoute {
						r.Async()
					}
				})
				raw, err := net.Dial("tcp", addr)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = raw.Close() }()
				_ = raw.(*net.TCPConn).SetReadBuffer(64 << 10)
				if _, err := io.WriteString(raw, strings.Repeat("GET /big HTTP/1.1\r\nHost: x\r\n\r\n", requests)); err != nil {
					t.Fatal(err)
				}
				time.Sleep(300 * time.Millisecond) // the server stages what it will before the client reads
				br := bufio.NewReaderSize(idleConn761{raw}, 64<<10)
				whole := 0
				for whole < requests {
					resp, err := http.ReadResponse(br, nil)
					if err != nil {
						if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
							break
						}
						t.Fatalf("%s/%s: after %d whole responses: %s", e.name, sh.name, whole, describeReadEnd761(err))
					}
					got, err := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if err != nil || !bytes.Equal(got, body) {
						t.Fatalf("%s/%s: response %d: %d of %d bytes, equal=%v, then %s", e.name, sh.name, whole+1, len(got), n, bytes.Equal(got, body), describeReadEnd761(err))
					}
					whole++
				}
				t.Logf("%s/%s: %d of %d responses, then the close", e.name, sh.name, whole, requests)
				if whole == 0 {
					t.Fatalf("%s/%s: no response at all", e.name, sh.name)
				}
			})
		}
	}
}

// startServer761 starts a server with routes and waits until it answers
// /ping. An io_uring start that fails only with ENOMEM is retried, with a new
// server, for up to 30 s: the kernel charges ring memory to RLIMIT_MEMLOCK
// per UID and gives it back some milliseconds after a ring closes, so at the
// CI runner's 8 MiB a start made right after the previous server stopped, or
// while another package's test binary holds rings, can fail although nothing
// leaked (see startC714DetachServer).
func startServer761(t *testing.T, eng celeris.EngineType, asyncServer bool, routes func(*celeris.Server)) string {
	t.Helper()
	retryUntil := time.Now().Add(30 * time.Second)
	for tries := 1; ; tries++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		s := celeris.New(celeris.Config{Engine: eng, Addr: addr, AsyncHandlers: asyncServer})
		s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
		routes(s)
		startDone := make(chan error, 1)
		go func() { startDone <- s.Start() }()
		err = waitReady761(addr, startDone)
		if err == nil {
			if tries > 1 {
				t.Logf("server start retried on ring ENOMEM: %d tries", tries)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = s.Shutdown(ctx)
				select {
				case <-startDone:
				case <-time.After(15 * time.Second):
					t.Errorf("Start did not return within 15s of Shutdown")
				}
			})
			return addr
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.Shutdown(ctx)
		cancel()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(retryUntil) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		t.Fatalf("server did not start: %v", err)
	}
}

// waitReady761 polls /ping until it answers 200, or returns the error the
// start returned first.
func waitReady761(addr string, startDone <-chan error) error {
	probe := &http.Client{Timeout: 300 * time.Millisecond}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		select {
		case err := <-startDone:
			if err == nil {
				err = errors.New("Start returned nil before the server was ready")
			}
			return err
		default:
		}
		if resp, err := probe.Get("http://" + addr + "/ping"); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("no answer on /ping at %s within 15s", addr)
}

// idleConn761 gives every Read a fresh deadline, so a response that keeps
// arriving is never cut off and one that stops is given up after idleCap761.
type idleConn761 struct{ net.Conn }

func (c idleConn761) Read(p []byte) (int, error) {
	_ = c.SetReadDeadline(time.Now().Add(idleCap761))
	return c.Conn.Read(p)
}

// countingReader761 counts the bytes the client has received, headers
// included, so a failure says how far the response got.
type countingReader761 struct {
	r io.Reader
	n int
}

func (c *countingReader761) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func describeReadEnd761(err error) string {
	var ne net.Error
	switch {
	case err == nil:
		return "no error"
	case errors.As(err, &ne) && ne.Timeout():
		return fmt.Sprintf("no byte for %v, connection still open", idleCap761)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "EOF"
	}
	return err.Error()
}

// checkH1Response761 sends one GET for target on a new connection and checks
// that the whole of want arrives. With keepAlive the connection must then
// answer /ping; without it, the request says Connection: close and the body
// must be followed by EOF.
func checkH1Response761(t *testing.T, addr, target, desc string, want []byte, keepAlive bool) {
	t.Helper()
	n := len(want)
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	cr := &countingReader761{r: idleConn761{raw}}
	br := bufio.NewReaderSize(cr, 64<<10)
	connHdr := ""
	if !keepAlive {
		connHdr = "Connection: close\r\n"
	}
	if _, err := fmt.Fprintf(raw, "GET %s HTTP/1.1\r\nHost: x\r\n%s\r\n", target, connHdr); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("%s: no response head (%d bytes received, %s)", desc, cr.n, describeReadEnd761(err))
	}
	if resp.StatusCode != http.StatusOK || resp.ContentLength != int64(n) {
		t.Fatalf("%s: status %d, Content-Length %d", desc, resp.StatusCode, resp.ContentLength)
	}
	got := make([]byte, n)
	m, err := io.ReadFull(resp.Body, got)
	if err != nil {
		t.Fatalf("%s: received %d of %d body bytes (%d bytes in all), then %s",
			desc, m, n, cr.n, describeReadEnd761(err))
	}
	if !bytes.Equal(got, want) {
		i := 0
		for i < n && got[i] == want[i] {
			i++
		}
		t.Fatalf("%s: all bytes arrived but differ from byte %d on", desc, i)
	}
	_ = resp.Body.Close()

	if !keepAlive {
		// The body must be followed by the close, not by more bytes and not
		// by an open connection.
		extra, err := io.Copy(io.Discard, br)
		if err != nil || extra != 0 {
			t.Fatalf("%s: %d bytes after the body, then %s", desc, extra, describeReadEnd761(err))
		}
		return
	}
	// The connection must still carry the next request.
	if _, err := io.WriteString(raw, "GET /ping HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatalf("%s: the connection took no next request: %v", desc, err)
	}
	next, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("%s: no answer to the next request on the connection (%s)", desc, describeReadEnd761(err))
	}
	b, err := io.ReadAll(next.Body)
	_ = next.Body.Close()
	if err != nil || next.StatusCode != http.StatusOK || string(b) != "ok" {
		t.Fatalf("%s: next request on the connection answered %d %q (%v)", desc, next.StatusCode, b, err)
	}
}
