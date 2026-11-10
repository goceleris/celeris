package conn

import (
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris/internal/mono"
	h1 "github.com/goceleris/celeris/internal/protocol/h1"
)

// celeris#1178. What conn stamps and the engines' sweep compares with the clock
// of internal/mono must not move with a step of the wall clock. The clock here
// is the seam's: its wall reading steps (Stepper.Add), its monotonic reading
// runs. Each case asserts both directions: a forward step must not expire what is
// not due, a backward one must not hold what is.

// The slowloris defence: a header deadline armed at accept (ReadHeaderTimeout 10 s)
// is 10 s ahead of the clock after a step of 75 s either way.
func TestHeaderDeadlineIgnoresAStepOfTheWallClock1178(t *testing.T) {
	const budget = 10 * time.Second
	for _, tc := range []struct {
		name string
		step time.Duration
	}{{"forward", 75 * time.Second}, {"backward", -75 * time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			var st mono.Stepper
			defer mono.SetStepper(&st)()
			s := NewH1State()
			s.ReadHeaderTimeoutNs = int64(budget)
			s.ArmHeaderDeadline()
			st.Add(tc.step)
			left := time.Duration(s.HeaderDeadlineNs.Load() - mono.Now())
			if left < budget-time.Second || left > budget+time.Second {
				t.Fatalf("after a step of %v the header deadline is %v away, want about %v", tc.step, left, budget)
			}
		})
	}
}

// A WebSocket's idle deadline is given in wall Unix nanoseconds (the contract of
// Context.SetWSIdleDeadline). The engine's idle sweep compares what conn stores
// with its clock, so it keeps the time left: a step of the wall clock after the
// call does not make a live socket idle (forward) or immortal (backward).
func TestWSIdleDeadlineIgnoresAStepOfTheWallClock1178(t *testing.T) {
	const idle = 30 * time.Second
	for _, tc := range []struct {
		name string
		step time.Duration
	}{{"forward", 75 * time.Second}, {"backward", -75 * time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			var st mono.Stepper
			defer mono.SetStepper(&st)()
			state := NewH1State()
			s := populateCachedStream(state, &h1.Request{Method: "GET", Path: "/ws", Version: "HTTP/1.1", KeepAlive: true, HeadersComplete: true}, nil)
			s.OnWSSetIdleDeadline(time.Now().Add(idle).UnixNano()) // what the websocket middleware does, on the real wall
			st.Add(tc.step)
			left := time.Duration(state.IdleDeadlineNs.Load() - mono.Now())
			if left < idle-time.Second || left > idle+time.Second {
				t.Fatalf("after a step of %v the idle deadline is %v away, want about %v", tc.step, left, idle)
			}
		})
	}
}

// The sentinels pass through the conversion whatever the clock does: 0 clears the
// deadline and 1 (SSE's close after done) is expired. A pin of the conversion's
// pass-through, not a red-then-green case: 1 is expired before the flip as well.
func TestWSIdleDeadlineSentinelsPassThrough1178(t *testing.T) {
	for _, step := range []time.Duration{0, 75 * time.Second, -75 * time.Second} {
		var st mono.Stepper
		restore := mono.SetStepper(&st)
		st.Add(step)
		state := NewH1State()
		s := populateCachedStream(state, &h1.Request{Method: "GET", Path: "/e", Version: "HTTP/1.1", KeepAlive: true, HeadersComplete: true}, nil)
		s.OnWSSetIdleDeadline(1)
		if got := state.IdleDeadlineNs.Load(); got != 1 {
			t.Errorf("step %v: SetWSIdleDeadline(1) stored %d, want 1", step, got)
		}
		if state.IdleDeadlineNs.Load() > mono.Now() {
			t.Errorf("step %v: the expired sentinel is in the future", step)
		}
		s.OnWSSetIdleDeadline(0)
		if got := state.IdleDeadlineNs.Load(); got != 0 {
			t.Errorf("step %v: SetWSIdleDeadline(0) stored %d, want 0", step, got)
		}
		restore()
	}
}

// An HTTP/2 request that is not whole has ReadTimeout from its HEADERS (celeris#889).
// A step of the wall clock after it must not expire it (forward) nor keep it past its
// time (backward).
func TestH2RequestDeadlineIgnoresAStepOfTheWallClock1178(t *testing.T) {
	const rt = 700 * time.Millisecond
	for _, tc := range []struct {
		name string
		step time.Duration
	}{{"forward", 75 * time.Second}, {"backward", -75 * time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			var st mono.Stepper
			defer mono.SetStepper(&st)()
			r := newSweepRig889(t, H2Config{ReadTimeout: rt})
			r.written()
			r.frames(func(fr *http2.Framer) {
				_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: r.block("POST", "/u"), EndHeaders: true})
			})
			st.Add(tc.step)
			if tc.step > 0 {
				if res := r.st.Sweep(mono.Now(), r.write); res.Expired != 0 || res.Wrote {
					t.Fatalf("a request just armed was expired by a forward step of %v: %+v", tc.step, res)
				}
			}
			time.Sleep(rt + 300*time.Millisecond)
			if res := r.st.Sweep(mono.Now(), r.write); res.Expired != 1 {
				t.Fatalf("a request past ReadTimeout (%v of real time, step %v) was not expired: %+v", rt+300*time.Millisecond, tc.step, res)
			}
		})
	}
}
