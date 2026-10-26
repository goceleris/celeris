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

// BenchmarkConnWindowReader892 is the regime of a client that reads but is over
// the connection's outbound budget (celeris#892 item 28): its stream windows
// are open and the connection window is the bottleneck, so each handler waiting
// in AwaitSendWindow ends once per connection WINDOW_UPDATE that gives it room,
// takes the room and waits again. BenchmarkWindowUpdateWithWaiters893 keeps
// every waiter's stream window at 0, so no waiter returns and re-waits, and the
// per-wait timer (time.AfterFunc, armed and stopped by every wait) is never
// measured.
//
// deadline=set gives each wait the deadline the engines give it (the server's
// WriteTimeout); deadline=none gives none, so no timer is armed: the same
// binary, the two arms differ in the timer alone. ns/op is the time per frame
// as the event loop sees it, handler wake-up and handoff included; cpu-ns/op is
// the process's user+system CPU per frame.
//
// take=cas takes the connection credit with a CAS of the window; take=reserve
// takes it as a writer does, with ReserveSendWindow, whose partial reservations
// refund the stream window (and notify, when the refund reopens it).
//
// Opt-in (CELERIS_893_WAITERS_BENCH=1), like BenchmarkWindowUpdateWithWaiters893.
func BenchmarkConnWindowReader892(b *testing.B) {
	if os.Getenv("CELERIS_893_WAITERS_BENCH") == "" {
		b.Skip("opt-in: CELERIS_893_WAITERS_BENCH=1 (celeris#892 item 28)")
	}
	for _, waiters := range []int{1, 8} {
		for _, withDeadline := range []bool{true, false} {
			for _, take := range []string{"cas", "reserve"} {
				name := "none"
				if withDeadline {
					name = "set"
				}
				b.Run(fmt.Sprintf("waiters=%d/deadline=%s/take=%s", waiters, name, take), func(b *testing.B) {
					p := NewProcessor(HandlerFunc(func(context.Context, *Stream) error { return nil }), newTestFrameWriter(), newTestResponseWriter())
					m := p.GetManager()
					m.connectionWindow = 0
					var dl time.Time
					if withDeadline {
						dl = time.Now().Add(time.Hour)
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
							for s.AwaitSendWindow(dl) == nil {
								if take == "reserve" {
									// What a writer does: reserve a chunk of 64 from both
									// windows; the connection grants 1, so every reservation
									// is partial and refunds 63 to the stream window.
									m.ReserveSendWindow(s, 64)
									continue
								}
								for { // take one unit of the connection window, if any is left
									w := atomic.LoadInt32(&m.connectionWindow)
									if w <= 0 || atomic.CompareAndSwapInt32(&m.connectionWindow, w, w-1) {
										break
									}
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
						// The frame's credit is taken by a waiter before the next
						// frame: every frame ends one wait.
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
}
