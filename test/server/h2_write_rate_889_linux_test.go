//go:build linux

package celeris_test

import (
	"fmt"
	"io"
	"strconv"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// The minimum rate of the write deadline must not cut a reader that is slow but
// moving (celeris#889, D2, review r7). Two ways it did:
//
//   - a client that sends few WINDOW_UPDATEs (nghttp2/curl update when half of the
//     window is consumed) is far above the floor on average but has no update in
//     some windows: progress counted only when an update arrived and nothing
//     carried over, so it was reset (curl --limit-rate 300k, WriteTimeout 1 s, was
//     cut after 7.9 s where main completes);
//   - parallel downloads that share a slow link are each judged on their share,
//     link/N, and were reset while the connection as a whole moved far above the
//     floor.
//
// The clients here mimic those policies with the raw framer; the connection's
// window is opened to 1 GiB, as Go's Transport and browsers do.

// batched889 downloads path with stream window `window`, "consuming" at bps and
// sending a WINDOW_UPDATE (stream and conn) whenever step bytes are consumed and
// not yet granted back. It reports how the stream ended, the bytes received and when.
func batched889(t *testing.T, addr, path string, window uint32, step int, bps float64, limit time.Duration) (string, int, time.Duration) {
	t.Helper()
	c := dialH2raw889(t, addr, h2dial889{initialWindow: window})
	c.window889(0, 1<<30-65535)
	_ = c.conn.SetReadDeadline(time.Time{})
	type ev struct {
		n   int
		end string
	}
	ch := make(chan ev, 4096)
	go func() {
		for {
			f, err := c.fr.ReadFrame()
			if err != nil {
				ch <- ev{end: "conn error: " + err.Error()}
				return
			}
			switch f := f.(type) {
			case *http2.DataFrame:
				if f.StreamID == 1 {
					e := ev{n: len(f.Data())}
					if f.StreamEnded() {
						e.end = "complete"
					}
					ch <- e
					if e.end != "" {
						return
					}
				}
			case *http2.RSTStreamFrame:
				if f.StreamID == 1 {
					ch <- ev{end: "RST " + f.ErrCode.String()}
					return
				}
			case *http2.GoAwayFrame:
				ch <- ev{end: "GOAWAY " + f.ErrCode.String()}
				return
			case *http2.PingFrame:
				if !f.IsAck() {
					c.wmu.Lock()
					_ = c.fr.WritePing(true, f.Data)
					c.wmu.Unlock()
				}
			}
		}
	}()
	c.request(1, "GET", path, true, true)
	t0 := time.Now()
	recv, acked := 0, 0
	tk := time.NewTicker(10 * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case e := <-ch:
			recv += e.n
			if e.end != "" {
				return e.end, recv, time.Since(t0)
			}
		case <-tk.C:
			el := time.Since(t0)
			if el > limit {
				return "still open at the limit", recv, el
			}
			consumed := min(int(bps*el.Seconds()), recv)
			if consumed-acked >= step {
				c.window889(1, uint32(consumed-acked))
				c.window889(0, uint32(consumed-acked))
				acked = consumed
			}
		}
	}
}

