//go:build linux

package websocket

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	celeris "github.com/goceleris/celeris"
)

// TestWSOWatchFailsOnParkedEngineReader runs the real watch on a real engine
// whose read side is genuinely parked (celeris#871): the handler applies the
// engine's own pause behind the chanReader's back, so the engine reads nothing
// from a socket that holds unread bytes while the chanReader is not paused and
// holds nothing, the handler is alive in ReadMessage, and the Go scheduler
// has nothing to explain it. That is what WSO-STALL exists to catch, and the
// scheduler-latency discount must not hide it:
//
//   - real probe, 6.5 s parked: the stretch fails at the limit (the idle process
//     records no scheduler latency to discount);
//   - a probe that claims 30 s of latency, 6.5 s parked: the discount applies
//     (the test's check that the discount reaches the real engine path);
//   - the same probe, 16.3 s parked: the stretch fails while it is open, and
//     is still going when the watch stops: the cap holds on a real engine.
//
// The handler calls the engine's raw pause callback itself, without pausedMu.
// Production calls it only from requestPause, on the engine's worker under
// pausedMu; this test relies on both engines' pause being safe to call from
// another goroutine (it passes under -race on epoll and io_uring). The
// 6.5 s arm has 1.5 s of margin: it fails if more than that much scheduler
// latency lands inside its stretch, so a very loaded runner can flake it, and
// the CI job does not run this test (the fake-clock tests carry the verdict
// there).
func TestWSOWatchFailsOnParkedEngineReader(t *testing.T) {
	if testing.Short() {
		t.Skip("holds an engine parked for up to 16 s")
	}
	arms := []struct {
		name      string
		probe     func() wsoSchedProbe
		hold      time.Duration
		wantStall bool
		wantOpen  bool
	}{
		{"real probe, parked 6.5s", func() wsoSchedProbe { return newWSOSchedLat() }, 6500 * time.Millisecond, true, false},
		{"claimed 30s latency, parked 6.5s: discounted", func() wsoSchedProbe { return wsoFixedSched(30 * time.Second) }, 6500 * time.Millisecond, false, false},
		{"claimed 30s latency, parked 16.3s: the cap fails it live", func() wsoSchedProbe { return wsoFixedSched(30 * time.Second) }, 16300 * time.Millisecond, true, true},
	}
	for _, kind := range engineKinds(t) {
		for _, arm := range arms {
			t.Run(kind.String()+"/"+arm.name, func(t *testing.T) {
				t.Parallel()
				rig := newWSORig(t)
				rig.sched = arm.probe()
				parked := make(chan struct{})
				var once sync.Once
				var rawResume func()
				var handlers sync.WaitGroup
				var mu sync.Mutex
				addr, shutdown, _ := startNativeServerWithHandle(t, kind, Config{
					CheckOrigin: func(*celeris.Context) bool { return true },
					Handler: func(c *Conn) {
						handlers.Add(1)
						defer handlers.Done()
						r := c.engineReader
						// The engine's own callbacks, taken before attach wraps them:
						// calling the wrapped pause would be counted as a pause the
						// watch sees, and the chanReader's own state would not be it.
						r.pausedMu.Lock()
						rawPause, resume := r.pause, r.resume
						r.pausedMu.Unlock()
						s := rig.attach(c)
						defer s.exited("returned", nil)
						if rawPause == nil || resume == nil {
							return
						}
						mu.Lock()
						rawResume = resume
						mu.Unlock()
						rawPause() // the engine stops reading; the chanReader does not know
						once.Do(func() { close(parked) })
						for {
							if _, _, err := c.ReadMessage(); err != nil {
								return
							}
						}
					},
				})
				defer func() {
					done := make(chan struct{})
					go func() { shutdown(); handlers.Wait(); close(done) }()
					select {
					case <-done:
					case <-time.After(30 * time.Second):
						t.Error("engine shutdown did not complete within 30 s")
					}
				}()
				hostPort := strings.TrimSuffix(strings.TrimPrefix(addr, "ws://"), "/ws")
				rig.setServer(hostPort)
				stopWatch := rig.watch()
				c, err := net.DialTimeout("tcp", hostPort, 3*time.Second)
				if err != nil {
					stopWatch()
					t.Fatal(err)
				}
				cl := rig.client(c)
				defer rig.closeClient(cl, c)
				if err := cl.handshake(c, hostPort); err != nil {
					stopWatch()
					t.Fatalf("handshake: %v", err)
				}
				select {
				case <-parked:
				case <-time.After(5 * time.Second):
					stopWatch()
					t.Fatal("the handler never parked the engine")
				}
				// 20 masked 100-byte binary frames the engine will not read.
				frame := make([]byte, 6+100)
				frame[0], frame[1] = 0x82, 0x80|100
				for i := range 20 {
					_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
					if _, err := c.Write(frame); err != nil {
						stopWatch()
						t.Fatalf("frame %d: %v", i, err)
					}
				}
				time.Sleep(arm.hold)
				// The record's server-side skmem, read through the engine's own
				// descriptor of this joined connection (not a plain listener's).
				if got := rig.wsoSrvSkmem(c.LocalAddr().(*net.TCPAddr).Port); !strings.HasPrefix(got, "skmem:(r") {
					t.Errorf("the engine socket's skmem = %q, want the counters", got)
				}
				stalls := stopWatch()
				mu.Lock()
				if rawResume != nil {
					rawResume()
				}
				mu.Unlock()
				for _, st := range stalls {
					t.Log(st.String())
				}
				t.Logf("%s", rig.waitStats())
				if got := len(stalls); (got > 0) != arm.wantStall {
					t.Fatalf("stalls = %d, want a stall: %t (stallDiscounted=%d)", got, arm.wantStall, rig.stallDiscounted)
				}
				if arm.wantStall {
					st := stalls[0]
					if st.effective < wsoRecvStall {
						t.Fatalf("effective stall %v is under the limit %v", st.effective, wsoRecvStall)
					}
					if arm.wantOpen && !st.open {
						t.Fatalf("the stretch was not still going when the watch stopped")
					}
				} else if rig.stallDiscounted != 1 {
					t.Fatalf("stallDiscounted = %d, want 1: the stretch was neither failed nor counted as excused", rig.stallDiscounted)
				}
			})
		}
	}
}
