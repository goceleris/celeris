//go:build unix

package stream

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// BenchmarkConnWindowReaderRate1170 is BenchmarkConnWindowReader892 for the wait of
// celeris#1170: the same regime (stream windows open, the connection window the
// bottleneck, every waiter ends one wait per connection WINDOW_UPDATE and waits
// again), with the wait bounded as a minimum rate (rate=set: a record made by the
// first block, credited per send, looked up per wait) or not at all (rate=none: no
// WriteTimeout, no record, no timer). 892's deadline=set arm is the absolute
// deadline this replaces. Opt-in (CELERIS_893_WAITERS_BENCH=1).
func BenchmarkConnWindowReaderRate1170(b *testing.B) {
	if os.Getenv("CELERIS_893_WAITERS_BENCH") == "" {
		b.Skip("opt-in: CELERIS_893_WAITERS_BENCH=1 (celeris#892 item 28)")
	}
	for _, waiters := range []int{1, 8} {
		for _, withRate := range []bool{true, false} {
			name := "none"
			if withRate {
				name = "set"
			}
			b.Run(fmt.Sprintf("waiters=%d/rate=%s", waiters, name), func(b *testing.B) {
				p := NewProcessor(HandlerFunc(func(context.Context, *Stream) error { return nil }), newTestFrameWriter(), newTestResponseWriter())
				m := p.GetManager()
				m.connectionWindow = 0
				if withRate {
					m.SetWriteTimeout(time.Hour)
				}
				var wg sync.WaitGroup
				for i := 0; i < waiters; i++ {
					s := m.CreateStream(uint32(2*i + 1))
					s.SetState(StateOpen)
					s.flags.Or(flagAsyncRunning)
					s.SetWindowSize(1 << 30)
					wg.Add(1)
					go func() {
						defer wg.Done()
						var w SendWait
						defer w.End(s, s.Gen())
						for s.AwaitSendWindowRate(&w, 1<<30) == nil {
							// What a writer does: reserve a chunk of 64; the connection
							// grants 1, so every reservation is partial.
							if n := m.ReserveSendWindow(s, 64); n > 0 {
								m.NoteWaitSent(s.ID, int(n))
							}
						}
					}()
				}
				for until := time.Now().Add(5 * time.Second); int(m.sendWindowWaiters.Load()) < waiters; {
					if time.Now().After(until) {
						b.Fatalf("%d of %d waiters", m.sendWindowWaiters.Load(), waiters)
					}
					time.Sleep(time.Millisecond)
				}
				inc := []byte{0, 0, 0, 1}
				var r0, r1 syscall.Rusage
				_ = syscall.Getrusage(syscall.RUSAGE_SELF, &r0)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := p.HandleRawWindowUpdate(0, inc); err != nil {
						b.Fatal(err)
					}
					for atomic.LoadInt32(&m.connectionWindow) > 0 {
						runtime.Gosched()
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
