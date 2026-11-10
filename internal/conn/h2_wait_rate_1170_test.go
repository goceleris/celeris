package conn

import (
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// celeris#1170 (review r2 F4): the reset of a wait that was judged goes only if the
// call cancelled the stream. A stream the peer reset between the verdict and the
// enqueue (its RST_STREAM was processed in between) gets no second RST_STREAM
// (RFC 9113 §5.1), which #906's expiry sent.
func TestWaitExpiryAfterThePeersResetSendsNoSecondRST1170(t *testing.T) {
	for _, tc := range []struct {
		name       string
		peerFirst  bool
		wantResets int
	}{{"the server alone", false, 1}, {"the peer reset it first", true, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWaitBound893(t, "async", time.Hour) // the handler waits; its own timer will not fire
			time.Sleep(150 * time.Millisecond)          // it is blocked in sendRest
			s, ok := w.st.processor.GetManager().GetStream(1)
			if !ok {
				t.Fatal("no stream 1")
			}
			if tc.peerFirst {
				s.Cancel() // the peer's RST_STREAM, as the processor applies it
			}
			w.st.adapter.resetStreamOnce(s, http2.ErrCodeInternal)
			<-w.done // the cancel ends the handler's wait
			w.st.DrainWriteQueue(w.write)
			_, _, _, rst := w.wire1()
			if len(rst) != tc.wantResets {
				t.Fatalf("%d RST_STREAM frames on the wire (%v), want %d", len(rst), rst, tc.wantResets)
			}
		})
	}
}
