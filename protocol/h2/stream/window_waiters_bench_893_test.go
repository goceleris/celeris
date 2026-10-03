package stream

import (
	"context"
	"fmt"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// BenchmarkWindowUpdateWithWaiters893 measures what celeris#893's window
// waits add to a WINDOW_UPDATE: with pool handlers waiting in AwaitSendWindow,
// the event loop takes sendWindowMu and closes the channel every waiter of the
// connection selects on, so every waiter wakes, re-checks its two windows and
// waits again. 99 streams are open in every case (the per-stream work of a
// connection WINDOW_UPDATE is the same); waiters=99 has a handler waiting on
// each, its stream window closed, so none can send and each update wakes all
// of them. ns/op is the event loop's time per frame; cpu-ns/op is the
// process's user+system CPU per frame, the waiters' wakeups included.
//
// Opt-in (CELERIS_893_WAITERS_BENCH=1): its 99 goroutines make it a poor
// walltime micro-benchmark, so CodSpeed's set (./protocol/...) skips it.
func BenchmarkWindowUpdateWithWaiters893(b *testing.B) {
	if os.Getenv("CELERIS_893_WAITERS_BENCH") == "" {
		b.Skip("opt-in: CELERIS_893_WAITERS_BENCH=1 (celeris#893's window-wait cost)")
	}
	const streams = 99
	for _, frame := range []string{"conn", "stream"} {
		for _, waiters := range []int{0, streams} {
			b.Run(fmt.Sprintf("%s/waiters=%d", frame, waiters), func(b *testing.B) {
				p := NewProcessor(HandlerFunc(func(context.Context, *Stream) error { return nil }), newTestFrameWriter(), newTestResponseWriter())
				m := p.GetManager()
				ss := make([]*Stream, streams)
				for i := range ss {
					s := m.CreateStream(uint32(2*i + 1))
					s.SetState(StateOpen)
					s.flags.Or(flagAsyncRunning)
					s.SetWindowSize(0)
					ss[i] = s
				}
				// The stream the stream-level frames credit: open, nobody waits on it.
				other := m.CreateStream(uint32(2*streams + 1))
				other.SetState(StateOpen)
				var wg sync.WaitGroup
				for i := 0; i < waiters; i++ {
					wg.Add(1)
					go func(s *Stream) {
						defer wg.Done()
						for s.AwaitSendWindow() {
						}
					}(ss[i])
				}
				for until := time.Now().Add(5 * time.Second); int(m.sendWindowWaiters.Load()) < waiters; {
					if time.Now().After(until) {
						b.Fatalf("%d of %d waiters", m.sendWindowWaiters.Load(), waiters)
					}
					time.Sleep(time.Millisecond)
				}
				id, inc := uint32(0), []byte{0, 0, 0, 1}
				if frame == "stream" {
					id = other.ID
				}
				var r0, r1 syscall.Rusage
				_ = syscall.Getrusage(syscall.RUSAGE_SELF, &r0)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := p.HandleRawWindowUpdate(id, inc); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				_ = syscall.Getrusage(syscall.RUSAGE_SELF, &r1)
				cpu := (r1.Utime.Nano() + r1.Stime.Nano()) - (r0.Utime.Nano() + r0.Stime.Nano())
				b.ReportMetric(float64(cpu)/float64(b.N), "cpu-ns/op")
				m.Close()
				wg.Wait()
			})
		}
	}
}
