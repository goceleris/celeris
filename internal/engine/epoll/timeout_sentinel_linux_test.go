//go:build linux

package epoll

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// TestTimeoutSentinelReachesEngine pins celeris#594 on the epoll engine: the
// documented -1 "no timeout" sentinel must survive the double normalisation
// (Server.doPrepare applies WithDefaults, then New applies it again) and reach
// the loop config as disabled (0). The loop reads these fields with `> 0`
// guards (loop.go checkTimeouts and the header deadline), so 0 is "off" and a
// reinstated 10s default is an enforced timeout the caller asked to disable.
// (The epoll_wait cap and the sweep gate used to follow ReadHeaderTimeout, and
// this test read the sentinel through them; since celeris#1194 they do not, and
// the fields are read directly.)
func TestTimeoutSentinelReachesEngine(t *testing.T) {
	const explicit = 3 * time.Second

	cases := []struct {
		name                            string
		in                              time.Duration
		wantRead, wantHeader, wantWrite time.Duration
		wantIdle                        time.Duration
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
			})
		}
	}
}
