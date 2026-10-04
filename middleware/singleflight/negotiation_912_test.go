package singleflight

import (
	"strings"
	"testing"

	"github.com/goceleris/celeris"
)

// TestSingleflightRespondAccept912: celeris#912's family through the
// framework's own content negotiation. c.Respond picks JSON, XML or text from
// Accept, which the default key leaves out. A waiter that sent
// Accept: application/xml took a leader's JSON (with x-singleflight: HIT).
// Respond now names Accept in Vary, so the waiter runs its own handler; a
// waiter with the leader's Accept still takes the leader's response.
func TestSingleflightRespondAccept912(t *testing.T) {
	type item struct{ K string }
	respond := func(c *celeris.Context) error { return c.Respond(200, item{K: "v"}) }
	for _, tc := range []struct {
		name           string
		leader, second string
		wantShared     bool
	}{
		{"json leader, xml waiter", "application/json", "application/xml", false},
		{"xml leader, json waiter", "application/xml", "application/json", false},
		{"xml leader, text waiter", "application/xml", "text/plain", false},
		{"control: same Accept", "application/xml", "application/xml", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runPair(t, New(), []celeris.HandlerFunc{respond},
				[][2]string{{"accept", tc.leader}}, [][2]string{{"accept", tc.second}})
			if !r.joined {
				t.Fatalf("the second request did not join the leader (the key must still coalesce; Vary decides afterwards)")
			}
			ct := r.second.Header("content-type")
			if !strings.HasPrefix(ct, tc.second) {
				t.Errorf("the waiter sent Accept: %s and got Content-Type %q (leader's %q)", tc.second, ct, r.leader.Header("content-type"))
			}
			shared := r.second.Header("x-singleflight") == "HIT"
			if shared != tc.wantShared {
				t.Errorf("waiter x-singleflight %q, want shared=%v", r.second.Header("x-singleflight"), tc.wantShared)
			}
			if wantCalls := map[bool]int32{true: 1, false: 2}[tc.wantShared]; r.calls != wantCalls {
				t.Errorf("the handler ran %d times, want %d", r.calls, wantCalls)
			}
			t.Logf("leader ct %q vary %q; waiter ct %q x-singleflight %q; calls %d", r.leader.Header("content-type"), r.leader.Header("vary"), ct, r.second.Header("x-singleflight"), r.calls)
		})
	}
}
