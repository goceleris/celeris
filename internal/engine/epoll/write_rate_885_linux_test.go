//go:build linux

package epoll

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/resource"
)

// celeris#885 item 51, the minimum-rate rule (parkedBelowRate): a response a
// keep-alive conn has parked on the socket is held another WriteTimeout window
// if the peer acknowledged at least parkedMinAcked (64 KiB) in the last one, and
// closed if it did not; nothing the peer sends moves it, and there is no
// elapsed-time ceiling. WriteTimeout <= 0 is no write timeout at all.
//
// The rig tests drive the real checkTimeouts, armEpollOut and handleWritable on
// a real TCP pair (parked885: a 256 KiB send buffer, a 16 KiB receive buffer on
// the peer, 8 MiB queued); the engine tests are the reviewer's probes of the
// first design of this item (celeris PR 1116, whose IdleTimeout ceiling cut a
// download at IdleTimeout and was refreshed by any inbound byte).

// rateFloor885 is the floor of the rule, 64 KiB per window, as the tests state
// it (parkedMinAcked in the engine: the tests do not read the constant, so that
// they build against main, where they fail).
const rateFloor885 = 64 << 10

// TestParkedPeerAboveTheFloorWithoutAnEdgeIsNotReaped885: the peer takes 16 KiB
// every 100 ms (about 160 KiB/s, 128 KiB per 800 ms window, twice the floor)
// for five windows, no EPOLLOUT edge (handleWritable is never called) and no
// flush: the write clock the flushes keep is at its first stamp. The conn must
// survive every sweep (main closed it a WriteTimeout in). Then the peer stops,
// and the conn goes within two windows of its last byte.
func TestParkedPeerAboveTheFloorWithoutAnEdgeIsNotReaped885(t *testing.T) {
	const bound = 800 * time.Millisecond
	r := parked885(t, resource.Config{ReadTimeout: -1, IdleTimeout: -1, WriteTimeout: bound})
	start := time.Now()
	lastRead := start
	for time.Since(start) < 5*bound {
		time.Sleep(25 * time.Millisecond)
		if time.Since(lastRead) >= 100*time.Millisecond {
			if r.read(16<<10) == 0 && time.Since(start) < bound/2 {
				t.Fatalf("apparatus: the peer read nothing")
			}
			lastRead = time.Now()
			// What the kernel took is what the peer can read: the engine's
			// own flush keeps it supplied. A flush that is no EPOLLOUT edge
			// (flushWrites, not handleWritable) is not progress for the
			// flush clock either, which is the premise of this test.
			_ = r.l.flushWrites(r.cs, true)
		}
		r.l.checkTimeouts()
		if !r.open() {
			t.Fatalf("celeris#885 item 51: reaped %v in, the peer taking 16 KiB every 100 ms (twice the floor of %d KiB per %v) and no edge",
				time.Since(start).Round(time.Millisecond), rateFloor885>>10, bound)
		}
	}
	stopped := time.Now()
	for time.Since(stopped) < 4*bound {
		time.Sleep(25 * time.Millisecond)
		r.l.checkTimeouts()
		if !r.open() {
			if gone := time.Since(stopped); gone > 2*bound+200*time.Millisecond {
				t.Errorf("a peer that stopped was reaped %v after its last byte, want within two windows (%v)", gone.Round(time.Millisecond), 2*bound)
			}
			return
		}
	}
	t.Fatalf("a peer that stopped reading was not reaped %v after its last byte", 4*bound)
}

// TestParkedPeerBelowTheFloorIsReapedWhateverItsFlushes885: the peer takes 12 KiB
// every 300 ms (40 KiB/s, 24 KiB per 600 ms window, under the floor) and every
// read is followed by an EPOLLOUT edge, handleWritable, whose flush takes bytes
// and restarts the flush clock; the acknowledged bytes per window are what count,
// and the conn is gone within three windows of parking.
func TestParkedPeerBelowTheFloorIsReapedWhateverItsFlushes885(t *testing.T) {
	const bound = 600 * time.Millisecond
	r := parked885(t, resource.Config{ReadTimeout: -1, IdleTimeout: -1, WriteTimeout: bound})
	start := time.Now()
	for time.Since(start) < 6*bound {
		time.Sleep(300 * time.Millisecond)
		r.read(12 << 10)
		r.l.handleWritable(r.cs)
		r.l.checkTimeouts()
		if !r.open() {
			if gone := time.Since(start); gone > 3*bound {
				t.Errorf("reaped %v in, want within three windows (%v)", gone.Round(time.Millisecond), 3*bound)
			}
			return
		}
	}
	t.Fatalf("celeris#885 item 51: the conn of a peer taking 12 KiB every 300 ms (under the floor of %d KiB per %v), flushing at every edge, "+
		"was still held %v in", rateFloor885>>10, bound, 6*bound)
}

