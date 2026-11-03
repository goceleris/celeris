//go:build linux

package memcached

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// recordSetsockopt replaces the setsockopt seam with a recorder that
// accepts every call, and puts the real one back when the test ends. These
// tests do not run in parallel: the seam is a package variable.
func recordSetsockopt(t *testing.T) *[][3]int {
	t.Helper()
	var calls [][3]int
	old := setsockoptInt
	setsockoptInt = func(_, level, opt, val int) error {
		calls = append(calls, [3]int{level, opt, val})
		return nil
	}
	t.Cleanup(func() { setsockoptInt = old })
	return &calls
}

// busyPollCalls is the SO_BUSY_POLL values among calls, and whether
// TCP_QUICKACK was set.
func busyPollCalls(calls [][3]int) (polls []int, quickAck bool) {
	for _, c := range calls {
		switch {
		case c[0] == unix.SOL_SOCKET && c[1] == unix.SO_BUSY_POLL:
			polls = append(polls, c[2])
		case c[0] == unix.IPPROTO_TCP && c[1] == unix.TCP_QUICKACK:
			quickAck = true
		}
	}
	return polls, quickAck
}

// celeris#1228: 0 is off and means no setsockopt call at all, so a socket keeps
// what the kernel gave it (net.core.busy_read); a positive budget is set in
// whole microseconds. TCP_QUICKACK is not part of the decision.
func TestTuneConnSocketBusyPollCalls(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    time.Duration
		want []int // SO_BUSY_POLL values asked for; none: off
	}{
		{"off", 0, nil},
		{"negative is off", -time.Microsecond, nil},
		{"sub-microsecond is off", 900 * time.Nanosecond, nil},
		{"37us", 37 * time.Microsecond, []int{37}},
		{"1.9us truncates to 1", 1900 * time.Nanosecond, []int{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := recordSetsockopt(t)
			tuneConnSocket(3, tc.d)
			polls, quickAck := busyPollCalls(*calls)
			if len(polls) != len(tc.want) || (len(polls) == 1 && polls[0] != tc.want[0]) {
				t.Errorf("SO_BUSY_POLL calls = %v, want %v", polls, tc.want)
			}
			if !quickAck {
				t.Error("TCP_QUICKACK not set")
			}
		})
	}
}
