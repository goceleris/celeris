//go:build linux

package celeris_test

import (
	"bufio"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestEpollStaticFileDownloadAtAModerateRateCompletes885 is the sendfile half of
// the first design's blocker (celeris PR 1116): a 12 MiB file sent with c.File
// (sendfile(2)) to a client reading about 2 MiB/s (64 KiB every 30 ms), IdleTimeout 2 s,
// WriteTimeout -1 and 60 s. The transfer takes about 6 s, three times
// IdleTimeout, and the client takes it steadily: it must arrive whole (the first
// design cut it at IdleTimeout: EOF at 3.9 s with IdleTimeout 3 s, 8.1 MB of 24).
func TestEpollStaticFileDownloadAtAModerateRateCompletes885(t *testing.T) {
	dir := t.TempDir()
	const size = 12 << 20
	if err := os.WriteFile(filepath.Join(dir, "f"), make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, wt := range []time.Duration{-1, 60 * time.Second} {
		t.Run(wt.String(), func(t *testing.T) {
			addr := startServerConfig421(t, celeris.Config{Engine: celeris.Epoll, WriteTimeout: wt, IdleTimeout: 2 * time.Second, ReadTimeout: -1}, func(s *celeris.Server) {
				s.GET("/f", func(c *celeris.Context) error { return c.File(filepath.Join(dir, "f")) })
			})
			c, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			if _, err := c.Write([]byte("GET /f HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
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
			t.Logf("celeris885 sendfile wt=%v: %d of %d bytes in %v (Content-Length %s)", wt, got, size, took.Round(10*time.Millisecond), strconv.FormatInt(resp.ContentLength, 10))
			if got != size {
				t.Errorf("the client received %d of %d bytes of the file %v in, reading steadily, with IdleTimeout 2s", got, size, took.Round(10*time.Millisecond))
			}
			if took < 4*time.Second {
				t.Errorf("apparatus: the transfer took %v, not well over IdleTimeout", took)
			}
		})
	}
}
