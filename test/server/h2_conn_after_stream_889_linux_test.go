//go:build linux

package celeris_test

import (
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris"
)

// The idle clock of an h2 conn after its last stream ends (celeris#889, L19).
// A conn with a stream the server owes is not closed by ReadTimeout or
// IdleTimeout, whose clock is the client's last frame, so when the stream ends
// after a long time that clock is long past both and a sweep closed the conn
// 2 to 5 ms later on epoll and adaptive (measured), with no GOAWAY: a client
// that was about to send its next request on the conn lost it. std starts its
// idle timer when the last stream closes. The conn is to be kept for the
// timeout from the stream's end, within a sweep, and reaped after it.

const (
	// streamLife889 is how long the stream lasts: two and a half timeouts.
	streamLife889 = 2500 * time.Millisecond
	// reuseAfter889 is when the client sends its next request after the end.
	reuseAfter889 = 400 * time.Millisecond
)

// streamEnd889 reads the conn until stream id ends and returns when.
func (c *h2raw889) streamEnd889(id uint32, d time.Duration) (time.Duration, error) {
	_ = c.conn.SetReadDeadline(c.t0.Add(d))
	for {
		f, err := c.fr.ReadFrame()
		if err != nil {
			return time.Since(c.t0), err
		}
		if df, ok := f.(*http2.DataFrame); ok && df.StreamID == id && df.StreamEnded() {
			return time.Since(c.t0), nil
		}
	}
}

// TestH2ConnIsReusableAfterALongStream889: a stream that outlives the timeout
// ends, and 400 ms later (well inside the timeout) the client sends its next
// request on the same conn, which is served.
func TestH2ConnIsReusableAfterALongStream889(t *testing.T) {
	for _, e := range nativeEngines889() {
		for _, arm := range []timeoutArm889{armRead889, armIdle889} {
			t.Run(e.name+"/"+arm.name, func(t *testing.T) {
				t.Parallel()
				var ended atomic.Bool
				addr := startServerConfig761(t, arm.config(e.eng, to889), func(s *celeris.Server) { holdRoute889(s, streamLife889, &ended) })
				c := dialH2raw889(t, addr, h2dial889{})
				c.request(1, "GET", "/hold889", true, true)
				end, err := c.streamEnd889(1, streamLife889+3*time.Second)
				if err != nil {
					t.Fatalf("the stream did not end (server %s timeout %v, a stream of %v): %v after %v", arm.name, to889, streamLife889, err, end.Round(time.Millisecond))
				}
				time.Sleep(reuseAfter889)
				c.request(3, "GET", "/ping", true, true)
				_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				for {
					f, err := c.fr.ReadFrame()
					if err != nil {
						t.Fatalf("the conn was closed before the client's next request, %v after the stream ended at %v (timeout %v): %v",
							reuseAfter889, end.Round(time.Millisecond), to889, err)
					}
					if hf, ok := f.(*http2.HeadersFrame); ok && hf.StreamID == 3 {
						t.Logf("H2REUSE889 engine=%s arm=%s: stream ended at %v, the next request %v later was answered", e.name, arm.name, end.Round(time.Millisecond), reuseAfter889)
						return
					}
				}
			})
		}
	}
}

// TestH2ConnIsKeptForTheTimeoutAfterALongStream889: and a silent client after
// the end is reaped, not sooner than about the timeout after the stream's end
// and within the usual bound after it.
func TestH2ConnIsKeptForTheTimeoutAfterALongStream889(t *testing.T) {
	for _, e := range nativeEngines889() {
		for _, arm := range []timeoutArm889{armRead889, armIdle889} {
			t.Run(e.name+"/"+arm.name, func(t *testing.T) {
				t.Parallel()
				var ended atomic.Bool
				addr := startServerConfig761(t, arm.config(e.eng, to889), func(s *celeris.Server) { holdRoute889(s, streamLife889, &ended) })
				c := dialH2raw889(t, addr, h2dial889{})
				c.request(1, "GET", "/hold889", true, true)
				end, err := c.streamEnd889(1, streamLife889+3*time.Second)
				if err != nil {
					t.Fatalf("the stream did not end: %v after %v", err, end.Round(time.Millisecond))
				}
				_ = c.conn.SetReadDeadline(time.Now().Add(to889 + reap889))
				var closedAfter time.Duration
				for {
					if _, err := c.fr.ReadFrame(); err != nil {
						closedAfter = time.Since(c.t0) - end
						break
					}
				}
				t.Logf("H2KEPT889 engine=%s arm=%s: stream ended at %v, conn closed %v after the end", e.name, arm.name, end.Round(time.Millisecond), closedAfter.Round(time.Millisecond))
				if closedAfter < to889*7/10 {
					t.Fatalf("the conn was closed %v after the stream ended, want it kept for about the timeout (%v)", closedAfter.Round(time.Millisecond), to889)
				}
				if closedAfter > to889+reap889 {
					t.Fatalf("the conn was still open %v after the stream ended, want it reaped within %v", closedAfter.Round(time.Millisecond), to889+reap889)
				}
			})
		}
	}
}
