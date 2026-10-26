//go:build linux

package iouring

import "testing"

// celeris#884 item 76: a recv-end error that arrives after the running handler
// released detachMu, but before its hand-back (closeOwed) was drained, took
// closeOnRecvEnd's non-busy path, which called OnError(second) itself, and
// closeConn then delivered the error parked by the first one: a detached
// middleware was told [second, first], the later death and then the original.
// The first error is the conn's story (celeris#867); the busy path kept it, and
// epoll's closeOnReadEnd does here too (celeris#1059, "in place of err").
//
// Shown at unit level: the second recv-end CQE of a conn whose close was
// deferred (an RST after the FIN) is not reproduced end to end.
func TestSecondRecvEndErrorAfterTheHandlerReturned884(t *testing.T) {
	for _, tc := range []struct {
		name string
		errs []error // closeOnRecvEnd calls while the handler runs (the first parks)
		want []error // what OnError is told, in order, once the handler has returned
	}{
		{"second-after-release-keeps-the-first", []error{errFirst867}, []error{errFirst867}},
		// The control: with nothing parked the error that arrives is the story.
		{"nothing-parked-delivers-the-error-that-arrived", nil, []error{errSecond867}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, release := parkRecvEnd867(t, true, tc.errs...)
			release() // the handler returned; its hand-back is not drained yet
			if rig.w.conns[rig.local] != rig.cs {
				t.Fatal("apparatus: the conn closed before the second recv-end error")
			}
			rig.w.closeOnRecvEnd(rig.local, rig.cs, errSecond867)
			// closeConn may have deferred behind the hand-back: drain whatever
			// close is left, which is where a parked error is delivered.
			if rig.w.conns[rig.local] == rig.cs {
				rig.w.closeConn(rig.local)
			}
			t.Logf("celeris884.76 case=%s OnError calls=%v closeErr_after=%v", tc.name, rig.notified, rig.cs.closeErr)
			if len(rig.notified) != len(tc.want) {
				t.Fatalf("OnError was called %d times with %v, want %d times with %v", len(rig.notified), rig.notified, len(tc.want), tc.want)
			}
			for i := range tc.want {
				if rig.notified[i] != tc.want[i] {
					t.Errorf("OnError call %d got %v, want %v", i, rig.notified[i], tc.want[i])
				}
			}
			if rig.cs.closeErr != nil {
				t.Errorf("closeErr = %v after the close, want it consumed", rig.cs.closeErr)
			}
		})
	}
}
