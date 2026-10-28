//go:build linux

package celeris_test

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris"
)

// The per-stream write deadline of the native engines (celeris#889, L19 D2).
//
// A response whose bytes wait for the client's flow-control window (an inline
// handler's tail, buffered in the stream; a pool handler's) is bounded by
// WriteTimeout as a MINIMUM RATE, the rule #1119 gives a response parked on the
// socket: a stream that releases at least min(64 KiB, what it holds) of its DATA
// in a WriteTimeout window is held another window; one that does not is reset
// with RST_STREAM(INTERNAL_ERROR), as net/http resets a stream at its write
// deadline. No ceiling: a stream that takes its floor in every window is never
// cut. Progress is bytes released, never frames: a PING, or a WINDOW_UPDATE of one
// byte, is not progress. A stream whose own window has room but whose bytes wait
// for the CONNECTION window is credited with what the whole conn released. A
// conn with such a stream is exempt from ReadTimeout and IdleTimeout while the
// stream lives (the exemption of WriteTimeout > 0 replaces the old rule that
// closed the conn of a silent client, A2).
//
// Server: WriteTimeout 1 s unless a cell says so; ReadTimeout and IdleTimeout
// off. The client sends its request, takes the first window's DATA and then only
// what the cell says.

const wd889 = time.Second

// wdServer889 is the server of these cells: /inline889?n= an inline handler's
// body of n bytes (the tail beyond the windows is buffered on the loop), /pool889
// the same from a pool handler, /tick889 an SSE-like stream of small chunks.
func wdServer889(t *testing.T, eng celeris.EngineType, read, idle, write time.Duration) string {
	t.Helper()
	cfg := celeris.Config{Engine: eng, ReadTimeout: read, IdleTimeout: idle, WriteTimeout: write}
	var ended atomic.Bool
	return startServerConfig761(t, cfg, func(s *celeris.Server) {
		tickRoute889(s, "/tick889", 500*time.Millisecond, 4*time.Second, &ended)
		s.GET("/inline889", func(c *celeris.Context) error {
			n, _ := strconv.Atoi(c.Query("n"))
			return c.Blob(200, "application/octet-stream", make([]byte, n))
		})
		s.GET("/pool889", func(c *celeris.Context) error {
			n, _ := strconv.Atoi(c.Query("n"))
			return c.Blob(200, "application/octet-stream", make([]byte, n))
		}).Async()
	})
}

// wdPaths889 are the two ways a response comes to wait: from the loop and from
// the pool.
var wdPaths889 = []string{"inline889", "pool889"}

func (c *h2raw889) window889(id, n uint32) {
	c.wmu.Lock()
	_ = c.fr.WriteWindowUpdate(id, n)
	c.wmu.Unlock()
}

// every889 runs f every d until the returned stop is called.
func every889(d time.Duration, f func()) (stop func()) {
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(d)
		defer tk.Stop()
		for {
			select {
			case <-quit:
				return
			case <-tk.C:
				f()
			}
		}
	}()
	return func() { close(quit); <-done }
}

func wantWriteReset889(t *testing.T, ev rdEvent889, lo, hi time.Duration) {
	t.Helper()
	if ev.timeout || ev.connEnd != "" || !ev.reset {
		t.Fatalf("want the stalled stream reset with RST_STREAM(INTERNAL_ERROR) in [%v, %v], conn open; got %s", lo, hi, ev)
	}
	if ev.rst != http2.ErrCodeInternal {
		t.Fatalf("the stream was reset with %v, want INTERNAL_ERROR (what net/http and #906 send at a write deadline)", ev.rst)
	}
	if ev.at < lo || ev.at > hi {
		t.Fatalf("the stream was reset at %v, want it in [%v, %v]", ev.at.Round(time.Millisecond), lo, hi)
	}
}

