//go:build linux

package iouring

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// TestTimeoutSentinelReachesEngine pins celeris#594 on the io_uring engine:
// the documented -1 "no timeout" sentinel must survive the double
// normalisation (Server.doPrepare applies WithDefaults, then New applies it
// again) and reach the worker config as disabled (0). The trace on #594 showed
// w.cfg.ReadHeaderTimeout=10s on every sweep line with -1 configured.
func TestTimeoutSentinelReachesEngine(t *testing.T) {
	const explicit = 3 * time.Second

	cases := []struct {
		name                                      string
		in                                        time.Duration
		wantRead, wantHeader, wantWrite, wantIdle time.Duration
		wantHeaderGate                            bool
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
					Engine:            engine.IOUring,
					Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
					Resources:         resource.Resources{Workers: 2},
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
					// Only a genuine kernel-capability miss is a skip; any
					// other constructor error (e.g. a config the test built
					// wrong) must fail, or the test silently measures
					// nothing.
					if strings.Contains(err.Error(), "io_uring not available") {
						t.Skipf("iouring engine unavailable: %v", err)
					}
					t.Fatalf("New: %v", err)
				}
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

				// Consequence, celeris#584/#549: the sweep cadence gate
				// (0x3FF vs 0x1F) and the SubmitAndWait cap are
				// `w.detachedCount > 0 || w.cfg.ReadHeaderTimeout > 0`.
				// e.cfg is what Listen copies into every worker, so a
				// worker carrying it reproduces the gate exactly.
				// listenFD >= 0 = not draining; emptyIters > 100 = idle,
				// which is the branch that returns the cap.
				w := &Worker{cfg: e.cfg, listenFD: 3, emptyIters: 200}
				idle := w.adaptiveTimeout()
				gateForcedByHeader := w.cfg.ReadHeaderTimeout > 0
				w.detachedCount = 1
				detached := w.adaptiveTimeout()

				if gateForcedByHeader != tc.wantHeaderGate {
					t.Errorf("sweep gate forced by ReadHeaderTimeout = %v, want %v (cfg=%v)",
						gateForcedByHeader, tc.wantHeaderGate, w.cfg.ReadHeaderTimeout)
				}
				if tc.wantHeaderGate {
					if idle != 25*time.Millisecond || detached != 25*time.Millisecond {
						t.Errorf("adaptiveTimeout with ReadHeaderTimeout=%v: idle=%v detached=%v, want 25ms/25ms",
							e.cfg.ReadHeaderTimeout, idle, detached)
					}
				} else {
					// With the sentinel honoured the cadence is gated by
					// detachedCount as the code intends: 100ms idle,
					// tightening to 50ms while detached conns exist.
					if idle != 100*time.Millisecond || detached != 50*time.Millisecond {
						t.Errorf("adaptiveTimeout with ReadHeaderTimeout disabled: idle=%v detached=%v, want 100ms/50ms",
							idle, detached)
					}
				}
			})
		}
	}
}