// TestParkedConnWithNoWriteTimeoutIsNeverReapedByTheRate885: WriteTimeout -1 is
// no write timeout, at any rate: a peer that takes nothing for 2 s, with
// ReadTimeout and IdleTimeout off, is still held. (The same on main.)
func TestParkedConnWithNoWriteTimeoutIsNeverReapedByTheRate885(t *testing.T) {
	r := parked885(t, resource.Config{ReadTimeout: -1, IdleTimeout: -1, WriteTimeout: -1})
	for start := time.Now(); time.Since(start) < 2*time.Second; {
		time.Sleep(50 * time.Millisecond)
		r.l.checkTimeouts()
		if !r.open() {
			t.Fatalf("the conn was reaped %v in with WriteTimeout -1, ReadTimeout and IdleTimeout off", time.Since(start).Round(time.Millisecond))
		}
	}
}

// goneAfter885 polls the engine until the conn is gone, up to limit, and reports
// when (0: still open).
func goneAfter885(e *Engine, t0 time.Time, limit time.Duration, between func()) time.Duration {
	for time.Since(t0) < limit {
		time.Sleep(20 * time.Millisecond)
		if between != nil {
			between()
		}
		if e.Metrics().ActiveConnections == 0 {
			return time.Since(t0)
		}
	}
	return 0
}

// TestEngineDownloadAtAModerateRateCompletesUnderIdleTimeout885 is the first
// design's blocker (celeris PR 1116, probe P1): a 12 MiB keep-alive response
// read at about 2 MiB/s (64 KiB every 30 ms), ReadTimeout off, IdleTimeout 2 s,
// WriteTimeout 60 s and -1. The response takes about 6 s, three times
// IdleTimeout, and the client takes it steadily: it must arrive whole. The first
// design closed it at IdleTimeout, a download of any size at any rate.
func TestEngineDownloadAtAModerateRateCompletesUnderIdleTimeout885(t *testing.T) {
	for _, wt := range []time.Duration{-1, 60 * time.Second} {
		for _, async := range []bool{false, true} {
			t.Run(fmt.Sprintf("wt=%v/async=%v", wt, async), func(t *testing.T) {
				const body = 12 << 20
				_, addr := startE2E885(t, async, newPathHandler885(body), func(c *resource.Config) {
					c.ReadTimeout = -1
					c.IdleTimeout = 2 * time.Second
					c.WriteTimeout = wt
				})
				c := dialRcvBuf885(t, addr, 64<<10)
				get885(t, c, "/big")
				t0 := time.Now()
				_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
				resp, err := http.ReadResponse(bufio.NewReader(c), nil)
				if err != nil {
					t.Fatalf("response head: %v", err)
				}
				var got int64
				buf := make([]byte, 64<<10)
				for {
					n, err := resp.Body.Read(buf)
					got += int64(n)
					if err != nil {
						break
					}
					time.Sleep(30 * time.Millisecond)
				}
				took := time.Since(t0)
				t.Logf("celeris885 download wt=%v async=%v: %d of %d bytes in %v", wt, async, got, body, took.Round(10*time.Millisecond))
				if got != body {
					t.Errorf("the client received %d of %d bytes %v in, reading steadily at about 2 MiB/s, with IdleTimeout 2s: "+
						"a response is not an idle connection", got, body, took.Round(10*time.Millisecond))
				}
				if took < 4*time.Second {
					t.Errorf("apparatus: the transfer took %v, not well over IdleTimeout", took)
				}
			})
		}
	}
}

