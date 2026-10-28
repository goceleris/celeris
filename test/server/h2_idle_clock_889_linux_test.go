//go:build linux

package celeris_test

import (
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris"
)

// The idle clock of an h2 connection with no stream open (celeris#889, L19 D0).
// net/http's idle timer runs from the last stream's close and no frame touches
// it, a PING included; on the native engines every read restamped the
// connection's clock, so a client that only pings held a bare connection (its fd,
// HPACK tables, buffers) for ever. IdleTimeout, for a connection with no stream
// open, now runs from the last stream activity the sweep saw, and a PING, a
// SETTINGS or a connection WINDOW_UPDATE does not move it. It is IdleTimeout
// alone, as net/http's is: ReadTimeout is a request's deadline, not a bare
// connection's, and an h2 client that keeps a connection idle with PINGs under
// the default IdleTimeout (600 s) is left alone as before.

func idleServer889(t *testing.T, eng celeris.EngineType, read, idle time.Duration) string {
	t.Helper()
	cfg := celeris.Config{Engine: eng, ReadTimeout: read, IdleTimeout: idle, WriteTimeout: 30 * time.Second}
	return rdServerCfg889(t, cfg)
}

// rdServerCfg889 is rdServer889 with a whole Config.
func rdServerCfg889(t *testing.T, cfg celeris.Config) string {
	t.Helper()
	return startServerConfig761(t, cfg, func(s *celeris.Server) {
		s.POST("/upload889", func(c *celeris.Context) error { return c.String(200, "got %d", len(c.Body())) })
	})
}

// closedAfter889 reads until the conn ends and returns when (since c.t0), or a
// timeout.
func (c *h2raw889) closedAfter889(d time.Duration) (time.Duration, bool) {
	_ = c.conn.SetReadDeadline(c.t0.Add(d))
	for {
		f, err := c.fr.ReadFrame()
		if err != nil {
			return time.Since(c.t0), !isTimeout889(err)
		}
		if g, ok := f.(*http2.GoAwayFrame); ok {
			c.gaSeen, c.gaCode = true, g.ErrCode
		}
	}
}

// wantIdleGoAway889 checks that the conn ended with a GOAWAY(NO_ERROR) before it
// closed, as the idle close of D0 sends (celeris#889; a bare close left the
// client unsure what the server had processed). The cells that read until the
// end accepted a bare EOF for a while, and the io_uring close lost its GOAWAY
// without any of them noticing.
func (c *h2raw889) wantIdleGoAway889(t *testing.T) {
	t.Helper()
	if !c.gaSeen || c.gaCode != http2.ErrCodeNo {
		t.Fatalf("the conn was closed as idle without GOAWAY(NO_ERROR) before the close (seen %v, code %v)", c.gaSeen, c.gaCode)
	}
}

