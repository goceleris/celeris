//go:build linux

package epoll

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// TestTimeoutSentinelReachesEngine pins celeris#594 on the epoll engine: the
// documented -1 "no timeout" sentinel must survive the double normalisation
// (Server.doPrepare applies WithDefaults, then New applies it again) and reach
// the loop config as disabled (0). The loop reads these fields with `> 0`
// guards (loop.go checkTimeouts / the epoll_wait cap), so 0 is "off" and a
// reinstated 10s default is an enforced timeout the caller asked to disable.
func TestTimeoutSentinelReachesEngine(t *testing.T) {
	const explicit = 3 * time.Second

	cases := []struct {
		name                               string
		in                                 time.Duration
		wantRead, wantHeader, wantWrite    time.Duration
		wantIdle                           time.Duration
		wantHeaderDeadlineSweepGateEnabled bool
	}{
		{"disabled", -1, 0, 0, 0, 0, false},
		{"unset", 0, 60 * time.Second, 10 * time.Second, 60 * time.Second, 600 * time.Second, true},
		{"explicit", explicit, explicit, explicit, explicit, explicit, true},
	}

	for _, tc := range cases {
		for _, preNormalised := range []bool{false, true} {
			label := tc.name + "/raw"
			if preNormalised {
				label = tc.name + "/via-doPrepare"
			}
			t.Run(label, func(t *testing.T) {
				cfg := resource.Config{
					Addr:              "127.0.0.1:0",
					Engine:            engine.Epoll,
					ReadTimeout:       tc.in,
					ReadHeaderTimeout: tc.in,
					WriteTimeout:      tc.in,
					IdleTimeout:       tc.in,
				}
				if preNormalised {
					cfg = cfg.WithDefaults() // what Server.doPrepare does
				}
				e, err := New(cfg, stream.HandlerFunc(func(context.Context, *stream.Stream) error { return nil }))
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				// e.cfg is copied verbatim into every loop (loop.cfg).
				if e.cfg.ReadTimeout != tc.wantRead {
					t.Errorf("cfg.ReadTimeout = %v, want %v", e.cfg.ReadTimeout, tc.wantRead)
				}
				if e.cfg.ReadHeaderTimeout != tc.wantHeader {
					t.Errorf("cfg.ReadHeaderTimeout = %v, want %v", e.cfg.ReadHeaderTimeout, tc.wantHeader)
				}
				if e.cfg.WriteTimeout != tc.wantWrite {
					t.Errorf("cfg.WriteTimeout = %v, want %v", e.cfg.WriteTimeout, tc.wantWrite)
				}
				if e.cfg.IdleTimeout != tc.wantIdle {
					t.Errorf("cfg.IdleTimeout = %v, want %v", e.cfg.IdleTimeout, tc.wantIdle)
				}
				// Consequence (celeris#584 on the epoll side): the
				// epoll_wait cap and the checkTimeouts sweep gate are both
				// `cfg.ReadHeaderTimeout > 0`. With the sentinel honoured
				// the cap is driven by detachedCount alone; with the
				// default reinstated it is pinned at 25ms forever.
				l := &Loop{cfg: e.cfg, consecutiveEmpty: 200}
				idle := l.adaptiveTimeoutMs(100)
				l.detachedCount = 1
				detached := l.adaptiveTimeoutMs(100)
				if tc.wantHeaderDeadlineSweepGateEnabled {
					if idle != 25 || detached != 25 {
						t.Errorf("adaptiveTimeoutMs with ReadHeaderTimeout=%v: idle=%dms detached=%dms, want 25/25",
							e.cfg.ReadHeaderTimeout, idle, detached)
					}
				} else {
					if idle != 400 || detached != 50 {
						t.Errorf("adaptiveTimeoutMs with ReadHeaderTimeout disabled: idle=%dms detached=%dms, want 400/50",
							idle, detached)
					}
				}
			})
		}
	}
}
