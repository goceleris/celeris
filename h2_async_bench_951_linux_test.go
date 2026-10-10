//go:build linux

package celeris_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
)

// BenchmarkH2AsyncGET951 drives the epoll engine over HTTP/2 (h2c, prior
// knowledge) with C connections that each send a batch of K GETs for an async
// route in one write and wait for the K responses: the shape of the h2 cells
// where the pool handler's stream release moved to the event loop
// (celeris#951). It builds against main and against the fix; the A/B runs the
// two test binaries interleaved (evidence/lanes-20261009/H2/scripts/ab.sh).
func BenchmarkH2AsyncGET951(b *testing.B) {
	const conns, batch = 4, 32
	var addr string
	var s *celeris.Server
	startDone := make(chan error, 1)
	for tries := 0; ; tries++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			b.Fatal(err)
		}
		addr = ln.Addr().String()
		_ = ln.Close()
		s = celeris.New(celeris.Config{Addr: addr, Engine: celeris.Epoll})
		s.GET("/a", func(c *celeris.Context) error { return c.String(200, "ok") }).Async()
		go func() { startDone <- s.Start() }()
		ready := false
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
				_ = c.Close()
				ready = true
				break
			}
		}
		if ready {
			break
		}
		if tries > 3 {
			b.Fatal("server did not start")
		}
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		<-startDone
	}()

	type client struct {
		conn net.Conn
		fr   *http2.Framer
		enc  *hpack.Encoder
		hb   *bytes.Buffer
		next uint32
	}
	cs := make([]*client, conns)
	for i := range cs {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			b.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		hb := new(bytes.Buffer)
		c := &client{conn: conn, fr: http2.NewFramer(conn, conn), enc: hpack.NewEncoder(hb), hb: hb, next: 1}
		if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
			b.Fatal(err)
		}
		if err := c.fr.WriteSettings(); err != nil {
			b.Fatal(err)
		}
		cs[i] = c
	}
	// round sends a batch and waits for its responses.
	round := func(c *client) error {
		var out bytes.Buffer
		w := http2.NewFramer(&out, nil)
		for range batch {
			c.hb.Reset()
			for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":authority", addr}, {":path", "/a"}, {"user-agent", "bench"}, {"accept", "*/*"}} {
				_ = c.enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
			}
			if err := w.WriteHeaders(http2.HeadersFrameParam{StreamID: c.next, BlockFragment: c.hb.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
				return err
			}
			c.next += 2
		}
		if _, err := c.conn.Write(out.Bytes()); err != nil {
			return err
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		for ended := 0; ended < batch; {
			f, err := c.fr.ReadFrame()
			if err != nil {
				return err
			}
			switch f := f.(type) {
			case *http2.SettingsFrame:
				if !f.IsAck() {
					_ = c.fr.WriteSettingsAck()
				}
			case *http2.DataFrame:
				if f.StreamEnded() {
					ended++
				}
			case *http2.RSTStreamFrame:
				return fmt.Errorf("stream %d reset: %v", f.StreamID, f.ErrCode)
			}
		}
		return nil
	}
	for _, c := range cs { // warm up: the connections are established and the pools are warm
		if err := round(c); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()
	for range b.N {
		var wg sync.WaitGroup
		errs := make([]error, conns)
		for i, c := range cs {
			wg.Add(1)
			go func() { defer wg.Done(); errs[i] = round(c) }()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(float64(b.N*conns*batch)/time.Since(start).Seconds(), "req/s")
}
