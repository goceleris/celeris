//go:build linux

package sockopts

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// call is one setsockopt applyFD made.
type call struct{ level, opt, val int }

// recordCalls replaces the setsockopt seam with a recorder that answers each
// call with fail(opt), and puts the real one back when the test ends. It makes
// what applyFD would ask the kernel for visible without the privilege some of
// it needs: SO_PREFER_BUSY_POLL is refused without CAP_NET_ADMIN in the init
// user namespace, which no user or network namespace of a CI job can supply,
// and SO_BUSY_POLL_BUDGET cannot be read back on current kernels.
func recordCalls(t *testing.T, fail func(opt int) error) *[]call {
	t.Helper()
	var calls []call
	old := setsockoptInt
	setsockoptInt = func(_, level, opt, val int) error {
		calls = append(calls, call{level, opt, val})
		if fail != nil {
			return fail(opt)
		}
		return nil
	}
	t.Cleanup(func() { setsockoptInt = old })
	return &calls
}

func has(calls []call, level, opt int) (call, bool) {
	for _, c := range calls {
		if c.level == level && c.opt == opt {
			return c, true
		}
	}
	return call{}, false
}

const (
	soPreferBusyPoll = 69
	soBusyPollBudget = 70
)

// TestApplyFDBusyPollCalls: with busy poll off none of the busy-poll options is
// asked for; on, SO_BUSY_POLL carries the budget in microseconds, truncated,
// and SO_PREFER_BUSY_POLL is asked for. SO_BUSY_POLL_BUDGET is never asked for:
// raising it above the kernel's current value needs CAP_NET_ADMIN, and the 8
// that was set is the value the kernel already uses for a budget of 0.
func TestApplyFDBusyPollCalls(t *testing.T) {
	for _, tc := range []struct {
		name      string
		d         time.Duration
		wantPoll  int // 0: SO_BUSY_POLL not asked for
		wantPrefr bool
	}{
		{"off", 0, 0, false},
		{"sub-microsecond is off", 900 * time.Nanosecond, 0, false},
		{"37us", 37 * time.Microsecond, 37, true},
		{"1.9us truncates to 1", 1900 * time.Nanosecond, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := recordCalls(t, nil)
			if err := applyFD(3, Options{TCPQuickAck: true, RecvBuf: 4096, SOBusyPoll: tc.d}); err != nil {
				t.Fatalf("applyFD: %v", err)
			}
			c, polled := has(*calls, unix.SOL_SOCKET, unix.SO_BUSY_POLL)
			if polled != (tc.wantPoll != 0) || (polled && c.val != tc.wantPoll) {
				t.Errorf("SO_BUSY_POLL call = %+v (made: %v), want value %d (0: none)", c, polled, tc.wantPoll)
			}
			if _, ok := has(*calls, unix.SOL_SOCKET, soPreferBusyPoll); ok != tc.wantPrefr {
				t.Errorf("SO_PREFER_BUSY_POLL asked for: %v, want %v", ok, tc.wantPrefr)
			}
			if c, ok := has(*calls, unix.SOL_SOCKET, soBusyPollBudget); ok {
				t.Errorf("SO_BUSY_POLL_BUDGET asked for (%+v): a no-op at best, and an EPERM without CAP_NET_ADMIN", c)
			}
			// The other options are untouched by the budget: they are all made.
			if _, ok := has(*calls, unix.IPPROTO_TCP, unix.TCP_QUICKACK); !ok {
				t.Error("TCP_QUICKACK not set")
			}
			if _, ok := has(*calls, unix.SOL_SOCKET, unix.SO_RCVBUF); !ok {
				t.Error("SO_RCVBUF not set")
			}
		})
	}
}

// TestApplyFDPreferRefusedIsNotAnError: SO_PREFER_BUSY_POLL is best effort (an
// unprivileged process is refused it), so its refusal neither fails applyFD
// nor undoes SO_BUSY_POLL.
func TestApplyFDPreferRefusedIsNotAnError(t *testing.T) {
	calls := recordCalls(t, func(opt int) error {
		if opt == soPreferBusyPoll {
			return unix.EPERM
		}
		return nil
	})
	if err := applyFD(3, Options{SOBusyPoll: 50 * time.Microsecond}); err != nil {
		t.Fatalf("applyFD returned %v for a refused SO_PREFER_BUSY_POLL", err)
	}
	if c, ok := has(*calls, unix.SOL_SOCKET, unix.SO_BUSY_POLL); !ok || c.val != 50 {
		t.Errorf("SO_BUSY_POLL call = %+v, want 50", c)
	}
}

