//go:build linux

package adaptive

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// TestTimeoutSentinelReachesEngine pins celeris#594 on the adaptive engine,
// which is the worst case: Server.doPrepare normalises, adaptive.New
// normalises again, and the sub-engine constructor normalises a third time.
// The -1 "no timeout" sentinel must still be disabled (0) in the config
// adaptive hands to whichever sub-engine it builds.
func TestTimeoutSentinelReachesEngine(t *testing.T) {
	const explicit = 3 * time.Second

	cases := []struct {
		name                                      string
		in                                        time.Duration
		wantRead, wantHeader, wantWrite, wantIdle time.Duration
	}{
		{"disabled", -1, 0, 0, 0, 0},
		{"unset", 0, 60 * time.Second, 10 * time.Second, 60 * time.Second, 600 * time.Second},
		{"explicit", explicit, explicit, explicit, explicit, explicit},
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
					Engine:            engine.Adaptive,
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
				e, err := New(cfg, stream.HandlerFunc(func(context.Context, *stream.Stream) error { return nil }), nil)
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				defer func() { _ = e.Shutdown(context.Background()) }()

				// e.cfg is what buildStandby and the eager sub-engine
				// constructor are both handed.
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
				// The sub-engine normalises e.cfg once more; that pass
				// must be a no-op for the sentinel too.
				again := e.cfg.WithDefaults()
				if again.ReadHeaderTimeout != tc.wantHeader || again.ReadTimeout != tc.wantRead ||
					again.WriteTimeout != tc.wantWrite || again.IdleTimeout != tc.wantIdle {
					t.Errorf("sub-engine re-normalisation changed the timeouts: %v/%v/%v/%v, want %v/%v/%v/%v",
						again.ReadTimeout, again.ReadHeaderTimeout, again.WriteTimeout, again.IdleTimeout,
						tc.wantRead, tc.wantHeader, tc.wantWrite, tc.wantIdle)
				}
			})
		}
	}
}
