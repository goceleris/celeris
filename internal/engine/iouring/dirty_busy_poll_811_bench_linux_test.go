//go:build linux

package iouring

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync/atomic"
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
// request at a time. The large async sizes put the response's tail on a ring
// SEND while the client reads, so the conn is on the dirty list with a SEND in
// flight, which the worker used to spin through and now waits through; the
// small async size never reaches the ring. A sync conn is listed only when the
// SQ ring is full, so the sync arms are a second A/A (see ring_frac below).
// ns/op is the request round trip; MB/s the goodput. The A/B method is an
// interleaved base/fix/base2 A/B, with base2 an A/A copy of the base, and the
// exact counters iters_per_op, cqes_per_op and giveups_per_op.
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
				stopped := false
				stop := func() {
					if stopped {
						return
					}
					stopped = true
					cancel()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						b.Error("engine did not stop within 5s")
					}
				}
				defer stop()
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
				// ring_frac: the ring-sent bytes over the response bytes
				// requested (headers included, so a small body reads above 1).
				// It shows that ring SENDs happened, which the dirty pass's
				// give-up needs; it does not show that the give-up ran (a sync
				// conn is ring-sent entirely and is listed only on a full SQ
				// ring, and an async handler's direct write is counted
				// nowhere). Published by the worker once per loop iteration:
				// give it one.
				b.StopTimer()
				time.Sleep(200 * time.Millisecond)
				// cqes_per_op: the CQEs the kernel posted to the workers' CQ
				// rings (the cumulative tail, read while the rings are still
				// mapped, i.e. before the engine stops; the worker is idle
				// after the sleep) per request: a count, not a time.
				var cqes uint64
				for _, w := range e.workers {
					if w.ring != nil {
						cqes += uint64(atomic.LoadUint32((*uint32)(w.ring.cqTail)))
					}
				}
				b.ReportMetric(float64(cqes)/float64(b.N+21), "cqes_per_op")
				b.ReportMetric(float64(e.Metrics().RingBytes)/(float64(b.N+21)*float64(size)), "ring_frac")
				// iters_per_op: the run loops' iterations (iterCount, summed
				// over the workers, read once Listen has returned and the
				// workers are gone) per request. A count, not a time: a worker
				// that polls turns the loop many times per request, one that
				// parks does not. The idle iterations of the setup and of the
				// 200 ms above are in it (a few hundred, at most, over b.N
				// requests).
				stop()
				var iters, giveUps uint64
				for _, w := range e.workers {
					iters += w.iterCount
					giveUps += w.dirtySendGiveUps
				}
				b.ReportMetric(float64(iters)/float64(b.N+21), "iters_per_op")
				// giveups_per_op: the dirty pass's give-ups of a conn with a
				// SEND outstanding, the path the fix adds. 0 means the arm did
				// not exercise it. The base binary of the A/B declares the
				// counter and never increments it, so it reports 0 here.
				b.ReportMetric(float64(giveUps)/float64(b.N+21), "giveups_per_op")
			})
		}
	}
}