// TestH2SparseWindowUpdatesAreNotCut889: readers well above the floor (64 KiB/s at
// WriteTimeout 1 s) whose WINDOW_UPDATEs are further apart than WriteTimeout. The
// surplus released at one update carries over, up to a few windows' worth, and the
// bytes in flight when the stream stalls count for its first window, so a client
// that releases on average at least the floor survives. On the previous head each
// was reset at 1.0 s (or after the first gap).
func TestH2SparseWindowUpdatesAreNotCut889(t *testing.T) {
	type cell struct {
		name   string
		window uint32
		step   int
		bps    float64
		size   int
	}
	cells := []cell{
		// nghttp2's policy: update at half the window. 1 MiB window, 300 KB/s
		// (4.6x the floor): the gap between updates is 1.7 s.
		{"nghttp2-like/W1MiB/300KBps", 1 << 20, 1 << 19, 300e3, 2 << 20},
		// A larger window, a longer gap: half of 6 MiB at 1 MB/s is 3 s.
		{"halfwindow/W6MiB/1MBps", 6 << 20, 3 << 20, 1e6, 8 << 20},
		// Go's Transport: an update every 4 KiB. Guard, green before and after.
		{"go-like/W64KiB/100KBps", 65535, 4 << 10, 100e3, 512 << 10},
	}
	for _, e := range nativeEngines889() {
		for _, cl := range cells {
			t.Run(e.name+"/"+cl.name, func(t *testing.T) {
				t.Parallel()
				addr := wdServer889(t, e.eng, -1, -1, wd889)
				how, recv, at := batched889(t, addr, "/inline889?n="+strconv.Itoa(cl.size), cl.window, cl.step, cl.bps, 30*time.Second)
				t.Logf("WR889 sparse %s/%s: %s after %v, %d of %d bytes", e.name, cl.name, how, at.Round(time.Millisecond), recv, cl.size)
				if how != "complete" || recv != cl.size {
					t.Fatalf("a reader above the floor was cut: %s after %v, %d of %d bytes", how, at.Round(time.Millisecond), recv, cl.size)
				}
			})
		}
	}
}

// throttled889 reads from r at most bps bytes per second.
type throttled889 struct {
	r   io.Reader
	bps float64
	t0  time.Time
	n   int
}

func (t *throttled889) Read(p []byte) (int, error) {
	if len(p) > 4096 {
		p = p[:4096]
	}
	if want := time.Duration(float64(t.n) / t.bps * float64(time.Second)); time.Since(t.t0) < want {
		time.Sleep(want - time.Since(t.t0))
	}
	n, err := t.r.Read(p)
	t.n += n
	return n, err
}

// slowLink889 dials addr as a client that reads its socket at bps in total, opens
// the connection window to 1 GiB and grants a stream's window back 4 KiB at a time
// as it reads it (Go's Transport policy), with default 64 KiB stream windows.
// skip, if not nil, says which streams it does not grant at all (paused ones), and
// dribble which get a 1-byte update every 100 ms instead.
func slowLink889(t *testing.T, addr string, bps float64, ids []uint32, size int, skip, dribble map[uint32]bool) (res map[uint32]string, got map[uint32]int, at map[uint32]time.Duration) {
	t.Helper()
	c := dialH2raw889(t, addr, h2dial889{rcvBuf: 64 << 10})
	_ = c.conn.SetReadDeadline(time.Time{})
	c.window889(0, 1<<30-65535)
	fr := http2.NewFramer(c.conn, &throttled889{r: c.conn, bps: bps, t0: time.Now()})
	c.fr = fr
	for id := range dribble {
		id := id
		stop := every889(100*time.Millisecond, func() { c.window889(id, 1) })
		defer stop()
	}
	for _, id := range ids {
		c.request(id, "GET", "/inline889?n="+strconv.Itoa(size), true, true)
	}
	t0 := time.Now()
	got, res, at = map[uint32]int{}, map[uint32]string{}, map[uint32]time.Duration{}
	unacked := map[uint32]int{}
	want := len(ids)
	for len(res) < want && time.Since(t0) < 40*time.Second {
		f, err := fr.ReadFrame()
		if err != nil {
			t.Logf("WR889 slow link: conn error %v after %v", err, time.Since(t0).Round(time.Millisecond))
			break
		}
		switch f := f.(type) {
		case *http2.DataFrame:
			id := f.StreamID
			got[id] += len(f.Data())
			unacked[id] += len(f.Data())
			if f.StreamEnded() {
				res[id], at[id] = "complete", time.Since(t0)
			} else if unacked[id] >= 4096 && !skip[id] && !dribble[id] {
				c.window889(id, uint32(unacked[id]))
				unacked[id] = 0
			}
		case *http2.RSTStreamFrame:
			res[f.StreamID], at[f.StreamID] = "RST "+f.ErrCode.String(), time.Since(t0)
		case *http2.PingFrame:
			if !f.IsAck() {
				c.wmu.Lock()
				_ = fr.WritePing(true, f.Data)
				c.wmu.Unlock()
			}
		}
	}
	return res, got, at
}

