package conn

import (
	"testing"
	"time"
)

// celeris#1430: NextDeadline is what the io_uring worker sets its next look at an
// HTTP/2 connection from. It is the earliest of the four hints, and the end of
// the idle period when that is still ahead; a hint that is due is returned as it
// is (the worker looks again one floor on); the 100 ms look at the idle clock is
// not a deadline and is not in it.
func TestNextDeadline1430(t *testing.T) {
	const now = int64(1_000_000_000_000)
	ms := int64(time.Millisecond)
	mk := func(req, tomb, hdr, wr, d0At, idle int64) *H2State {
		s := NewH2State(nil, H2Config{}, func([]byte) {}, nil)
		s.hints.ReqDL.Store(req)
		s.hints.TombDL.Store(tomb)
		s.hints.HdrDL.Store(hdr)
		s.hints.WrDL.Store(wr)
		s.d0At, s.idleNs = d0At, idle
		return s
	}
	for _, tc := range []struct {
		name string
		s    *H2State
		want int64
	}{
		{"nothing armed", mk(0, 0, 0, 0, 0, 0), 0},
		{"the request deadline", mk(now+300*ms, 0, 0, 0, 0, 0), now + 300*ms},
		{"the earliest of the four", mk(now+300*ms, now+900*ms, now+200*ms, now+500*ms, 0, 0), now + 200*ms},
		{"a hint that is due is returned as it is", mk(now-5*ms, now+900*ms, 0, 0, 0, 0), now - 5*ms},
		{"the idle period ahead", mk(0, 0, 0, 0, now-100*ms, 400*ms), now + 300*ms + 1},
		{"the idle period ahead of a later hint", mk(now+time.Hour.Nanoseconds(), 0, 0, 0, now, 400*ms), now + 400*ms + 1},
		{"the idle period already over is not a deadline", mk(0, 0, 0, 0, now-time.Second.Nanoseconds(), 400*ms), 0},
		{"the idle clock not started", mk(0, 0, 0, 0, 0, 400*ms), 0},
		{"the idle clock off", mk(0, 0, 0, 0, now, 0), 0},
	} {
		if got := tc.s.NextDeadline(now); got != tc.want {
			t.Errorf("celeris1430 %s: NextDeadline = %d (%+d ms from now), want %d (%+d ms)",
				tc.name, got, (got-now)/ms, tc.want, (tc.want-now)/ms)
		}
	}
}
