//go:build linux

package celeris_test

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#936 follow-up (maintainer decision U-15): a response a handler wrote
// that is still queued when a later pipelined request on the same connection
// hijacks it must reach the peer before the hijacked connection is handed
// over. On epoll the queued reply was dropped without a word (the connection's
// state is released inside the Hijack call, or, off the loop thread, the loop
// is forbidden to flush it); on io_uring Hijack was refused outright
// ("cannot hijack with pending sends"), so no handler could hijack a
// connection whose client pipelines. std is the control: net/http finishes a
// request, flush included, before it reads the next.
//
// One Write carries both requests, so the first reply is still in the engine's
// write queue when the second handler runs (a reply of 8 KiB or more would go
// out at once through writev on a sync epoll conn, and a second Write may
// arrive in another recv, after the first reply was flushed).

const pipelined936Hijacked = "HIJACKED-BY-THE-HANDLER"

type pipelined936Arm struct {
	name       string
	engine     celeris.EngineType
	asyncAll   bool // /a, /f and /hj are Async routes: all run on the dispatch goroutine
	asyncHJ    bool // only /hj is Async: the first reply is written inline, the hijack runs on the goroutine
	asyncLoop  bool // Config.AsyncHandlers, every route Sync: inline on an async loop
	skipLarge  bool // std: its handler blocks writing the large reply until the client reads
	needsIOUrg bool
}

func pipelined936Arms(t *testing.T) []pipelined936Arm {
	arms := []pipelined936Arm{
		{name: "std", engine: celeris.Std, skipLarge: true},
		{name: "epoll", engine: celeris.Epoll},
		{name: "epoll-asynchandlers", engine: celeris.Epoll, asyncLoop: true},
		{name: "epoll-async-hijack", engine: celeris.Epoll, asyncHJ: true},
		{name: "epoll-async-all", engine: celeris.Epoll, asyncAll: true},
	}
	if ok, p := c714ProbeIOUring(t); ok {
		arms = append(arms, pipelined936Arm{name: "io_uring", engine: celeris.IOUring})
	} else if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
		t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids dropping the io_uring arm", p.IOUringTier, p.KernelVersion)
	}
	return arms
}

// start936 starts the server of an arm. Each Hijack's error is sent on hj.
func (a pipelined936Arm) start(t *testing.T, filePath string, big []byte, hj chan<- error) string {
	addr, stop := startC714DetachServer(t, func() *celeris.Server {
		srv := celeris.New(celeris.Config{Engine: a.engine, Workers: 2, AsyncHandlers: a.asyncLoop})
		ra := srv.GET("/a", func(c *celeris.Context) error { return c.String(200, "reply-A") })
		rf := srv.GET("/f", func(c *celeris.Context) error { return c.File(filePath) })
		rb := srv.GET("/big", func(c *celeris.Context) error {
			return c.Blob(200, "application/octet-stream", big)
		})
		rh := srv.GET("/hj", func(c *celeris.Context) error {
			conn, err := c.Hijack()
			hj <- err
			if err != nil {
				return c.String(503, "refused")
			}
			_, _ = io.WriteString(conn, pipelined936Hijacked)
			return conn.Close()
		})
		switch {
		case a.asyncAll:
			ra.Async()
			rf.Async()
			rb.Async()
			rh.Async()
		case a.asyncHJ:
			rh.Async()
		case a.asyncLoop:
			ra.Sync()
			rf.Sync()
			rb.Sync()
			rh.Sync()
		}
		return srv
	})
	t.Cleanup(func() {
		stopped := make(chan struct{})
		go func() { stop(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(15 * time.Second):
			t.Error("the server did not stop within 15 s")
		}
	})
	return addr
}

func dial936(t *testing.T, addr string, rcvbuf int) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if rcvbuf > 0 {
		_ = c.(*net.TCPConn).SetReadBuffer(rcvbuf)
	}
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	return c
}

