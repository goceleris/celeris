//go:build linux

package iouring

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/protocol/h2/stream"
)

// celeris#751: with AsyncHandlers, the dispatch goroutine writes each response
// straight to the socket (unix.Write at the end of a runAsyncHandler
// iteration), and it did so without asking whether a ring SEND of the
// connection's earlier bytes was still in flight. Those bytes are the rest of
// the previous response: its direct write was short, the goroutine left the
// remainder in writeBuf, and the worker moved it to sendBuf and submitted a
// SEND. A pipelined request answered while that SEND is outstanding went out
// ahead of it, in the middle of the previous response. The h2c-upgrade exit's
// direct write of the 101 had the same gap. The detached conns' guarded
// writeFn has always refused the raw write while a ring SEND is outstanding.
//
// The test plays the kernel and the client on one conn of an fdlFixture: it
// captures the SEND the worker submits, performs it itself (writing sendBuf to
// the socket) when it chooses, and reads the client end between steps. The
// window is therefore an input: the client has drained the socket, so a
// direct write issued while the SEND is outstanding is admitted.

// orderBody751 is the first response's body: far larger than a socketpair's
// buffer, so the goroutine's direct write is short and the rest becomes a
// ring SEND. A repeating pattern, so a foreign byte sequence inside it is seen.
var orderBody751 = bytes.Repeat([]byte("0123456789abcdef"), (1<<20)/16)

type orderHandler751 struct{}

func (orderHandler751) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	body := []byte("ok")
	if s.Path == "/big" {
		body = orderBody751
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", strconv.Itoa(len(body))}}, body)
}

// orderClient751 is the client end of the fixture's socketpair, read
// non-blocking, so the test decides when the client reads.
type orderClient751 struct {
	t    *testing.T
	peer int
	got  []byte
}

// drain reads everything the socket holds now.
func (c *orderClient751) drain() int {
	c.t.Helper()
	n0 := len(c.got)
	var b [64 << 10]byte
	for {
		n, err := unix.Read(c.peer, b[:])
		if n > 0 {
			c.got = append(c.got, b[:n]...)
			continue
		}
		if err == unix.EAGAIN || err == unix.EINTR {
			return len(c.got) - n0
		}
		c.t.Fatalf("client read: %d, %v", n, err)
	}
}

// waitDispatchIdle waits until f's dispatch goroutine has run everything
// delivered to it and reached its park (or, with exited, has exited). Its
// write and its enqueue happen before either, so both are done on return.
func waitDispatchIdle751(t *testing.T, f *fdlFixture, exited bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		f.cs.asyncInMu.Lock()
		idle := len(f.cs.asyncInBuf) == 0 && (f.cs.asyncRun && f.cs.asyncParked || exited && !f.cs.asyncRun)
		f.cs.asyncInMu.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the dispatch goroutine did not finish its requests within 10s")
		}
	}
}

// kernelSend performs the SEND the worker has in flight the way the kernel
// does, writing cs.sendBuf to the socket (the client reads whenever the
// socket is full), and completes it. It then runs the worker's per-iteration
// work (the detach queue, the dirty list) and repeats while a SEND is in
// flight. It reports how many SENDs it performed.
func kernelSend751(t *testing.T, f *fdlFixture, c *orderClient751) int {
	t.Helper()
	sends := 0
	for {
		f.w.drainDetachQueue()
		f.w.flushDirty()
		for _, s := range takeSQEs(f.w.ring) {
			if s.op != opSEND && s.op != opRECV {
				t.Fatalf("the worker placed %v, want only SENDs and recv arms", s)
			}
		}
		if !f.cs.sending {
			return sends
		}
		if sends++; sends > 16 {
			t.Fatal("more than 16 SENDs for two responses")
		}
		for b := f.cs.sendBuf; len(b) > 0; {
			n, err := unix.Write(f.fd, b)
			if n > 0 {
				b = b[n:]
				continue
			}
			if err != unix.EAGAIN && err != unix.EINTR {
				t.Fatalf("kernel send: %v", err)
			}
			if c.drain() == 0 {
				t.Fatal("kernel send: the socket is full and the client has nothing to read")
			}
		}
		f.process(f.sendCQE())
	}
}