// TestApplyFDRefusedBusyPollIsReported: a refused SO_BUSY_POLL comes back as
// the error (the callers decide what to do with it), after the buffer sizes.
func TestApplyFDRefusedBusyPollIsReported(t *testing.T) {
	calls := recordCalls(t, func(opt int) error {
		if opt == unix.SO_BUSY_POLL {
			return unix.ENOPROTOOPT
		}
		return nil
	})
	err := applyFD(3, Options{SOBusyPoll: 50 * time.Microsecond, SendBuf: 8192})
	if !errors.Is(err, unix.ENOPROTOOPT) {
		t.Fatalf("applyFD = %v, want ENOPROTOOPT", err)
	}
	if _, ok := has(*calls, unix.SOL_SOCKET, unix.SO_SNDBUF); !ok {
		t.Error("SO_SNDBUF not set before the refused SO_BUSY_POLL")
	}
	if _, ok := has(*calls, unix.SOL_SOCKET, soPreferBusyPoll); ok {
		t.Error("SO_PREFER_BUSY_POLL asked for after SO_BUSY_POLL was refused")
	}
}

// TestApplyAcceptedWarnsOnceWhenBusyPollIsRefused: a refused SO_BUSY_POLL that
// Config.SocketBusyPoll asked for is logged at Warn, once per process however
// many connections are accepted; nothing is logged when it was not asked for,
// when a different option fails, when it works, or with no logger.
func TestApplyAcceptedWarnsOnceWhenBusyPollIsRefused(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	reset := func() { busyPollRefusedWarned.Store(false); buf.Reset() }
	t.Cleanup(func() { busyPollRefusedWarned.Store(false) })

	refuseBusyPoll := func(opt int) error {
		if opt == unix.SO_BUSY_POLL {
			return unix.ENOPROTOOPT
		}
		return nil
	}
	t.Run("refused is logged once", func(t *testing.T) {
		reset()
		recordCalls(t, refuseBusyPoll)
		for range 5 {
			ApplyAccepted(3, Options{SOBusyPoll: 50 * time.Microsecond}, logger)
		}
		out := buf.String()
		if n := strings.Count(out, "level=WARN"); n != 1 {
			t.Fatalf("%d Warn lines, want 1:\n%s", n, out)
		}
		if !strings.Contains(out, "SO_BUSY_POLL") || !strings.Contains(out, "budget_us=50") || !strings.Contains(out, "protocol not available") {
			t.Errorf("the Warn does not name the option, the budget and the kernel's answer:\n%s", out)
		}
	})
	t.Run("not asked for", func(t *testing.T) {
		reset()
		recordCalls(t, refuseBusyPoll)
		ApplyAccepted(3, Options{}, logger)
		if buf.Len() != 0 {
			t.Errorf("logged although busy poll was off:\n%s", buf.String())
		}
	})
	t.Run("another option failing", func(t *testing.T) {
		reset()
		recordCalls(t, func(opt int) error {
			if opt == unix.SO_RCVBUF {
				return unix.EINVAL
			}
			return nil
		})
		ApplyAccepted(3, Options{RecvBuf: 4096, SOBusyPoll: 50 * time.Microsecond}, logger)
		if buf.Len() != 0 {
			t.Errorf("logged for an error that is not SO_BUSY_POLL's:\n%s", buf.String())
		}
	})
	t.Run("accepted by the kernel", func(t *testing.T) {
		reset()
		recordCalls(t, nil)
		ApplyAccepted(3, Options{SOBusyPoll: 50 * time.Microsecond}, logger)
		if buf.Len() != 0 {
			t.Errorf("logged although SO_BUSY_POLL was set:\n%s", buf.String())
		}
	})
	t.Run("no logger", func(t *testing.T) {
		reset()
		recordCalls(t, refuseBusyPoll)
		ApplyAccepted(3, Options{SOBusyPoll: 50 * time.Microsecond}, nil) // must not panic
	})
}