// TestHijackFlushesThePipelinedReply936: GET <first>, GET /hj in one Write.
// The client reads the first reply whole, then what the hijacker wrote.
func TestHijackFlushesThePipelinedReply936(t *testing.T) {
	file := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64 KiB: a sync epoll conn sends it by sendfile
	filePath := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(filePath, file, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, a := range pipelined936Arms(t) {
		for _, first := range []struct {
			path string
			body []byte
		}{{"/a", []byte("reply-A")}, {"/f", file}} {
			t.Run(a.name+first.path, func(t *testing.T) {
				hj := make(chan error, 4)
				addr := a.start(t, filePath, nil, hj)
				c := dial936(t, addr, 0)
				req := "GET " + first.path + " HTTP/1.1\r\nHost: x\r\n\r\nGET /hj HTTP/1.1\r\nHost: x\r\n\r\n"
				if _, err := io.WriteString(c, req); err != nil {
					t.Fatal(err)
				}
				br := bufio.NewReader(c)
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					t.Fatalf("the reply to %s never arrived (it was queued when the next request hijacked): %v", first.path, err)
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil || resp.StatusCode != 200 || !bytes.Equal(body, first.body) {
					t.Fatalf("reply to %s: status %d, %d bytes (want %d), err %v", first.path, resp.StatusCode, len(body), len(first.body), err)
				}
				_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
				rest, _ := io.ReadAll(br)
				select {
				case herr := <-hj:
					if herr != nil {
						t.Fatalf("Hijack: %v (the reply to %s was queued, and a hijack must flush it, not fail)", herr, first.path)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("the hijacking handler never ran")
				}
				if string(rest) != pipelined936Hijacked {
					t.Fatalf("after the reply, the hijacker's bytes were %q, want %q", rest, pipelined936Hijacked)
				}
			})
		}
	}
}

// TestHijackWithAQueuedLargeReply936: the first reply is 3.5 MiB and the
// client, whose receive buffer is small, reads nothing until the hijacking
// handler has answered. How much of the reply the kernel takes at once is the
// host's to say, so both outcomes are right, and what the test pins is that
// the first reply is never cut:
//
//   - the engine flushed it all and handed the connection over: the client
//     reads the whole reply, then the hijacker's bytes;
//   - some of it could not be sent without blocking the engine: Hijack fails,
//     the handler answers 503 (the connection is still the engine's), and the
//     client reads the whole reply, then the 503.
//
// A build that hands the connection over with the rest of the reply still
// queued cuts it (epoll dropped what was left in its write queue). The
// reply stays under the 4 MiB the engines allow a connection to leave unsent
// before they stop serving it (celeris#761), so the hijack request is served.
func TestHijackWithAQueuedLargeReply936(t *testing.T) {
	big := bytes.Repeat([]byte("B"), 3584<<10)
	for _, a := range pipelined936Arms(t) {
		if a.skipLarge {
			continue
		}
		t.Run(a.name, func(t *testing.T) {
			hj := make(chan error, 4)
			addr := a.start(t, "", big, hj)
			c := dial936(t, addr, 64<<10)
			req := "GET /big HTTP/1.1\r\nHost: x\r\n\r\nGET /hj HTTP/1.1\r\nHost: x\r\n\r\n"
			if _, err := io.WriteString(c, req); err != nil {
				t.Fatal(err)
			}
			var herr error
			select {
			case herr = <-hj:
				t.Logf("Hijack returned: %v", herr)
			case <-time.After(10 * time.Second):
				t.Fatal("the hijacking handler never ran")
			}
			br := bufio.NewReader(c)
			resp, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || !bytes.Equal(body, big) {
				t.Fatalf("the big reply: %d of %d bytes, err %v (Hijack returned %v)", len(body), len(big), err, herr)
			}
			if herr == nil {
				rest, _ := io.ReadAll(br)
				if string(rest) != pipelined936Hijacked {
					t.Fatalf("after the big reply, the hijacker's bytes were %q, want %q", rest, pipelined936Hijacked)
				}
				return
			}
			resp2, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("the answer to the refused hijack never arrived: %v", err)
			}
			b2, _ := io.ReadAll(resp2.Body)
			_ = resp2.Body.Close()
			if resp2.StatusCode != 503 || string(b2) != "refused" {
				t.Fatalf("answer to the refused hijack: %d %q, want 503 %q", resp2.StatusCode, b2, "refused")
			}
		})
	}
}
