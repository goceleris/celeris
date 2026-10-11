//go:build linux

package iouring

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// sizedHandler811 answers every request with a body of fixed size, inline or
// on the dispatch goroutine.
type sizedHandler811 struct {
	body  []byte
	async bool
}

func (h *sizedHandler811) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "application/octet-stream"}, {"content-length", strconv.Itoa(len(h.body))}}, h.body)
}
func (h *sizedHandler811) RouteAsync(_, _ string) bool { return h.async }
func (h *sizedHandler811) HasAsyncRoutes() bool        { return h.async }

// BenchmarkResponseSize811 is the path celeris#811's change sits next to,
// with no stalled client: one keep-alive client that reads every response, a
// request at a time. The large sizes put the response's tail on a ring SEND
// while the client reads, so the conn is on the dirty list with a SEND in
// flight, which the worker used to spin through and now waits through; the
// small size never reaches the ring. ns/op is the request round trip; MB/s the
// goodput. The A/B script (evidence/lanes-20261009/E14/ab.sh) runs it against
// the base and the fix with an A/A floor.
func BenchmarkResponseSize811(b *testing.B) {
	for _, async := range []bool{false, true} {
		mode := "sync"
		if async {
			mode = "async"
		}
		for _, size := range []int{1 << 10, 256 << 10, 4 << 20} {
			b.Run(mode+"/"+strconv.Itoa(size>>10)+"KiB", func(b *testing.B) {
				body := make([]byte, size)
				for i := range body {
					body[i] = byte(i)
				}
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					b.Fatalf("pick port: %v", err)
				}
				addr := ln.Addr().String()
				_ = ln.Close()
				e, err := New(resource.Config{
					Addr:     addr,
					Protocol: engine.HTTP1,
					// A 64 KiB server send buffer, as the #811 tests set: the
					// autotuned default takes up to a few MiB, and on some hosts
					// the whole response, in which case the direct write sends
					// it and no ring SEND, hence no dirty-list state, ever
					// happens.
					Resources:     resource.Resources{Workers: 2, SocketSend: 64 << 10},
					AsyncHandlers: async,
					Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
				}, &sizedHandler811{body: body, async: async})
				if err != nil {
					b.Skipf("iouring engine unavailable: %v", err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- e.Listen(ctx) }()
				defer func() {
					cancel()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						b.Error("engine did not stop within 5s")
					}
				}()
				var c net.Conn
				for deadline := time.Now().Add(8 * time.Second); ; {
					if c, err = net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil && e.NumWorkers() > 0 {
						break
					}
					if c != nil {
						_ = c.Close()
					}
					if time.Now().After(deadline) {
						b.Fatal("engine did not start listening within 8s")
					}
					time.Sleep(20 * time.Millisecond)
				}
				defer func() { _ = c.Close() }()
				req := []byte("GET /a HTTP/1.1\r\nHost: x\r\n\r\n")
				br := bufio.NewReaderSize(c, 256<<10)
				if _, err := c.Write(req); err != nil {
					b.Fatal(err)
				}
				// rawResponseLen reads one whole response, the body included.
				if _, err := rawResponseLen(br); err != nil {
					b.Fatalf("first response: %v", err)
				}
				read := func() {
					if _, err := c.Write(req); err != nil {
						b.Fatal(err)
					}
					if _, err := rawResponseLen(br); err != nil {
						b.Fatal(err)
					}
				}
				for range 20 {
					read()
				}
				b.SetBytes(int64(size))
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					read()
				}
				// The witness that the changed path ran: the share of the
				// response bytes that went through a ring SEND (a conn with a
				// SEND in flight is what the dirty pass gives up). Published by
				// the worker once per loop iteration: give it one.
				b.StopTimer()
				time.Sleep(200 * time.Millisecond)
				m := e.Metrics()
				if tot := m.RingBytes + m.InlineBytes; tot > 0 {
					b.ReportMetric(float64(m.RingBytes)/float64(tot), "ring_share")
				}
			})
		}
	}
}
