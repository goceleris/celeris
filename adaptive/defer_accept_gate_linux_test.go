//go:build linux

package adaptive

import (
	"log/slog"
	"testing"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/probe"
	"github.com/goceleris/celeris/resource"
)

// celeris#662 / celeris#675, the gate.
//
// Clearing TCP_DEFER_ACCEPT is a MEASURED cost on connection churn (+2.46 to
// +3.51 us of server CPU per connection, +14-21% ns/op), and adaptive is the
// DEFAULT engine on Linux. So the fix may only be applied where a pause can
// actually lose a connection -- that is, where a switch is reachable. These
// tests pin the predicate and the wiring: if the gate is widened back to
// "always", the m8/h2c cases below fail; if it is narrowed so a switchable
// engine keeps the option, the switchable case fails and celeris#662 returns.

// TestSwitchPossibleGate pins the predicate itself, independent of any engine.
func TestSwitchPossibleGate(t *testing.T) {
	cfg := func(proto engine.Protocol) resource.Config {
		return resource.Config{Protocol: proto}
	}
	tests := []struct {
		name    string
		profile engine.CapabilityProfile
		cfg     resource.Config
		start   engine.EngineType
		memlock int // -1 = uncapped
		wantUp  bool
		wantAny bool
	}{
		{"epoll start, viable, h1: can promote, so can pause",
			viableProfile(), cfg(engine.HTTP1), engine.Epoll, -1, true, true},
		{"epoll start, viable, auto: can promote, so can pause",
			viableProfile(), cfg(engine.Auto), engine.Epoll, -1, true, true},
		{"epoll start, h2c: never promotes, never pauses",
			viableProfile(), cfg(engine.H2C), engine.Epoll, -1, false, false},
		{"epoll start, old kernel: io_uring unviable, never pauses",
			oldProfile(), cfg(engine.HTTP1), engine.Epoll, -1, false, false},
		{"epoll start, memlock below one worker: never pauses",
			viableProfile(), cfg(engine.HTTP1), engine.Epoll, 1, false, false},
		{"io_uring start: the always-on error revert can still pause it",
			viableProfile(), cfg(engine.HTTP1), engine.IOUring, -1, false, true},
		{"io_uring start, h2c: the error revert does not care about protocol",
			viableProfile(), cfg(engine.H2C), engine.IOUring, -1, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withMemlock(t, tc.memlock)
			if got := upSwitchPossible(tc.profile, tc.cfg, tc.start); got != tc.wantUp {
				t.Errorf("upSwitchPossible = %v, want %v", got, tc.wantUp)
			}
			if got := switchPossible(tc.profile, tc.cfg, tc.start); got != tc.wantAny {
				t.Errorf("switchPossible = %v, want %v. This is the predicate the "+
					"celeris#662 fix is gated on: true must mean a sub-engine can be "+
					"paused, false must mean none ever is", got, tc.wantAny)
			}
		})
	}
}

// TestDeferAcceptFollowsSwitchPossibility pins the WIRING: New() must put
// DisableDeferAccept on the sub-engine configs exactly when switchPossible
// says a pause is reachable, and leave the option alone otherwise.
//
// It reads e.cfg, which New stores as the standby's config -- the same value
// both sub-engines are built from.
func TestDeferAcceptFollowsSwitchPossibility(t *testing.T) {
	newFor := func(t *testing.T, proto engine.Protocol) *Engine {
		t.Helper()
		e, err := New(resource.Config{
			Addr:     "127.0.0.1:0",
			Protocol: proto,
			Logger:   slog.New(slog.DiscardHandler),
		}, respHandler{}, nil)
		if err != nil {
			t.Skipf("adaptive.New unsupported here: %v", err)
		}
		return e
	}

	// h2c can never promote, on any machine, so this arm is environment-free.
	t.Run("h2c keeps the option", func(t *testing.T) {
		e := newFor(t, engine.H2C)
		if e.ctrl.connSwitchEnabled {
			t.Fatal("h2c must never enable the up-switch")
		}
		if e.cfg.DisableDeferAccept {
			t.Error("an h2c adaptive engine can never switch, so it never pauses a " +
				"sub-engine and must KEEP TCP_DEFER_ACCEPT. Setting it here puts the " +
				"measured churn cost (+2.46-3.51 us CPU per connection) on a " +
				"configuration that cannot hit celeris#662")
		}
	})

	// The switchable arm only exists where io_uring really is viable.
	t.Run("a switchable engine drops the option", func(t *testing.T) {
		e := newFor(t, engine.HTTP1)
		if !e.ctrl.connSwitchEnabled {
			t.Skipf("the up-switch is disabled in this environment (memlock worker "+
				"ceiling %d, -1 = unlimited); the gate's true-arm cannot be observed here",
				maxWorkersForMemlock())
		}
		if !e.cfg.DisableDeferAccept {
			t.Error("this engine CAN switch, and every switch pauses the outgoing " +
				"sub-engine, so it must NOT defer accept: a handshake-complete " +
				"connection that has sent nothing is invisible to the pause and is " +
				"reset (celeris#662)")
		}
	})

	// And the two must agree with the predicate, whatever this machine is.
	t.Run("wiring matches the predicate", func(t *testing.T) {
		e := newFor(t, engine.HTTP1)
		want := switchPossible(probe.Probe(), resource.Config{
			Addr: "127.0.0.1:0", Protocol: engine.HTTP1,
		}, e.startType)
		if e.cfg.DisableDeferAccept != want {
			t.Errorf("DisableDeferAccept = %v but switchPossible = %v: the gate and the "+
				"flag have drifted apart", e.cfg.DisableDeferAccept, want)
		}
	})
}