// TestH2StalledTailIsResetAtWriteTimeout889: a 4 MiB body, the client takes the
// first 65535 bytes (the initial window) and grants nothing, pinging every 300 ms:
// the stream is reset with INTERNAL_ERROR WriteTimeout after its tail was
// buffered, and the conn serves the next request, also after the client's late
// frames for the reset stream (WINDOW_UPDATE, RST_STREAM, PRIORITY). On main the
// stream lived for as long as the client pinged.
func TestH2StalledTailIsResetAtWriteTimeout889(t *testing.T) {
	for _, e := range nativeEngines889() {
		for _, path := range wdPaths889 {
			t.Run(e.name+"/"+path, func(t *testing.T) {
				t.Parallel()
				c := dialH2raw889(t, wdServer889(t, e.eng, -1, -1, wd889), h2dial889{})
				c.request(1, "GET", "/"+path+"?n=4194304", true, true)
				stop := c.pingEvery889(300 * time.Millisecond)
				defer stop()
				ev := c.await889(1, 6*time.Second)
				t.Logf("WD889 stalled engine=%s path=%s: %s", e.name, path, ev)
				wantWriteReset889(t, ev, 900*time.Millisecond, 2500*time.Millisecond)
				if ev.data < 60000 || ev.data > 70000 {
					t.Fatalf("%d DATA bytes before the reset, want the first window (65535)", ev.data)
				}
				// Late frames for the stream the server reset keep the conn.
				c.window889(1, 1000)
				c.wmu.Lock()
				_ = c.fr.WritePriority(1, http2.PriorityParam{Weight: 15, StreamDep: 0})
				_ = c.fr.WriteRSTStream(1, http2.ErrCodeCancel)
				c.wmu.Unlock()
				c.serves889(3)
			})
		}
	}
}

// TestH2WindowUpdateDribblerIsResetAtTheFirstJudgement889: a WINDOW_UPDATE of one
// byte on the stream and on the connection every 100 ms releases a byte at a
// time, 600 bytes in 60 s at that rate: the stream is reset at the first
// judgement, WriteTimeout after the tail was buffered, as a client that grants
// nothing is.
func TestH2WindowUpdateDribblerIsResetAtTheFirstJudgement889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, wdServer889(t, e.eng, -1, -1, wd889), h2dial889{})
			c.request(1, "GET", "/inline889?n=4194304", true, true)
			stop := every889(100*time.Millisecond, func() { c.window889(1, 1); c.window889(0, 1) })
			defer stop()
			ev := c.await889(1, 6*time.Second)
			t.Logf("WD889 dribble engine=%s: %s", e.name, ev)
			wantWriteReset889(t, ev, 900*time.Millisecond, 2500*time.Millisecond)
		})
	}
}

// TestH2WriteRateFloor889: the floor is min(64 KiB, what the stream holds) per
// window. A client that grants 65535 bytes (one less) once is reset at the first
// judgement; one that grants 65536 in each of three windows is held for three
// windows and reset in the first window it grants nothing in.
func TestH2WriteRateFloor889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name+"/floor-1", func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, wdServer889(t, e.eng, -1, -1, wd889), h2dial889{})
			c.request(1, "GET", "/inline889?n=4194304", true, true)
			time.AfterFunc(300*time.Millisecond, func() { c.window889(1, 65535); c.window889(0, 65535) })
			ev := c.await889(1, 6*time.Second)
			t.Logf("WD889 floor-1 engine=%s: %s", e.name, ev)
			wantWriteReset889(t, ev, 900*time.Millisecond, 2500*time.Millisecond)
		})
		t.Run(e.name+"/floor", func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, wdServer889(t, e.eng, -1, -1, wd889), h2dial889{})
			c.request(1, "GET", "/inline889?n=4194304", true, true)
			for k := range 3 { // in the middle of windows 1, 2 and 3
				time.AfterFunc(time.Duration(500+1000*k)*time.Millisecond, func() { c.window889(1, 65536); c.window889(0, 65536) })
			}
			ev := c.await889(1, 9*time.Second)
			t.Logf("WD889 floor engine=%s: %s", e.name, ev)
			wantWriteReset889(t, ev, 3000*time.Millisecond, 5500*time.Millisecond)
		})
	}
}

