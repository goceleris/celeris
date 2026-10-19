//go:build linux

package celeris_test

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// Tests for celeris#875. On HTTP/1.1 StreamWriter.Flush does nothing on the
// native engines (internal/conn/response.go, h1ResponseAdapter.Flush returns
// nil: "write() is synchronous"), and the write it relies on is not
// synchronous there: before Detach the epoll and io_uring write hooks only
// append to the connection's send buffer, which goes out when the handler
// returns, and a handler that blocks holds the bytes it has written back.
// After Detach each Write sends on the calling goroutine. The std engine
// flushes through http.Flusher. The docs said Flush "guarantees the bytes are
// on the wire" (docs streaming.md) and its godoc "ensures buffered data is
// sent to the network"; they now say what is measured here.
//
// The matrix covers std, epoll, io_uring and adaptive (the default engine on
// Linux, which runs on epoll and io_uring and behaves the same). The handler
// writes the head and one chunk, calls Flush, then waits for the test to
// release it. The client reports whether the chunk reached it before
// the release (early) and then that the whole body arrives once released
// (the control: a case that never delivered would otherwise read as "late").

const (
	first875  = "first-875"
	second875 = "second-875"
	// early875 is how long the client waits for the chunk before it calls the
	// engine "held back". Loopback delivers in microseconds; an engine that
	// sends on Write answers inside this window with a wide margin even under
	// -race, and one that holds the bytes back never does.
	early875 = 750 * time.Millisecond
)

// flushRoute875 registers /flush875: head, one chunk, Flush, wait for
// release, a second chunk, Close. With detach it follows the canonical
// pattern of the streaming docs: Detach, then a goroutine on an engine that
// supports it, the handler's own goroutine on std.
func flushRoute875(s *celeris.Server, release <-chan struct{}, detach, async bool) {
	r := s.GET("/flush875", func(c *celeris.Context) error {
		sw := c.StreamWriter()
		if sw == nil {
			return fmt.Errorf("no StreamWriter")
		}
		var done func()
		if detach {
			done = c.Detach()
		}
		run := func() {
			if done != nil {
				defer done()
			}
			defer func() { _ = sw.Close() }()
			if err := sw.WriteHeader(200, [][2]string{{"content-type", "text/plain"}}); err != nil {
				return
			}
			if _, err := sw.Write([]byte(first875)); err != nil {
				return
			}
			if err := sw.Flush(); err != nil {
				return
			}
			select {
			case <-release:
			case <-time.After(20 * time.Second):
			}
			_, _ = sw.Write([]byte(second875))
		}
		if detach && c.EngineSupportsAsyncDetach() {
			go run()
			return nil
		}
		run()
		return nil
	})
	if async {
		r.Async()
	}
}

// flushProbe875 requests /flush875 with Connection: close and reports
// whether the first chunk arrived within early875, then, after release is
// called, the whole chunked response up to its last chunk.
func flushProbe875(addr string, release func()) (early bool, full string, err error) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false, "", err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("GET /flush875 HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")); err != nil {
		return false, "", err
	}
	var mu sync.Mutex
	var buf strings.Builder
	seenFirst := make(chan struct{})
	var once sync.Once
	readDone := make(chan error, 1)
	go func() {
		p := make([]byte, 4096)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
			n, rerr := conn.Read(p)
			mu.Lock()
			buf.Write(p[:n])
			got := buf.String()
			mu.Unlock()
			if strings.Contains(got, first875) {
				once.Do(func() { close(seenFirst) })
			}
			// The chunked body ends with the last chunk. A detached
			// connection is not closed by the server after it, so the read
			// does not wait for EOF.
			if strings.Contains(got, second875) && strings.HasSuffix(got, "0\r\n\r\n") {
				readDone <- nil
				return
			}
			if rerr != nil {
				if rerr == io.EOF {
					rerr = nil
				}
				readDone <- rerr
				return
			}
		}
	}()
	select {
	case <-seenFirst:
		early = true
	case <-time.After(early875):
	}
	release()
	if rerr := <-readDone; rerr != nil {
		mu.Lock()
		defer mu.Unlock()
		return early, buf.String(), rerr
	}
	mu.Lock()
	defer mu.Unlock()
	return early, buf.String(), nil
}

