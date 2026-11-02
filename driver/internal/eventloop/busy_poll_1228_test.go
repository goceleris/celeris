package eventloop

import (
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

type busyPollServer struct{ d time.Duration }

func (busyPollServer) EventLoopProvider() engine.EventLoopProvider { return nil }
func (s busyPollServer) SocketBusyPoll() time.Duration             { return s.d }

// plainServer is a ServerProvider that does not report a busy poll budget.
type plainServer struct{}

func (plainServer) EventLoopProvider() engine.EventLoopProvider { return nil }

// celeris#1228: the driver's budget is its own option when positive, else the
// server's, else 0 (off).
func TestBusyPoll(t *testing.T) {
	const srv, own = 37 * time.Microsecond, 29 * time.Microsecond
	for _, tc := range []struct {
		name     string
		sp       ServerProvider
		explicit time.Duration
		want     time.Duration
	}{
		{"no server, no option", nil, 0, 0},
		{"no server, option", nil, own, own},
		{"server without the interface", plainServer{}, 0, 0},
		{"server without the interface, option", plainServer{}, own, own},
		{"server off", busyPollServer{}, 0, 0},
		{"server on", busyPollServer{srv}, 0, srv},
		{"option wins over the server", busyPollServer{srv}, own, own},
		{"option wins over a server that is off", busyPollServer{}, own, own},
		{"negative option falls through", busyPollServer{srv}, -own, srv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := BusyPoll(tc.sp, tc.explicit); got != tc.want {
				t.Errorf("BusyPoll = %v, want %v", got, tc.want)
			}
		})
	}
}