// TestH2ConnWindowStarvedStreamsAreJudgedPerConnection889: two streams, each with
// room in its own window (the client's SETTINGS_INITIAL_WINDOW_SIZE is large) and
// the connection window shut. The conn window is the only thing they wait for, so
// a stream's credit is what the whole conn released. A dribble of one byte
// resets both at the first judgement; 128 KiB a second held for three windows
// holds both, though one sibling takes them all.
func TestH2ConnWindowStarvedStreamsAreJudgedPerConnection889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name+"/dribble", func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, wdServer889(t, e.eng, -1, -1, wd889), h2dial889{initialWindow: 1 << 30})
			c.request(1, "GET", "/inline889?n=2097152", true, true)
			c.request(3, "GET", "/inline889?n=2097152", true, true)
			stop := every889(100*time.Millisecond, func() { c.window889(0, 1) })
			defer stop()
			got := map[uint32]bool{}
			var last rdEvent889
			for len(got) < 2 {
				ev := c.awaitAny889([]uint32{1, 3}, 6*time.Second)
				last = ev
				if !ev.reset {
					t.Fatalf("a starved stream was not reset (got %s, reset so far: %v)", ev, got)
				}
				got[ev.id] = true
			}
			t.Logf("WD889 conn-dribble engine=%s: both reset, the last at %v", e.name, last.at.Round(time.Millisecond))
			if last.at > 2500*time.Millisecond {
				t.Fatalf("the second stream was reset at %v, want both at the first judgement (about 1 s)", last.at)
			}
		})
		t.Run(e.name+"/credited", func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, wdServer889(t, e.eng, -1, -1, wd889), h2dial889{initialWindow: 1 << 30})
			c.request(1, "GET", "/inline889?n=2097152", true, true)
			c.request(3, "GET", "/inline889?n=2097152", true, true)
			var n atomic.Int32
			stop := every889(700*time.Millisecond, func() {
				if n.Add(1) <= 5 { // 128 KiB to the conn, about 3.5 s of it
					c.window889(0, 131072)
				}
			})
			defer stop()
			ev := c.awaitAny889([]uint32{1, 3}, 9*time.Second)
			t.Logf("WD889 conn-credited engine=%s: stream %d %s", e.name, ev.id, ev)
			if !ev.reset || ev.rst != http2.ErrCodeInternal {
				t.Fatalf("want a reset with INTERNAL_ERROR once the grants stop, got %s", ev)
			}
			if ev.at < 3000*time.Millisecond {
				t.Fatalf("a stream was reset at %v, while the conn released 128 KiB in every window (the starved sibling is credited with it)", ev.at.Round(time.Millisecond))
			}
		})
	}
}

// TestH2FlowingDownloadAndQuietStreamAreNotCut889: guards, green before and after.
// A body of 8 MiB taken at about 3 MB/s (longer than WriteTimeout, three windows:
// no ceiling) arrives whole; an SSE-like stream with window to spare, quiet
// between its chunks, is not touched.
func TestH2FlowingDownloadAndQuietStreamAreNotCut889(t *testing.T) {
	for _, e := range nativeEngines889() {
		for _, path := range wdPaths889 {
			t.Run(e.name+"/"+path, func(t *testing.T) {
				t.Parallel()
				c := dialH2raw889(t, wdServer889(t, e.eng, -1, -1, wd889), h2dial889{})
				const total = 8 << 20
				c.request(1, "GET", "/"+path+"?n="+strconv.Itoa(total), true, true)
				got, err := c.takeAll889(total, func(n int) {
					c.window889(0, uint32(n))
					c.window889(1, uint32(n))
					time.Sleep(time.Duration(n/3) * time.Microsecond) // about 3 MB/s
				})
				t.Logf("WD889 flowing engine=%s path=%s: %d of %d bytes, err %v, at %v", e.name, path, got, total, err, time.Since(c.t0).Round(time.Millisecond))
				if err != nil || got != total {
					t.Fatalf("a download that took its floor in every window was cut after %d of %d bytes: %v", got, total, err)
				}
			})
		}
		t.Run(e.name+"/quiet-sse", func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, wdServer889(t, e.eng, -1, -1, wd889), h2dial889{})
			c.request(1, "GET", "/tick889", true, true)
			ev := c.await889(1, 6*time.Second)
			t.Logf("WD889 quiet engine=%s: %s", e.name, ev)
			if ev.reset || ev.connEnd != "" {
				t.Fatalf("a quiet stream with window to spare was cut: %s", ev)
			}
		})
	}
}

// TestH2SilentClientWithAStalledResponseKeepsTheConnUntilTheStreamIsReset889:
// ReadTimeout 1 s, WriteTimeout 3 s (the read side is the shorter, so the cell can
// tell whether the stalled stream exempts the conn), a client that sends nothing
// after its GET. The old rule (A2) closed the whole conn at ReadTimeout without a
// word; now the stream is reset at WriteTimeout, with INTERNAL_ERROR, before the
// conn goes.
func TestH2SilentClientWithAStalledResponseKeepsTheConnUntilTheStreamIsReset889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, wdServer889(t, e.eng, time.Second, -1, 3*time.Second), h2dial889{})
			c.request(1, "GET", "/inline889?n=4194304", true, true)
			ev := c.await889(1, 8*time.Second)
			t.Logf("WD889 silent engine=%s: %s", e.name, ev)
			wantWriteReset889(t, ev, 2800*time.Millisecond, 5000*time.Millisecond)
		})
	}
}