// TestEngineReaderAboveTheFloorIsNotClosedBetweenEdges885: a deep send queue
// (1 MiB, so that half of it takes the client eight seconds), a client with a 4
// KiB receive buffer reading 4 KiB every 30 ms (about 130 KiB/s), WriteTimeout 1
// s, ReadTimeout and IdleTimeout off. It takes twice the 64 KiB per window and
// no EPOLLOUT edge comes; it is held for five seconds. Main closes it at the
// first WriteTimeout.
func TestEngineReaderAboveTheFloorIsNotClosedBetweenEdges885(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(fmt.Sprintf("async=%v", async), func(t *testing.T) {
			e, addr := startE2E885(t, async, newPathHandler885(64<<20), func(c *resource.Config) {
				c.ReadTimeout = -1
				c.IdleTimeout = -1
				c.WriteTimeout = time.Second
				c.Resources.SocketSend = 1 << 20
			})
			c := dialRcvBuf885(t, addr, 4096)
			get885(t, c, "/big")
			t0 := time.Now()
			var got int64
			var lastRead time.Time
			buf := make([]byte, 4096)
			gone := goneAfter885(e, t0, 5*time.Second, func() {
				if time.Since(lastRead) >= 30*time.Millisecond {
					lastRead = time.Now()
					_ = c.SetReadDeadline(time.Now().Add(time.Second))
					n, _ := c.Read(buf)
					got += int64(n)
				}
			})
			t.Logf("celeris885 above-floor async=%v: %d bytes in %v, gone at %v", async, got, time.Since(t0).Round(time.Millisecond), gone)
			if gone != 0 {
				t.Errorf("the conn of a client reading steadily at %d KiB in %v was closed %v in, WriteTimeout 1s: "+
					"progress between two EPOLLOUT edges is progress", got>>10, time.Since(t0).Round(time.Second), gone.Round(time.Millisecond))
			}
		})
	}
}

// TestEngineSlowlorisBelowTheFloorIsClosed885 is the issue's hold: a shallow
// send queue (16 KiB), so that every half queue the client takes is an EPOLLOUT
// edge and a flush that restarts the flush clock, and a client reading 4, 8, 12
// and 16 KiB in turn every 250 ms (40 KiB/s) of a 16 MiB response, WriteTimeout
// 1 s, ReadTimeout and IdleTimeout off. Main holds it for ever: the issue's
// probe held it for the 10 s it watched. The rate is under the floor of 64 KiB
// per window: the conn is closed within three windows.
func TestEngineSlowlorisBelowTheFloorIsClosed885(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(fmt.Sprintf("async=%v", async), func(t *testing.T) {
			e, addr := startE2E885(t, async, newPathHandler885(16<<20), func(c *resource.Config) {
				c.ReadTimeout = -1
				c.IdleTimeout = -1
				c.WriteTimeout = time.Second
				c.Resources.SocketSend = 16 << 10
			})
			c := dialRcvBuf885(t, addr, 4096)
			get885(t, c, "/big")
			t0 := time.Now()
			buf := make([]byte, 16<<10)
			var lastRead time.Time
			turn := 0
			gone := goneAfter885(e, t0, 8*time.Second, func() {
				if time.Since(lastRead) >= 250*time.Millisecond {
					lastRead = time.Now()
					turn++
					_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
					_, _ = c.Read(buf[:4096*(1+turn%4)])
				}
			})
			t.Logf("celeris885 slowloris async=%v: gone at %v", async, gone)
			if gone == 0 {
				t.Errorf("a client taking 40 KiB/s of a response, flushing at every edge, WriteTimeout 1s, was still held 8 s in: " +
					"nothing bounds a parked response whose peer takes a half queue per window")
			} else if gone > 3500*time.Millisecond {
				t.Errorf("the conn was closed %v in, want within three windows (3.5 s)", gone.Round(time.Millisecond))
			}
		})
	}
}