// TestH2PingOnlyConnIsClosedAtIdleTimeout889: after its last stream a client
// pings every 300 ms; IdleTimeout is 1 s: the conn is closed at about 1 s after
// the stream's end, as net/http closes it.
func TestH2PingOnlyConnIsClosedAtIdleTimeout889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name+"/after-a-request", func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, idleServer889(t, e.eng, -1, time.Second), h2dial889{})
			c.serves889(1)
			c.t0 = time.Now()
			stop := c.pingEvery889(300 * time.Millisecond)
			defer stop()
			at, closed := c.closedAfter889(5 * time.Second)
			t.Logf("RD889 idle engine=%s after a request: closed=%v at %v", e.name, closed, at.Round(time.Millisecond))
			if !closed || at < 800*time.Millisecond || at > 3*time.Second {
				t.Fatalf("a PING-only conn (IdleTimeout 1 s) was closed=%v at %v, want it closed at about 1 s", closed, at.Round(time.Millisecond))
			}
			c.wantIdleGoAway889(t)
		})
		t.Run(e.name+"/from-the-start", func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, idleServer889(t, e.eng, -1, time.Second), h2dial889{})
			c.t0 = time.Now()
			stop := c.pingEvery889(300 * time.Millisecond)
			defer stop()
			at, closed := c.closedAfter889(5 * time.Second)
			t.Logf("RD889 idle engine=%s from the start: closed=%v at %v", e.name, closed, at.Round(time.Millisecond))
			if !closed || at < 800*time.Millisecond || at > 3*time.Second {
				t.Fatalf("a PING-only conn from the start (IdleTimeout 1 s) was closed=%v at %v, want it closed at about 1 s", closed, at.Round(time.Millisecond))
			}
			c.wantIdleGoAway889(t)
		})
		// A client that goes on using the conn is not idle: a request every 500 ms.
		t.Run(e.name+"/busy", func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, idleServer889(t, e.eng, -1, time.Second), h2dial889{})
			for i := range 6 {
				c.serves889(uint32(1 + 2*i))
				time.Sleep(500 * time.Millisecond)
			}
		})
		// IdleTimeout off: nothing closes a PING-only conn, as before and as
		// net/http (ReadTimeout is a request's deadline). Pinned, with the
		// documented consequence.
		t.Run(e.name+"/idle-off", func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, idleServer889(t, e.eng, time.Second, -1), h2dial889{})
			c.serves889(1)
			c.t0 = time.Now()
			stop := c.pingEvery889(300 * time.Millisecond)
			defer stop()
			at, closed := c.closedAfter889(3 * time.Second)
			if closed {
				t.Fatalf("IdleTimeout is off, ReadTimeout 1 s: a PING-only conn was closed at %v (pinned: ReadTimeout does not apply to it)", at.Round(time.Millisecond))
			}
		})
	}
}

// TestH2ExpiredRequestThenPingOnlyIsClosedAtIdleTimeout889: the previous
// scenario after an expiry: a request that was never completed is reset at
// ReadTimeout (1 s), and the client then only pings: the conn is closed at
// IdleTimeout (2 s) after that stream's activity ended, not kept for ever.
func TestH2ExpiredRequestThenPingOnlyIsClosedAtIdleTimeout889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, idleServer889(t, e.eng, time.Second, 2*time.Second), h2dial889{})
			c.request(1, "POST", "/upload889", false, true)
			stop := c.pingEvery889(300 * time.Millisecond)
			defer stop()
			ev := c.await889(1, 4*time.Second)
			wantReset889(t, ev, 900*time.Millisecond, 2500*time.Millisecond)
			at, closed := c.closedAfter889(8 * time.Second)
			t.Logf("RD889 idle engine=%s after an expiry: closed=%v at %v", e.name, closed, at.Round(time.Millisecond))
			if !closed || at > 6*time.Second {
				t.Fatalf("a PING-only conn after an expired request was closed=%v at %v, want it closed by about 3 s", closed, at.Round(time.Millisecond))
			}
			c.wantIdleGoAway889(t)
		})
	}
}

// TestH2UploaderStillSendingAfterTheResetKeepsTheConn889: the slow uploader's DATA
// for the stream the server reset is ignored, and it is activity: with
// IdleTimeout 1 s a conn with no stream open and an uploader that goes on for
// 3 s after the reset is not closed.
func TestH2UploaderStillSendingAfterTheResetKeepsTheConn889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, idleServer889(t, e.eng, time.Second, time.Second), h2dial889{})
			c.request(1, "POST", "/upload889", false, true)
			stop := c.trickle889(1, 250*time.Millisecond)
			defer stop()
			ev := c.await889(1, 4*time.Second)
			wantReset889(t, ev, 900*time.Millisecond, 2500*time.Millisecond)
			c.t0 = time.Now()
			at, closed := c.closedAfter889(3 * time.Second)
			if closed {
				t.Fatalf("the conn was closed at %v while the uploader was still sending DATA for its reset stream", at.Round(time.Millisecond))
			}
		})
	}
}
