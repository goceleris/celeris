//go:build linux

package celeris_test

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// BenchmarkAutoNewConnRequest is one whole short conn in Auto mode: connect,
// one GET, the answer, close. Every op takes the accept path that arms the
// pre-detection deadline, the first read that detects the protocol, and the
// H1 header deadline that detection arms from it. It is the benchmark of the
// per-conn cost of that path (RULE 74); the per-request path is covered by the
// engines' keep-alive benchmarks and the parser's. The client closes with a
// reset (SO_LINGER 0) so that a run does not exhaust the ephemeral ports.
func BenchmarkAutoNewConnRequest(b *testing.B) {
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{{"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}} {
		b.Run(e.name, func(b *testing.B) {
			addr := startServerConfig761(b, celeris.Config{Engine: e.eng, Protocol: celeris.Auto, ReadHeaderTimeout: 10 * time.Second}, func(*celeris.Server) {})
			const req = "GET /ping HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"
			buf := make([]byte, 512)
			b.ReportAllocs()
			for b.Loop() {
				c, err := net.Dial("tcp", addr)
				if err != nil {
					b.Fatal(err)
				}
				_, _ = io.WriteString(c, req)
				var got []byte
				for !strings.HasSuffix(string(got), "ok") {
					k, err := c.Read(buf)
					got = append(got, buf[:k]...)
					if err != nil {
						break
					}
				}
				_ = c.(*net.TCPConn).SetLinger(0)
				_ = c.Close()
			}
		})
	}
}