// TestH2ParallelDownloadsSharingASlowLinkAreNotCut889: N downloads on one
// connection whose client reads its socket slowly get link/N each, below the
// per-stream floor, while the connection as a whole moves above it. Each is held
// as long as it takes part (a stream that gets nothing is bounded: see the next
// cell). On the previous head each was reset at 2.3 s.
func TestH2ParallelDownloadsSharingASlowLinkAreNotCut889(t *testing.T) {
	type cell struct {
		n    int
		bps  float64
		size int
	}
	// The last two end their responses while the link is still shared: the grant for
	// a stream's last bytes waits behind its siblings' bytes in the socket and lands
	// a moment after the window ends, when the stream (or the whole connection) holds
	// less than one floor and "released all of it" is the verdict (r8 F-A: reset 8 to
	// 16 bytes before the end of the file, after the whole transfer).
	for _, e := range nativeEngines889() {
		for _, cl := range []cell{{2, 100e3, 256 << 10}, {4, 200e3, 256 << 10}, {2, 100e3, 384 << 10}, {8, 200e3, 128 << 10}} {
			t.Run(fmt.Sprintf("%s/%dstreams/%.0fKBps/%dKiB", e.name, cl.n, cl.bps/1e3, cl.size>>10), func(t *testing.T) {
				t.Parallel()
				addr := wdServer889(t, e.eng, -1, -1, wd889)
				var ids []uint32
				for i := 0; i < cl.n; i++ {
					ids = append(ids, uint32(2*i+1))
				}
				res, got, at := slowLink889(t, addr, cl.bps, ids, cl.size, nil, nil)
				t.Logf("WR889 shared %s/%dstreams/%.0fKBps/%dKiB: %v %v at %v", e.name, cl.n, cl.bps/1e3, cl.size>>10, res, got, at)
				for _, id := range ids {
					if res[id] != "complete" || got[id] != cl.size {
						t.Fatalf("stream %d on a link shared by %d streams (the connection moved %.0f KB/s, above the floor): %q after %d of %d bytes at %v", id, cl.n, cl.bps/1e3, res[id], got[id], cl.size, at[id].Round(time.Millisecond))
					}
				}
			})
		}
	}
}

// TestH2PausedAndDribbledStreamsOnAHealthyConnAreBounded889: on a connection that
// moves well above the floor a stream the client does not read at all (no
// WINDOW_UPDATE), or feeds a byte every 100 ms, is held for a few windows (it may be
// waiting behind its siblings in the same socket) and reset after that: three
// windows with nothing of its own, so the fourth judgement, 4 s in. Its sibling,
// read at 250 KB/s, goes on. On the previous head it was reset at 1.0 s.
func TestH2PausedAndDribbledStreamsOnAHealthyConnAreBounded889(t *testing.T) {
	for _, e := range nativeEngines889() {
		for _, mode := range []string{"paused", "dribbled"} {
			t.Run(e.name+"/"+mode, func(t *testing.T) {
				t.Parallel()
				addr := wdServer889(t, e.eng, -1, -1, wd889)
				skip, dribble := map[uint32]bool{3: true}, map[uint32]bool(nil)
				if mode == "dribbled" {
					skip, dribble = nil, map[uint32]bool{3: true}
				}
				res, got, at := slowLink889(t, addr, 250e3, []uint32{1, 3}, 2<<20, skip, dribble)
				t.Logf("WR889 paused %s/%s: %v %v at %v", e.name, mode, res, got, at)
				if res[3] != "RST INTERNAL_ERROR" || at[3] < 3500*time.Millisecond || at[3] > 7*time.Second {
					t.Fatalf("stream 3 (%s): %q at %v, want RST INTERNAL_ERROR in [3.5 s, 7 s] (three windows of grace, then the verdict)", mode, res[3], at[3].Round(time.Millisecond))
				}
				if r := res[1]; r != "" && r != "complete" {
					t.Fatalf("the healthy sibling, stream 1, ended with %q", r)
				}
			})
		}
	}
}
