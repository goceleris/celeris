//go:build linux

package celeris_test

import (
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris"
)

// The header-block deadline (celeris#889, L19 D3). A header block is the
// connection's, not a stream's: HPACK state is shared, so a half-read block
// cannot be skipped. A client that starts one (HEADERS without END_HEADERS) and
// dribbles CONTINUATION frames, each of which restarts the connection's clock,
// held the connection for as long as maxContinuationFrames (32) of them took.
// ReadHeaderTimeout, from the first HEADERS frame of the block to END_HEADERS,
// bounds it: the connection ends with GOAWAY(ENHANCE_YOUR_CALM), the peer was slow,
// not malformed.

func hbServer889(t *testing.T, eng celeris.EngineType, rht time.Duration) string {
	t.Helper()
	cfg := celeris.Config{Engine: eng, ReadTimeout: 30 * time.Second, ReadHeaderTimeout: rht, IdleTimeout: -1, WriteTimeout: 30 * time.Second}
	return startServerConfig761(t, cfg, func(*celeris.Server) {})
}

// TestH2HeaderBlockDribbleIsClosedWithEnhanceYourCalm889: HEADERS, then one byte
// of CONTINUATION every 300 ms, ReadHeaderTimeout 1 s.
func TestH2HeaderBlockDribbleIsClosedWithEnhanceYourCalm889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, hbServer889(t, e.eng, time.Second), h2dial889{})
			blk := c.block("GET", "/ping")
			c.wmu.Lock()
			err := c.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: blk[:1], EndStream: true, EndHeaders: false})
			c.wmu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			c.t0 = time.Now()
			quit := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				tk := time.NewTicker(300 * time.Millisecond)
				defer tk.Stop()
				for i := 1; i < len(blk)-1; i++ {
					select {
					case <-quit:
						return
					case <-tk.C:
						c.wmu.Lock()
						_ = c.fr.WriteContinuation(1, false, blk[i:i+1])
						c.wmu.Unlock()
					}
				}
			}()
			ev := c.await889(1, 5*time.Second)
			close(quit)
			<-done
			t.Logf("RD889 hblock engine=%s: %s", e.name, ev)
			if ev.connEnd != "GOAWAY ENHANCE_YOUR_CALM" {
				t.Fatalf("want GOAWAY(ENHANCE_YOUR_CALM) for a header block still open ReadHeaderTimeout after its first frame, got %s", ev)
			}
			if ev.at < 900*time.Millisecond || ev.at > 2500*time.Millisecond {
				t.Fatalf("the GOAWAY came at %v, want it at about ReadHeaderTimeout (1 s)", ev.at.Round(time.Millisecond))
			}
			// And the conn is closed after it.
			_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			for {
				if _, err := c.fr.ReadFrame(); err != nil {
					break
				}
			}
		})
	}
}

// TestH2FragmentedHeaderBlockInTimeIsServed889: a block split over HEADERS and
// CONTINUATION frames that completes inside ReadHeaderTimeout is served, at once
// and after a pause of 0.5 s; and with ReadHeaderTimeout off (-1) a dribble is
// not closed.
func TestH2FragmentedHeaderBlockInTimeIsServed889(t *testing.T) {
	for _, e := range nativeEngines889() {
		for _, pause := range []time.Duration{0, 500 * time.Millisecond} {
			t.Run(e.name+"/pause-"+pause.String(), func(t *testing.T) {
				t.Parallel()
				c := dialH2raw889(t, hbServer889(t, e.eng, time.Second), h2dial889{})
				blk := c.block("GET", "/ping")
				c.wmu.Lock()
				err := c.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: blk[:3], EndStream: true, EndHeaders: false})
				c.wmu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				c.t0 = time.Now()
				time.Sleep(pause)
				c.wmu.Lock()
				err = c.fr.WriteContinuation(1, true, blk[3:])
				c.wmu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				ev := c.await889(1, 4*time.Second)
				if ev.status != "200" || ev.connEnd != "" {
					t.Fatalf("a header block completed after %v (ReadHeaderTimeout 1 s) must be served: %s", pause, ev)
				}
			})
		}
		t.Run(e.name+"/off", func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, hbServer889(t, e.eng, -1), h2dial889{})
			blk := c.block("GET", "/ping")
			c.wmu.Lock()
			err := c.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: blk[:1], EndStream: true, EndHeaders: false})
			c.wmu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			c.t0 = time.Now()
			quit := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				tk := time.NewTicker(300 * time.Millisecond)
				defer tk.Stop()
				for i := 1; i < 6; i++ {
					select {
					case <-quit:
						return
					case <-tk.C:
						c.wmu.Lock()
						_ = c.fr.WriteContinuation(1, false, blk[i:i+1])
						c.wmu.Unlock()
					}
				}
			}()
			ev := c.await889(1, 2500*time.Millisecond)
			close(quit)
			<-done
			if !ev.timeout || ev.connEnd != "" {
				t.Fatalf("with ReadHeaderTimeout off a dribble for 2.5 s must not end the conn: %s", ev)
			}
		})
	}
}
