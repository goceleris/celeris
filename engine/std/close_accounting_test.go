package std

import (
	"net"
	"net/http"
	"testing"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

// TestConnStateHookCountsCloses: EngineMetrics.CloseCount was declared for
// every engine but this one never wrote it, so a std cell reported 0 closes
// forever. celeris#624 turns on the comparison engine_closed vs hook_closed to
// separate a close that skipped its hook from a connection lost to a
// transplant hand-off; std can do neither, which is exactly why it is the
// control — but only once its close count is real.
//
// Drives the ConnState hook directly: the transitions are the whole contract,
// and a real dial would make the StateClosed callback asynchronous.
func TestConnStateHookCountsCloses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state http.ConnState
		want  uint64
	}{
		{"closed", http.StateClosed, 1},
		{"hijacked", http.StateHijacked, 1},
		{"active_is_not_a_close", http.StateActive, 0},
		{"idle_is_not_a_close", http.StateIdle, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disconnects := 0
			e, err := New(resource.Config{
				Addr:         "127.0.0.1:0",
				Engine:       engine.Std,
				Protocol:     engine.HTTP1,
				OnDisconnect: func(string) { disconnects++ },
			}, &echoHandler{})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			c, peer := net.Pipe()
			defer func() { _ = c.Close(); _ = peer.Close() }()

			e.connStateHook(c, http.StateNew)
			e.connStateHook(c, tc.state)

			if got := e.Metrics().CloseCount; got != tc.want {
				t.Errorf("Metrics().CloseCount = %d, want %d — the field is "+
					"declared for every engine and this one leaves it at zero",
					got, tc.want)
			}
			// The whole value of std as a control is that its close count and
			// its hook count cannot disagree.
			if uint64(disconnects) != e.Metrics().CloseCount {
				t.Errorf("OnDisconnect fired %d times but CloseCount = %d — "+
					"engine_closed and hook_closed must be identical on std",
					disconnects, e.Metrics().CloseCount)
			}
			// And the close count must move with the gauge, never apart.
			wantActive := int64(1) - int64(tc.want)
			if got := e.Metrics().ActiveConnections; got != wantActive {
				t.Errorf("ActiveConnections = %d, want %d", got, wantActive)
			}
		})
	}
}
