//go:build linux

package celeris_test

import (
	"net"
	"strconv"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris"
)

// The idle clock of an h2 conn with no stream open must not run while the conn
// still has a response to send (celeris#889, found by the review of the first
// version of D0). The stream of a response is gone from the manager as soon as
// its last DATA is in the conn's buffers, which for a client with a wide window
// (Go's Transport grants 4 MiB per stream) is the whole tail of a large body:
// the bytes are still the server's to send, in its write buffer and in the
// kernel's send queue, and a client that is receiving them (WINDOW_UPDATEs) or
// keeping the conn alive (PINGs) is not idle. Closing the conn then, with the
// client's frames unread in the receive queue, is a TCP reset that discards the
// send queue too: the download is cut. Server: IdleTimeout 1 s.

func tailServer889(t *testing.T, eng celeris.EngineType) string {
	t.Helper()
	cfg := celeris.Config{Engine: eng, ReadTimeout: -1, IdleTimeout: time.Second, WriteTimeout: 30 * time.Second}
	return startServerConfig761(t, cfg, func(s *celeris.Server) {
		s.GET("/tail889", func(c *celeris.Context) error {
			n, _ := strconv.Atoi(c.Query("n"))
			return c.Blob(200, "application/octet-stream", make([]byte, n))
		})
	})
}

// takeAll889 reads the body of stream 1 to its end and returns the bytes, or the
// error that cut it. perFrame runs after each DATA frame.
func (c *h2raw889) takeAll889(total int, perFrame func(n int)) (int, error) {
	got := 0
	_ = c.conn.SetReadDeadline(time.Now().Add(40 * time.Second))
	for {
		f, err := c.fr.ReadFrame()
		if err != nil {
			return got, err
		}
		switch f := f.(type) {
		case *http2.DataFrame:
			if f.StreamID != 1 {
				continue
			}
			n := len(f.Data())
			got += n
			if f.StreamEnded() {
				return got, nil
			}
			if perFrame != nil && n > 0 {
				perFrame(n)
			}
		case *http2.RSTStreamFrame:
			return got, errReset889(f.ErrCode)
		}
	}
}

type errReset889 http2.ErrCode

func (e errReset889) Error() string { return "RST_STREAM " + http2.ErrCode(e).String() }

// TestH2IdleClockDoesNotCutAFinishedResponseOfAPingingClient889: a GET of 8 MiB
// with wide windows, the client sends a PING every 300 ms, reads nothing for
// 2.5 s (IdleTimeout is 1 s) and then reads: the body arrives whole. On main the
// PINGs restamped the conn's clock and it did.
func TestH2IdleClockDoesNotCutAFinishedResponseOfAPingingClient889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, tailServer889(t, e.eng), h2dial889{initialWindow: 1 << 30})
			if tc, ok := c.conn.(*net.TCPConn); ok {
				_ = tc.SetReadBuffer(32 << 10)
			}
			c.wmu.Lock()
			_ = c.fr.WriteWindowUpdate(0, 1<<30)
			c.wmu.Unlock()
			const total = 8 << 20
			c.request(1, "GET", "/tail889?n="+strconv.Itoa(total), true, true)
			stop := c.pingEvery889(300 * time.Millisecond)
			defer stop()
			time.Sleep(2500 * time.Millisecond)
			got, err := c.takeAll889(total, nil)
			t.Logf("TAIL889 ping engine=%s: %d of %d bytes, err %v, at %v", e.name, got, total, err, time.Since(c.t0).Round(time.Millisecond))
			if err != nil || got != total {
				t.Fatalf("the download of a pinging client was cut after %d of %d bytes: %v", got, total, err)
			}
		})
	}
}

// TestH2IdleClockDoesNotCutASlowReader889: the plain case, no PING: a 4 MiB
// stream window, a client that reads about 1 MB/s and sends WINDOW_UPDATEs for
// every DATA frame (what every client does): 8 MiB take about 8 s, IdleTimeout
// is 1 s; the tail of the body, in the buffers and in the kernel's queue after
// the stream has ended on the server's side, goes on being taken.
func TestH2IdleClockDoesNotCutASlowReader889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, tailServer889(t, e.eng), h2dial889{initialWindow: 4 << 20})
			c.wmu.Lock()
			_ = c.fr.WriteWindowUpdate(0, 1<<30)
			c.wmu.Unlock()
			const total = 8 << 20
			c.request(1, "GET", "/tail889?n="+strconv.Itoa(total), true, true)
			got, err := c.takeAll889(total, func(n int) {
				c.wmu.Lock()
				_ = c.fr.WriteWindowUpdate(0, uint32(n))
				_ = c.fr.WriteWindowUpdate(1, uint32(n))
				c.wmu.Unlock()
				time.Sleep(time.Duration(n) * time.Microsecond) // about 1 MB/s
			})
			t.Logf("TAIL889 wu engine=%s: %d of %d bytes, err %v, at %v", e.name, got, total, err, time.Since(c.t0).Round(time.Millisecond))
			if err != nil || got != total {
				t.Fatalf("the download of a slow reader was cut after %d of %d bytes: %v", got, total, err)
			}
		})
	}
}