func TestStreamWriterFlushH1_875(t *testing.T) {
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{{"std", celeris.Std}, {"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}, {"adaptive", celeris.Adaptive}} {
		for _, sh := range []struct {
			name  string
			async bool
		}{{"sync", false}, {"async-route", true}} {
			for _, detach := range []bool{false, true} {
				name := fmt.Sprintf("%s/%s/detach=%v", e.name, sh.name, detach)
				t.Run(name, func(t *testing.T) {
					release := make(chan struct{})
					var once sync.Once
					rel := func() { once.Do(func() { close(release) }) }
					addr := startServerConfig761(t, celeris.Config{Engine: e.eng, Protocol: celeris.HTTP1}, func(s *celeris.Server) {
						flushRoute875(s, release, detach, sh.async)
					})
					// Registered after the server's own cleanup, so it runs
					// first: a handler still waiting for release is let go
					// before Shutdown waits for it.
					t.Cleanup(rel)
					early, full, err := flushProbe875(addr, rel)
					// std flushes through http.Flusher. A native engine sends on
					// Write only after Detach; before it the chunk waits for the
					// handler to return, Flush or not.
					want := e.eng == celeris.Std || detach
					t.Logf("%s: chunk before the handler returned = %v (want %v)", name, early, want)
					if err != nil {
						t.Fatalf("read: %v (got %q)", err, full)
					}
					if early != want {
						t.Errorf("chunk written and flushed arrived before the handler returned = %v, want %v", early, want)
					}
					if !strings.HasPrefix(full, "HTTP/1.1 200") || !strings.Contains(full, first875) || !strings.Contains(full, second875) {
						t.Fatalf("response incomplete after release: %q", full)
					}
				})
			}
		}
	}
}

// The adapter's Write and Flush used to return nil on the native engines
// whatever became of the connection (internal/conn/response.go,
// h1ResponseAdapter): the engine's write hook has no error return, and after
// the connection is gone it drops the bytes. So on epoll and io_uring over
// HTTP/1.1 neither Write nor Flush reported a client that had gone away, which
// the streaming docs said both did (celeris#494 is the SSE middleware's
// answer: it watches the connection through the engine's callbacks). The
// engines now install an error query on the H1 state that the adapter
// consults after each write (celeris#884 item 45, celeris#885 item 30), so a
// Write or Flush after the engine has torn the connection down returns an
// error, as std's does from net/http. The handler detaches (the only way to
// stream there), waits for the client to reset the connection, then writes and
// flushes for a while and reports the first error it saw.

func TestStreamWriterWriteErrorAfterPeerGoneH1_875(t *testing.T) {
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{{"std", celeris.Std}, {"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}, {"adaptive", celeris.Adaptive}} {
		t.Run(e.name, func(t *testing.T) {
			gone := make(chan struct{})
			var once sync.Once
			goneNow := func() { once.Do(func() { close(gone) }) }
			type result struct {
				err    error
				writes int
			}
			res := make(chan result, 1)
			addr := startServerConfig761(t, celeris.Config{Engine: e.eng, Protocol: celeris.HTTP1}, func(s *celeris.Server) {
				s.GET("/gone875", func(c *celeris.Context) error {
					sw := c.StreamWriter()
					if sw == nil {
						return fmt.Errorf("no StreamWriter")
					}
					done := c.Detach()
					run := func() {
						defer done()
						if err := sw.WriteHeader(200, [][2]string{{"content-type", "text/plain"}}); err != nil {
							res <- result{err: err}
							return
						}
						if _, err := sw.Write([]byte(first875)); err != nil {
							res <- result{err: err}
							return
						}
						if err := sw.Flush(); err != nil {
							res <- result{err: err}
							return
						}
						<-gone
						time.Sleep(300 * time.Millisecond) // the engine sees the reset
						chunk := make([]byte, 1024)
						n := 0
						for ; n < 80; n++ {
							if _, err := sw.Write(chunk); err != nil {
								res <- result{err: err, writes: n}
								return
							}
							if err := sw.Flush(); err != nil {
								res <- result{err: err, writes: n}
								return
							}
							time.Sleep(25 * time.Millisecond)
						}
						res <- result{writes: n}
					}
					if c.EngineSupportsAsyncDetach() {
						go run()
						return nil
					}
					run()
					return nil
				})
			})
			t.Cleanup(goneNow) // after the server's cleanup, so it runs before Shutdown
			conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Write([]byte("GET /gone875 HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			// Read until the first chunk is in, then reset the connection.
			var got strings.Builder
			p := make([]byte, 4096)
			for !strings.Contains(got.String(), first875) {
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				n, rerr := conn.Read(p)
				got.Write(p[:n])
				if rerr != nil {
					t.Fatalf("read before the first chunk: %v (got %q)", rerr, got.String())
				}
			}
			_ = conn.(*net.TCPConn).SetLinger(0)
			_ = conn.Close()
			goneNow()

			select {
			case r := <-res:
				t.Logf("%s: first write/flush error after the reset = %v, after %d writes", e.name, r.err, r.writes)
				if r.err == nil {
					t.Errorf("Write/Flush never reported the gone client in %d writes", r.writes)
				}
				// std reports it after a write or two (the socket's RST arrives
				// with the first); a native engine learns of the reset on its
				// loop, which the handler's 300 ms pause above let happen, so
				// the first write after it must fail too.
				if r.err != nil && r.writes > 2 {
					t.Errorf("the gone client was reported only after %d writes, want within 2", r.writes)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("the handler did not finish")
			}
		})
	}
}

// The godoc advice is "to deliver bytes while the handler is still running,
// Detach it". The matrix above detaches and then returns the handler (the
// canonical pattern: the goroutine streams), so it does not show that the
// advice holds for a handler that detaches and keeps writing inline, on its
// own goroutine, while it runs. This one does: Detach, then head, chunk and
// Flush in the handler, which blocks until released. The chunk must reach the
// client before that, and the .Async() route (where the dispatch goroutine
// holds the detach mutex until the Detach has been published) must not
// deadlock on the inline Write.

func TestStreamWriterDetachInlineWriteH1_875(t *testing.T) {
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{{"std", celeris.Std}, {"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}, {"adaptive", celeris.Adaptive}} {
		for _, async := range []bool{false, true} {
			name := fmt.Sprintf("%s/async=%v", e.name, async)
			t.Run(name, func(t *testing.T) {
				release := make(chan struct{})
				var once sync.Once
				rel := func() { once.Do(func() { close(release) }) }
				returned := make(chan struct{})
				addr := startServerConfig761(t, celeris.Config{Engine: e.eng, Protocol: celeris.HTTP1}, func(s *celeris.Server) {
					r := s.GET("/flush875", func(c *celeris.Context) error {
						defer close(returned)
						sw := c.StreamWriter()
						if sw == nil {
							return fmt.Errorf("no StreamWriter")
						}
						done := c.Detach()
						defer done()
						defer func() { _ = sw.Close() }()
						if err := sw.WriteHeader(200, [][2]string{{"content-type", "text/plain"}}); err != nil {
							return err
						}
						if _, err := sw.Write([]byte(first875)); err != nil {
							return err
						}
						if err := sw.Flush(); err != nil {
							return err
						}
						select {
						case <-release:
						case <-time.After(20 * time.Second):
						}
						_, _ = sw.Write([]byte(second875))
						return nil
					})
					if async {
						r.Async()
					}
				})
				t.Cleanup(rel)
				early, full, err := flushProbe875(addr, rel)
				t.Logf("%s: detached, inline write: chunk before the handler returned = %v", name, early)
				if err != nil {
					t.Fatalf("read: %v (got %q)", err, full)
				}
				if !early {
					t.Errorf("a detached handler's chunk did not reach the client while the handler was still running")
				}
				if !strings.HasPrefix(full, "HTTP/1.1 200") || !strings.Contains(full, first875) || !strings.Contains(full, second875) {
					t.Fatalf("response incomplete after release: %q", full)
				}
				select {
				case <-returned:
				case <-time.After(5 * time.Second):
					t.Error("the handler did not return after release")
				}
			})
		}
	}
}

// What gives a raw StreamWriter a disconnect signal on epoll and io_uring over
// HTTP/1.1, where Write and Flush do not: Context.SetWSDetachClose and
// Context.SetWSErrorHandler, which the SSE middleware installs for exactly
// this (celeris#494). Despite the WS in their names they are plain
// detached-connection hooks, with no WebSocket upgrade needed. Installed
// before Detach, SetWSDetachClose must fire once the peer has reset the
// connection and must not fire while the peer is still there. The error
// handler is logged, not required (a close without an I/O error does not call
// it). The hooks run on the engine thread, so they only record.

func TestDetachedStreamPeerGoneHooksH1_875(t *testing.T) {
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{{"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}, {"adaptive", celeris.Adaptive}} {
		t.Run(e.name, func(t *testing.T) {
			var resetDone atomic.Bool
			var closeBefore, closeAfter, errBefore, errAfter atomic.Int32
			closed := make(chan struct{})
			var closedOnce sync.Once
			streamEnd := make(chan struct{})
			var endOnce sync.Once
			end := func() { endOnce.Do(func() { close(streamEnd) }) }
			addr := startServerConfig761(t, celeris.Config{Engine: e.eng, Protocol: celeris.HTTP1}, func(s *celeris.Server) {
				s.GET("/hooks875", func(c *celeris.Context) error {
					sw := c.StreamWriter()
					if sw == nil {
						return fmt.Errorf("no StreamWriter")
					}
					c.SetWSErrorHandler(func(error) {
						if resetDone.Load() {
							errAfter.Add(1)
						} else {
							errBefore.Add(1)
						}
					})
					c.SetWSDetachClose(func() {
						if resetDone.Load() {
							closeAfter.Add(1)
						} else {
							closeBefore.Add(1)
						}
						closedOnce.Do(func() { close(closed) })
					})
					done := c.Detach()
					go func() {
						defer done()
						_ = sw.WriteHeader(200, [][2]string{{"content-type", "text/plain"}})
						_, _ = sw.Write([]byte(first875))
						_ = sw.Flush()
						select {
						case <-closed:
						case <-streamEnd:
						case <-time.After(10 * time.Second):
						}
					}()
					return nil
				})
			})
			t.Cleanup(end) // after the server's cleanup, so it runs before Shutdown
			conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Write([]byte("GET /hooks875 HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			var got strings.Builder
			p := make([]byte, 4096)
			for !strings.Contains(got.String(), first875) {
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				n, rerr := conn.Read(p)
				got.Write(p[:n])
				if rerr != nil {
					t.Fatalf("read before the first chunk: %v (got %q)", rerr, got.String())
				}
			}
			// The peer is still there: nothing may have fired.
			time.Sleep(300 * time.Millisecond)
			if closeBefore.Load() != 0 || errBefore.Load() != 0 {
				t.Errorf("hooks fired while the peer was still connected: detach-close %d, error handler %d", closeBefore.Load(), errBefore.Load())
			}
			resetDone.Store(true)
			_ = conn.(*net.TCPConn).SetLinger(0)
			_ = conn.Close()
			select {
			case <-closed:
			case <-time.After(8 * time.Second):
				t.Errorf("SetWSDetachClose did not fire within 8s of the client resetting the connection")
			}
			end()
			t.Logf("%s: after the reset: detach-close fired %d, error handler fired %d; before: %d, %d",
				e.name, closeAfter.Load(), errAfter.Load(), closeBefore.Load(), errBefore.Load())
		})
	}
}

// A StreamWriter used without Detach on the native engines is held whole in
// memory until the handler returns (the first test above), and used to be
// held without a bound: one chunk of any size was appended to the connection's
// send buffer and reported written (celeris#884 item 45, celeris#885 item 30).
// The adapter now asks the engine before it stages a chunk, and the engine
// refuses a chunk that would take the response staged but not yet sent past
// 64 MiB (the limit a detached or HTTP/2 conn has): nothing of that chunk is
// staged, the Write returns the error and counts no bytes, and the connection
// is closed after what was staged before it has gone out, as it is for any
// refused write (celeris#761). The response is cut, and a client sees a
// chunked body with no last chunk. A response below the limit, 8 MiB streamed
// in celeris#761's test among them, is delivered whole when the handler
// returns, as before.

const (
	mib875 = 1 << 20
)

func TestStreamWriterOverCapH1_875(t *testing.T) {
	// wantOK is how many of the chunks the engine takes: -1 for all of them.
	// The limit is 67108864 bytes of staged response: 63 chunks of 1 MiB and
	// their framing fit, the 64th does not (64 * 1048586 > 64 MiB); a 60 MiB
	// chunk fits and an 8 MiB one behind it does not.
	cases := []struct {
		name   string
		chunks []int
		wantOK int
	}{
		{"1MiB", []int{1 * mib875}, -1},
		{"8MiB", []int{8 * mib875}, -1},
		{"63MiB", []int{63 * mib875}, -1},
		{"65MiB", []int{65 * mib875}, 0},
		{"60MiB+8MiB", []int{60 * mib875, 8 * mib875}, 1},
		{"70x1MiB", func() []int {
			c := make([]int, 70)
			for i := range c {
				c[i] = mib875
			}
			return c
		}(), 63},
	}
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{{"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}, {"adaptive", celeris.Adaptive}} {
		for _, async := range []bool{false, true} {
			for _, tc := range cases {
				name := fmt.Sprintf("%s/async=%v/%s", e.name, async, tc.name)
				t.Run(name, func(t *testing.T) {
					type outcome struct {
						ok      int   // chunks the Write took
						err     error // the first Write error
						written int64 // StreamWriter.BytesWritten at the end of the loop
						closeEr error
					}
					res := make(chan outcome, 1)
					addr := startServerConfig761(t, celeris.Config{Engine: e.eng, Protocol: celeris.HTTP1}, func(s *celeris.Server) {
						r := s.GET("/cap875", func(c *celeris.Context) error {
							sw := c.StreamWriter()
							if sw == nil {
								return fmt.Errorf("no StreamWriter")
							}
							if err := sw.WriteHeader(200, [][2]string{{"content-type", "application/octet-stream"}}); err != nil {
								res <- outcome{err: err}
								return nil
							}
							var o outcome
							for _, n := range tc.chunks {
								if _, err := sw.Write(make([]byte, n)); err != nil {
									o.err = err
									break
								}
								o.ok++
							}
							o.written = sw.BytesWritten()
							// A handler that ignores the error and goes on
							// must not grow the staged response further.
							if o.err != nil {
								for i := 0; i < 64; i++ {
									_, _ = sw.Write(make([]byte, mib875))
									_ = sw.Flush()
								}
							}
							o.closeEr = sw.Close()
							res <- o
							return nil
						})
						if async {
							r.Async()
						}
						s.GET("/ok875", func(c *celeris.Context) error { return c.String(200, "ok") })
					})
					conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = conn.Close() }()
					if _, err := conn.Write([]byte("GET /cap875 HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")); err != nil {
						t.Fatal(err)
					}
					_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
					n, tail, rerr := drain875(conn)
					var o outcome
					select {
					case o = <-res:
					case <-time.After(10 * time.Second):
						t.Fatal("the handler did not finish")
					}
					t.Logf("%s: %d of %d chunks taken, first error %v, BytesWritten %d, Close %v; client read %d bytes, read error %v",
						name, o.ok, len(tc.chunks), o.err, o.written, o.closeEr, n, rerr)
					if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
						t.Fatalf("the client's read timed out: the connection was neither completed nor closed (read %d bytes)", n)
					}
					wantOK := tc.wantOK
					if wantOK < 0 {
						wantOK = len(tc.chunks)
					}
					if o.ok != wantOK {
						t.Errorf("%d chunks were taken, want %d (error %v)", o.ok, wantOK, o.err)
					}
					if (wantOK < len(tc.chunks)) != (o.err != nil) {
						t.Errorf("first Write error = %v with %d of %d chunks taken", o.err, o.ok, len(tc.chunks))
					}
					// BytesWritten counts what Write returned nil for, and the
					// client was sent exactly that and the framing (the head is
					// under 1 KiB): a chunk Write reported refused is not staged.
					var accepted, framed int64
					for _, c := range tc.chunks[:wantOK] {
						accepted += int64(c)
						framed += int64(c) + int64(len(strconv.FormatInt(int64(c), 16))) + 4 // hex size, CRLF, data, CRLF
					}
					if o.written != accepted {
						t.Errorf("BytesWritten = %d, want %d", o.written, accepted)
					}
					if n < framed || n > framed+1024 {
						t.Errorf("the client read %d bytes, want the head and %d bytes of chunks: a refused chunk was staged, or the stream was lost", n, framed)
					}
					complete := strings.HasSuffix(tail, "0\r\n\r\n")
					if wantOK == len(tc.chunks) {
						if !complete || o.closeEr != nil {
							t.Errorf("a response below the limit was not delivered whole: complete=%v, Close %v", complete, o.closeEr)
						}
					} else {
						if complete {
							t.Errorf("the response past the limit was delivered whole; want it cut")
						}
						// Cut after the last chunk taken: the tail is that
						// chunk's closing CRLF and nothing else follows it.
						if !strings.HasSuffix(tail, "\r\n") || strings.Contains(tail, "HTTP/1.1") {
							t.Errorf("the cut response does not end at a chunk boundary: %q", tail)
						}
						if o.closeEr == nil {
							t.Errorf("Close on a refused stream returned nil")
						}
					}
					// The server serves the next connection either way.
					c2, err := net.DialTimeout("tcp", addr, 2*time.Second)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = c2.Close() }()
					_ = c2.SetReadDeadline(time.Now().Add(5 * time.Second))
					if _, err := c2.Write([]byte("GET /ok875 HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")); err != nil {
						t.Fatal(err)
					}
					b, _ := io.ReadAll(c2)
					if !strings.Contains(string(b), "ok") {
						t.Errorf("the server did not answer the next connection: %q", b)
					}
				})
			}
		}
	}
}

// drain875 reads r to EOF or error, returning the byte count and the last 16
// bytes seen.
func drain875(r io.Reader) (n int64, tail string, err error) {
	buf := make([]byte, 64<<10)
	var last []byte
	for {
		m, rerr := r.Read(buf)
		n += int64(m)
		if m > 0 {
			last = append(last, buf[:m]...)
			if len(last) > 16 {
				last = append(last[:0], last[len(last)-16:]...)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				rerr = nil
			}
			return n, string(last), rerr
		}
	}
}

// After Detach the write hook refuses a write once 64 MiB are unsent
// (maxPendingBytesDetached), and nothing read the refusal: the bytes were
// dropped, the Write returned nil, and the connection stayed open with a
// client that had stopped reading. The error query reports it, and the engine
// closes the connection. The client here never reads until the handler has
// been told.

func TestDetachedStreamOverCapClosesH1_875(t *testing.T) {
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{{"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}, {"adaptive", celeris.Adaptive}} {
		t.Run(e.name, func(t *testing.T) {
			const chunk = 24 << 20
			type result struct {
				err    error
				writes int
			}
			res := make(chan result, 1)
			addr := startServerConfig761(t, celeris.Config{Engine: e.eng, Protocol: celeris.HTTP1}, func(s *celeris.Server) {
				s.GET("/detcap875", func(c *celeris.Context) error {
					sw := c.StreamWriter()
					if sw == nil {
						return fmt.Errorf("no StreamWriter")
					}
					done := c.Detach()
					run := func() {
						defer done()
						if err := sw.WriteHeader(200, [][2]string{{"content-type", "application/octet-stream"}}); err != nil {
							res <- result{err: err}
							return
						}
						b := make([]byte, chunk)
						for n := 0; n < 16; n++ {
							if _, err := sw.Write(b); err != nil {
								res <- result{err: err, writes: n}
								return
							}
							if err := sw.Flush(); err != nil {
								res <- result{err: err, writes: n}
								return
							}
						}
						res <- result{writes: 16}
					}
					if c.EngineSupportsAsyncDetach() {
						go run()
						return nil
					}
					run()
					return nil
				})
			})
			conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if _, err := conn.Write([]byte("GET /detcap875 HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			var r result
			select {
			case r = <-res:
			case <-time.After(20 * time.Second):
				t.Fatal("the handler did not finish")
			}
			t.Logf("%s: first write/flush error = %v, after %d writes of %d MiB", e.name, r.err, r.writes, chunk>>20)
			if r.err == nil {
				t.Errorf("the stream of a client that never reads was never refused in %d writes of %d MiB", r.writes, chunk>>20)
			}
			// Now read: the connection must end (EOF or reset), not hang.
			_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
			n, _, rerr := drain875(conn)
			t.Logf("%s: client read %d bytes, then %v", e.name, n, rerr)
			if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
				t.Errorf("the connection stayed open after the stream was refused (read %d bytes)", n)
			}
		})
	}
}
