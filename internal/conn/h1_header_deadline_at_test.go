package conn

import (
	"testing"
	"time"
)

// TestArmHeaderDeadlineAt pins ArmHeaderDeadlineAt: the deadline is the earlier
// of now + ReadHeaderTimeoutNs and the limit the engine passes, the
// callback fires on arming, and a second call neither moves the deadline nor
// fires the callback again.
func TestArmHeaderDeadlineAt(t *testing.T) {
	const budget = int64(10 * time.Second)
	now := time.Now().UnixNano()
	cases := []struct {
		name     string
		timeout  int64
		notAfter int64
		want     func(before, after, dl int64) bool
		armed    bool
	}{
		{"no-limit-is-ArmHeaderDeadline", budget, 0, func(b, a, dl int64) bool { return dl >= b+budget && dl <= a+budget }, true},
		{"limit-before-the-budget", budget, now + int64(time.Second), func(b, a, dl int64) bool { return dl == now+int64(time.Second) }, true},
		{"limit-after-the-budget", budget, now + 2*budget, func(b, a, dl int64) bool { return dl >= b+budget && dl <= a+budget }, true},
		{"limit-already-passed", budget, now - int64(time.Second), func(b, a, dl int64) bool { return dl == now-int64(time.Second) }, true},
		{"disabled-ignores-the-limit", 0, now + int64(time.Second), func(b, a, dl int64) bool { return dl == 0 }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewH1State()
			s.ReadHeaderTimeoutNs = tc.timeout
			calls := 0
			s.OnHeaderDeadlineArmed = func() { calls++ }
			before := time.Now().UnixNano()
			s.ArmHeaderDeadlineAt(tc.notAfter)
			after := time.Now().UnixNano()
			dl := s.HeaderDeadlineNs.Load()
			if !tc.want(before, after, dl) {
				t.Fatalf("deadline %d, notAfter %d, now %d..%d, budget %d", dl, tc.notAfter, before, after, tc.timeout)
			}
			if tc.armed != (calls == 1) || (!tc.armed && calls != 0) {
				t.Fatalf("OnHeaderDeadlineArmed ran %d times, armed=%v", calls, tc.armed)
			}
			s.ArmHeaderDeadlineAt(0)
			s.ArmHeaderDeadlineAt(tc.notAfter + 1)
			if s.HeaderDeadlineNs.Load() != dl {
				t.Fatalf("a second arm moved the deadline from %d to %d", dl, s.HeaderDeadlineNs.Load())
			}
			if tc.armed && calls != 1 {
				t.Fatalf("OnHeaderDeadlineArmed ran %d times after the repeat arms, want 1", calls)
			}
		})
	}
}
