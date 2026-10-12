package stream

import (
	"testing"

	"golang.org/x/net/http2"
)

// DATA's padding is flow-controlled: the sender's windows shrink by the whole
// payload, the Pad Length byte and the padding included (RFC 9113 sections
// 6.1 and 6.9.1), so the WINDOW_UPDATE credit that gives it back is the
// payload's length, not the data's. A client that pads its DATA and is
// credited for the data only runs its connection window down for good.
func TestPaddedDataIsCreditedInFull889(t *testing.T) {
	p := newProc944()
	e := newEnc981()
	if err := p.ProcessRawHeaders(1, false, e.req()); err != nil { // stream 1 waits for its body
		t.Fatal(err)
	}
	const data, pad = 9000, 200 // 9000+1+200 bytes of payload: over the 8192 at which credit is sent (padding is at most 255 bytes)
	const flowControlled = data + 1 + pad
	err := processFrames981(t, p.Processor, func(w *http2.Framer) {
		if werr := w.WriteDataPadded(1, false, make([]byte, data), make([]byte, pad)); werr != nil {
			t.Fatal(werr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	fw := p.writer.(*testFrameWriter)
	fw.mu.Lock()
	conn, strm := fw.windowUpdates[0], fw.windowUpdates[1]
	fw.mu.Unlock()
	if conn != flowControlled || strm != flowControlled {
		t.Errorf("WINDOW_UPDATE credit: connection %d, stream %d, want %d for both (the whole payload)", conn, strm, flowControlled)
	}
}