// TestAsyncResponseWaitsForAnInFlightRingSend: the second of two pipelined
// requests on a promoted async conn is answered while the ring SEND of the
// first response's tail is in flight. The client must receive the first
// response whole, then the second: arm response is a plain response, arm
// h2c_upgrade is the 101 of an h2c upgrade.
func TestAsyncResponseWaitsForAnInFlightRingSend(t *testing.T) {
	for _, arm := range []string{"response", "h2c_upgrade"} {
		t.Run(arm, func(t *testing.T) {
			f := newFDLFixture(t, true)
			f.w.handler = orderHandler751{}
			if arm == "h2c_upgrade" {
				f.w.cfg.EnableH2Upgrade = true
				f.w.h2cfg = conn.H2Config{MaxConcurrentStreams: 100, InitialWindowSize: 65535,
					MaxFrameSize: 16384, MaxRequestBodySize: 1 << 20}
				f.w.initProtocol(f.cs) // a fresh h1State with the upgrade on, as onAcceptedFD makes it
			}
			f.cs.asyncPromoted.Store(true)
			f.armFirstRecv()
			if err := unix.SetNonblock(f.peer, true); err != nil {
				t.Fatalf("nonblock: %v", err)
			}
			client := &orderClient751{t: t, peer: f.peer}
			t.Cleanup(func() {
				f.cs.asyncInMu.Lock()
				f.cs.asyncClosed.Store(true)
				f.cs.asyncCond.Broadcast()
				f.cs.asyncInMu.Unlock()
				f.w.asyncWG.Wait()
			})

			// Request 1: the goroutine's direct write fills the socket, and the
			// worker submits the rest as a ring SEND, which stays in flight.
			f.deliver("GET /big HTTP/1.1\r\nHost: x\r\n\r\n")
			waitDispatchIdle751(t, f, false)
			f.w.drainDetachQueue()
			f.w.flushDirty()
			var placed []sqeRec
			for _, s := range takeSQEs(f.w.ring) {
				if s.op != opRECV {
					placed = append(placed, s)
				}
			}
			if len(placed) != 1 || placed[0].op != opSEND || !f.cs.sending || len(f.cs.sendBuf) == 0 {
				t.Fatalf("apparatus: after request 1 the worker placed %v (sending=%v, %d bytes to send), want one "+
					"SEND of the response's tail", placed, f.cs.sending, len(f.cs.sendBuf))
			}
			tail := len(f.cs.sendBuf)

			// The client reads what the direct write delivered: the socket is empty
			// again, and the SEND is still the kernel's.
			head := client.drain()

			// Request 2, answered while the SEND is outstanding.
			req2 := "GET /small HTTP/1.1\r\nHost: x\r\n\r\n"
			if arm == "h2c_upgrade" {
				req2 = h2cUpgradeHead722("GET", 0)
			}
			f.deliver(req2)
			waitDispatchIdle751(t, f, arm == "h2c_upgrade")
			early := client.drain()

			// The kernel completes the SEND (and any the worker submits after it).
			sends := kernelSend751(t, f, client)
			client.drain()
			t.Logf("celeris751 ORDER arm=%s head=%d tail=%d early=%d sends=%d total=%d",
				arm, head, tail, early, sends, len(client.got))
			if early != 0 {
				t.Errorf("celeris#751: %d bytes of the answer to request 2 reached the client while the ring SEND "+
					"of response 1's last %d bytes was still in flight", early, tail)
			}

			br := bufio.NewReader(bytes.NewReader(client.got))
			r1, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("response 1: %v", err)
			}
			b1, err := io.ReadAll(r1.Body)
			if err != nil || !bytes.Equal(b1, orderBody751) {
				at := 0
				for at < min(len(b1), len(orderBody751)) && b1[at] == orderBody751[at] {
					at++
				}
				t.Fatalf("celeris#751: response 1's body is not intact (%d bytes, err %v; first difference at "+
					"byte %d: %q): the next response was written into it", len(b1), err, at,
					b1[at:min(at+48, len(b1))])
			}
			r2, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("celeris#751: the answer to request 2 does not follow response 1: %v", err)
			}
			want := 200
			if arm == "h2c_upgrade" {
				want = http.StatusSwitchingProtocols
			}
			if r2.StatusCode != want {
				t.Errorf("request 2 got status %d, want %d", r2.StatusCode, want)
			}
			if arm == "response" {
				if b2, err := io.ReadAll(r2.Body); err != nil || string(b2) != "ok" {
					t.Errorf("response 2 body %q, err %v; want \"ok\"", b2, err)
				}
				if rest, _ := io.ReadAll(br); len(rest) != 0 {
					t.Errorf("%d bytes after response 2: %q", len(rest), rest[:min(48, len(rest))])
				}
			}
		})
	}
}
