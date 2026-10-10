//go:build linux

package adaptive

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/iouring"
	"github.com/goceleris/celeris/internal/resource"
)

// Tests for celeris#729. The README's CELERIS_MAX_IOURING_TIER row said that
// at "none" Adaptive "neither starts on io_uring nor switches to it". That is
// true of Adaptive's automatic choice only: chooseStartEngine returns the
// CELERIS_ADAPTIVE_START=iouring override before it asks ioUringViable, so New
// still builds io_uring first, iouring.New refuses the capped tier, and New
// logs one WARN and starts on epoll. The row now says so. These tests pin each
// half of the new wording against the code, through the real probe. The cap
// makes them independent of the host's kernel.

const warn729 = "io_uring start engine unavailable, falling back to epoll start"

func newCappedNone729(t *testing.T, startEnv string) (e *Engine, logs string) {
	t.Helper()
	withMemlock(t, -1)
	t.Setenv("CELERIS_MAX_IOURING_TIER", "none")
	t.Setenv("CELERIS_ADAPTIVE_START", startEnv)
	var buf bytes.Buffer
	cfg := resource.Config{
		Addr:     "127.0.0.1:0",
		Protocol: engine.HTTP1,
		Logger:   slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	e, err := New(cfg, noopHandler{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Release what New built the way a server does (see
	// TestNew_TierCapNoneNeverPromotes).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = e.Listen(ctx)
	return e, buf.String()
}

// TestNew_TierCapNoneExplicitStartIOUringFallsBackToEpoll729 is the new half
// of the README row: with the override set, New tries io_uring (so the WARN
// is logged once, carrying iouring.New's error), starts on epoll, and leaves
// the conns-per-worker up-switch off.
func TestNew_TierCapNoneExplicitStartIOUringFallsBackToEpoll729(t *testing.T) {
	e, logs := newCappedNone729(t, "iouring")
	if n := strings.Count(logs, warn729); n != 1 {
		t.Errorf("fallback WARN %q logged %d times, want 1; log:\n%s", warn729, n, logs)
	}
	if !strings.Contains(logs, "io_uring not available on this system") {
		t.Errorf("fallback WARN does not carry iouring.New's error; log:\n%s", logs)
	}
	if e.startType != engine.Epoll {
		t.Errorf("start engine = %v, want epoll: iouring.New refuses the capped tier", e.startType)
	}
	if e.ctrl.connSwitchEnabled {
		t.Error("the conns-per-worker UP switch is enabled with io_uring capped to none")
	}
}

// TestNew_TierCapNoneOverrideEpollAndAutoNeverTryIOUring729 is the control and
// the half the row keeps: with the override at epoll, or at auto (set,
// unrecognised or empty), Adaptive does not try io_uring, so nothing is logged.
func TestNew_TierCapNoneOverrideEpollAndAutoNeverTryIOUring729(t *testing.T) {
	for _, env := range []string{"epoll", "auto", "", "bogus"} {
		t.Run("ADAPTIVE_START="+env, func(t *testing.T) {
			e, logs := newCappedNone729(t, env)
			if logs != "" {
				t.Errorf("New logged at WARN or above:\n%s", logs)
			}
			if e.startType != engine.Epoll {
				t.Errorf("start engine = %v, want epoll", e.startType)
			}
			if e.ctrl.connSwitchEnabled {
				t.Error("the conns-per-worker UP switch is enabled with io_uring capped to none")
			}
		})
	}
}

// TestIOUringEngineTierCapNoneUnavailable729 pins the other half of the row
// the same way: the io_uring engine itself reports io_uring as unavailable at
// none (the error the adaptive fallback above carries).
func TestIOUringEngineTierCapNoneUnavailable729(t *testing.T) {
	t.Setenv("CELERIS_MAX_IOURING_TIER", "none")
	eng, err := iouring.New(resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1}, noopHandler{})
	if err == nil {
		_ = eng.Shutdown(context.Background())
		t.Fatal("iouring.New succeeded with CELERIS_MAX_IOURING_TIER=none")
	}
	if !strings.HasPrefix(err.Error(), "io_uring not available on this system") {
		t.Errorf("iouring.New error = %q, want the prefix %q", err, "io_uring not available on this system")
	}
}
