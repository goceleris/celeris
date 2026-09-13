package std

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// TestTimeoutSentinelReachesEngine pins celeris#594 on the std engine: the
// documented -1 "no timeout" sentinel must survive the double normalisation
// (Server.doPrepare applies WithDefaults, then New applies it again) and reach
// both the engine config and the wrapped http.Server as disabled (0).
//
// The `normalised` column is what doPrepare hands the constructor; the raw
// column is a caller building the engine directly. Both must land on the same
// value.
func TestTimeoutSentinelReachesEngine(t *testing.T) {
	const explicit = 3 * time.Second

	type want struct {
		read, readHeader, write, idle time.Duration
	}
	cases := []struct {
		name string
		in   time.Duration // applied to all four timeout fields
		want want
	}{
		{
			name: "disabled",
			in:   -1,
			want: want{0, 0, 0, 0},
		},
		{
			name: "unset",
			in:   0,
			want: want{60 * time.Second, 10 * time.Second, 60 * time.Second, 600 * time.Second},
		},
		{
			name: "explicit",
			in:   explicit,
			want: want{explicit, explicit, explicit, explicit},
		},
	}

	for _, tc := range cases {
		for _, preNormalised := range []bool{false, true} {
			label := tc.name
			if preNormalised {
				label += "/via-doPrepare"
			} else {
				label += "/raw"
			}
			t.Run(label, func(t *testing.T) {
				cfg := resource.Config{
					Addr:              "127.0.0.1:0",
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
				got := []struct {
					name       string
					cfg, srv   time.Duration
					wantedFrom time.Duration
				}{
					{"ReadTimeout", e.cfg.ReadTimeout, e.server.ReadTimeout, tc.want.read},
					{"ReadHeaderTimeout", e.cfg.ReadHeaderTimeout, e.server.ReadHeaderTimeout, tc.want.readHeader},
					{"WriteTimeout", e.cfg.WriteTimeout, e.server.WriteTimeout, tc.want.write},
					{"IdleTimeout", e.cfg.IdleTimeout, e.server.IdleTimeout, tc.want.idle},
				}
				for _, g := range got {
					if g.cfg != g.wantedFrom {
						t.Errorf("cfg.%s = %v, want %v", g.name, g.cfg, g.wantedFrom)
					}
					if g.srv != g.wantedFrom {
						t.Errorf("http.Server.%s = %v, want %v", g.name, g.srv, g.wantedFrom)
					}
					// A negative duration must never reach net/http: its idle
					// path arms a deadline whenever the value is non-zero, so
					// -1 would expire every keep-alive connection instantly.
					if g.srv < 0 {
						t.Errorf("http.Server.%s = %v: negative deadline leaked into net/http", g.name, g.srv)
					}
				}
				_ = e.Shutdown(context.Background())
			})
		}
	}
}