// TestEnginePeerInboundBytesDoNotRefreshTheWriteClock885 is the first design's
// M1 (probe P2d): a deep queue, WriteTimeout 1 s, IdleTimeout 3 s, ReadTimeout
// off, a client reading 4 KiB every 400 ms (10 KiB/s) of a 3 MiB response that
// also sends nothing, a bare CRLF every 2 s (the reviewer's probe), or a pipelined
// GET every 400 ms. The rate is under the floor: the conn is gone within 3 s in
// every case, a window and the sweeps after the response parked, whatever the
// client sends (the first design held the CRLF and the GET clients for 15 s; main
// holds the GET client for ever).
//
// A bare CRLF is not a request: the engine answers it as a request error and
// closes the conn once its queue has gone, so that conn is in the closing drain
// (celeris#876), whose clock restarts at every byte the client acknowledges and
// which this rule does not change (the PR body says so). The CRLF client is
// closed here because its first CRLF comes after the first window has passed;
// one sent every 400 ms would hold the conn in the closing drain.
func TestEnginePeerInboundBytesDoNotRefreshTheWriteClock885(t *testing.T) {
	for _, p := range []struct {
		poke  string
		every time.Duration
	}{{"", 0}, {"\r\n", 2 * time.Second}, {"GET /small HTTP/1.1\r\nHost: x\r\n\r\n", 400 * time.Millisecond}} {
		poke := p.poke
		for _, async := range []bool{false, true} {
			t.Run(fmt.Sprintf("poke=%q/async=%v", poke, async), func(t *testing.T) {
				e, addr := startE2E885(t, async, newPathHandler885(3<<20), func(c *resource.Config) {
					c.ReadTimeout = -1
					c.IdleTimeout = 3 * time.Second
					c.WriteTimeout = time.Second
					c.Resources.SocketSend = 256 << 10
				})
				c := dialRcvBuf885(t, addr, 4096)
				get885(t, c, "/big")
				t0 := time.Now()
				buf := make([]byte, 4096)
				var lastRead, lastPoke time.Time
				gone := goneAfter885(e, t0, 8*time.Second, func() {
					if time.Since(lastRead) >= 400*time.Millisecond {
						lastRead = time.Now()
						_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
						_, _ = c.Read(buf)
					}
					if poke != "" && time.Since(lastPoke) >= p.every {
						lastPoke = time.Now()
						_ = c.SetWriteDeadline(time.Now().Add(time.Second))
						_, _ = io.WriteString(c, poke)
					}
				})
				t.Logf("celeris885 inbound poke=%q async=%v: gone at %v", poke, async, gone)
				if gone == 0 || gone > 3*time.Second {
					t.Errorf("a client reading 10 KiB/s and sending %q was closed at %v (0: still open), want within 3 s: "+
						"the bytes it sends are not progress", poke, gone)
				}
			})
		}
	}
}

// TestEngineStuckPeersAreClosedWithinTwoWindows885: a client that takes nothing
// (a zero window, also with a pipelined GET sent every second) and one that reads
// a byte every 900 ms (the kernel acknowledges a window update, not a byte), with
// WriteTimeout 1 s: both gone within 3 s.
func TestEngineStuckPeersAreClosedWithinTwoWindows885(t *testing.T) {
	for _, name := range []string{"zero-window", "zero-window+GET/s", "1B/900ms"} {
		t.Run(name, func(t *testing.T) {
			e, addr := startE2E885(t, false, newPathHandler885(16<<20), func(c *resource.Config) {
				c.ReadTimeout = -1
				c.IdleTimeout = -1
				c.WriteTimeout = time.Second
				c.Resources.SocketSend = 256 << 10
			})
			c := dialRcvBuf885(t, addr, 4096)
			get885(t, c, "/big")
			t0 := time.Now()
			var lastAct time.Time
			one := make([]byte, 1)
			gone := goneAfter885(e, t0, 6*time.Second, func() {
				switch name {
				case "zero-window+GET/s":
					if time.Since(lastAct) >= time.Second {
						lastAct = time.Now()
						_ = c.SetWriteDeadline(time.Now().Add(time.Second))
						_, _ = io.WriteString(c, "GET /small HTTP/1.1\r\nHost: x\r\n\r\n")
					}
				case "1B/900ms":
					if time.Since(lastAct) >= 900*time.Millisecond {
						lastAct = time.Now()
						_ = c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
						_, _ = c.Read(one)
					}
				}
			})
			t.Logf("celeris885 stuck %s: gone at %v", name, gone)
			if gone == 0 || gone > 3*time.Second {
				t.Errorf("%s: closed at %v (0: still open), want within 3 s (two windows)", name, gone)
			}
		})
	}
}

// TestEngineNoWriteTimeoutHoldsAStuckPeer885: WriteTimeout -1, ReadTimeout and
// IdleTimeout off: a client that takes nothing for 4 s is still connected. (The
// same on main.)
func TestEngineNoWriteTimeoutHoldsAStuckPeer885(t *testing.T) {
	e, addr := startE2E885(t, false, newPathHandler885(16<<20), func(c *resource.Config) {
		c.ReadTimeout = -1
		c.IdleTimeout = -1
		c.WriteTimeout = -1
		c.Resources.SocketSend = 256 << 10
	})
	c := dialRcvBuf885(t, addr, 4096)
	get885(t, c, "/big")
	if gone := goneAfter885(e, time.Now(), 4*time.Second, nil); gone != 0 {
		t.Errorf("the conn of a client that takes nothing was closed %v in with WriteTimeout -1 and ReadTimeout and IdleTimeout off", gone)
	}
}
