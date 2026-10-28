//go:build linux

package celeris_test

import (
	"strconv"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// The progress cap of the idle clock (celeris#889, L19 D0) must cut a conn whose
// output is not going out and must not cut one whose client is reading, however
// slowly. The first version of the cap counted the bytes the conn held (its
// buffers plus the kernel's send queue); on io_uring that count included the SEND
// in flight and the kernel's queue for the same bytes, went up while the client
// read, and a client taking 500 KB/s was reset at 1 MiB of 4 (review r5). The
// measure is now the bytes the peer acknowledged. Server: IdleTimeout 1 s.

// TestH2IdleClockDoesNotCutAReaderThatReads889: a plain client (default socket
// buffers, no PING, a WINDOW_UPDATE for every DATA frame) reads a 2 MiB and a
// 4 MiB body at about 500 KB/s, 4 and 8 s, with IdleTimeout 1 s: the body arrives
// whole, no reset.
func TestH2IdleClockDoesNotCutAReaderThatReads889(t *testing.T) {
	for _, e := range nativeEngines889() {
		for _, total := range []int{2 << 20, 4 << 20} {
			t.Run(e.name+"/"+strconv.Itoa(total>>20)+"MiB", func(t *testing.T) {
				t.Parallel()
				c := dialH2raw889(t, tailServer889(t, e.eng), h2dial889{initialWindow: 1 << 30})
				c.wmu.Lock()
				_ = c.fr.WriteWindowUpdate(0, 1<<30)
				c.wmu.Unlock()
				c.request(1, "GET", "/tail889?n="+strconv.Itoa(total), true, true)
				got, err := c.takeAll889(total, func(n int) {
					c.wmu.Lock()
					_ = c.fr.WriteWindowUpdate(0, uint32(n))
					c.wmu.Unlock()
					time.Sleep(time.Duration(n*2) * time.Microsecond) // about 500 KB/s
				})
				t.Logf("PROGRESS889 steady engine=%s: %d of %d bytes, err %v, at %v", e.name, got, total, err, time.Since(c.t0).Round(time.Millisecond))
				if err != nil || got != total {
					t.Fatalf("a client reading at 500 KB/s was cut after %d of %d bytes: %v", got, total, err)
				}
			})
		}
	}
}

// TestH2IdleClockDoesNotCutABurstyReader889: a reader that takes 64 KiB, waits
// 500 ms and takes 64 KiB again (a PING every 300 ms, a small receive buffer: the
// kernel holds only a little, so the tail is in the server) gets a 2 MiB body
// whole with IdleTimeout 1 s: the bytes it takes in every idle period are the
// progress.
func TestH2IdleClockDoesNotCutABurstyReader889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, tailServer889(t, e.eng), h2dial889{initialWindow: 1 << 30, rcvBuf: 4096})
			c.wmu.Lock()
			_ = c.fr.WriteWindowUpdate(0, 1<<30)
			c.wmu.Unlock()
			const total = 2 << 20
			c.request(1, "GET", "/tail889?n="+strconv.Itoa(total), true, true)
			stop := c.pingEvery889(300 * time.Millisecond)
			defer stop()
			since := 0
			got, err := c.takeAll889(total, func(n int) {
				since += n
				if since >= 64<<10 {
					since = 0
					time.Sleep(500 * time.Millisecond)
				}
			})
			t.Logf("PROGRESS889 bursty engine=%s: %d of %d bytes, err %v, at %v", e.name, got, total, err, time.Since(c.t0).Round(time.Millisecond))
			if err != nil || got != total {
				t.Fatalf("a client taking 64 KiB every 500 ms was cut after %d of %d bytes: %v", got, total, err)
			}
		})
	}
}

// TestH2IdleCloseSendsGoAway889: a client that reads everything and pings every
// 300 ms, one small GET, IdleTimeout 1 s: three conns per engine are each closed
// with a GOAWAY(NO_ERROR) before the close, naming the stream the server
// processed. The io_uring close once reaped the conn in the sweep that staged the
// GOAWAY, before its SEND was submitted: none of five had it.
func TestH2IdleCloseSendsGoAway889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			addr := tailServer889(t, e.eng)
			for i := range 3 {
				c := dialH2raw889(t, addr, h2dial889{})
				c.request(1, "GET", "/tail889?n=100", true, true)
				c.t0 = time.Now()
				stop := c.pingEvery889(300 * time.Millisecond)
				at, closed := c.closedAfter889(5 * time.Second)
				stop()
				t.Logf("PROGRESS889 goaway engine=%s #%d: closed=%v at %v, GOAWAY seen=%v code=%v", e.name, i, closed, at.Round(time.Millisecond), c.gaSeen, c.gaCode)
				if !closed {
					t.Fatalf("conn %d not closed after %v", i, at)
				}
				c.wantIdleGoAway889(t)
			}
		})
	}
}

// TestH2FramesOnAResetStreamAreNotActivity889: a client opens a request without
// :path and without END_STREAM (the server resets it), and then sends one byte of
// DATA on that stream every 300 ms for ever. The server ignores the DATA, as the
// RFC asks for frames in flight, but it is not stream activity for the idle
// clock: with IdleTimeout 1 s the conn is closed about 1 s after the reset. A
// frame for a stream the server reset for another cause than an expired request
// used to restamp the clock and the cap's baseline for as long as the client
// liked, at no cost to it.
func TestH2FramesOnAResetStreamAreNotActivity889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, tailServer889(t, e.eng), h2dial889{})
			c.wmu.Lock()
			noPath := []byte{0x82, 0x86, 0x41, 0x01, 'x'} // :method GET, :scheme http, :authority x; no :path
			err := c.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: noPath, EndStream: false, EndHeaders: true})
			c.wmu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			c.t0 = time.Now()
			stop := c.trickle889(1, 300*time.Millisecond)
			defer stop()
			at, closed := c.closedAfter889(6 * time.Second)
			t.Logf("PROGRESS889 reset-stream engine=%s: closed=%v at %v", e.name, closed, at.Round(time.Millisecond))
			if !closed || at > 4*time.Second {
				t.Fatalf("a client sending DATA on a stream the server reset held the conn: closed=%v at %v, want it closed by about 1 to 2 s", closed, at.Round(time.Millisecond))
			}
		})
	}
}
