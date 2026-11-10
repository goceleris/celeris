package stream

import (
	"testing"

	"golang.org/x/net/http2"
)

// celeris#889 item 49: a request body over MaxRequestBodySize resets its stream
// and keeps the connection, and the resets are counted like every other
// refusal the server makes for the price of a few bytes: 100 in a second are
// served, the 101st ends the connection with GOAWAY ENHANCE_YOUR_CALM, and the
// next second is a new window.
func TestBodyLimitResetsAreCounted889(t *testing.T) {
	x, c := newClocked889()
	x.MaxRequestBodySize = 4
	hdr := reqHeaders944()
	hdr[0][1] = "POST"
	block := encodeHeaders(t, hdr)
	next := uint32(1)
	oneReset := func() error {
		id := next
		next += 2
		if err := feed944(t, x, "raw", id, false, block); err != nil {
			t.Fatalf("stream %d: %v", id, err)
		}
		return feedData889(t, x, id, false, []byte("body over the limit"))
	}
	for round := range 3 {
		for i := range 100 {
			if err := oneReset(); err != nil {
				t.Fatalf("round %d: reset %d of 100 in a second ended the connection: %v", round, i+1, err)
			}
		}
		if goaways, _ := sinks889(x.Processor); len(goaways) != 0 {
			t.Fatalf("round %d: GOAWAYs %+v within the burst", round, goaways)
		}
		c.sec.Add(1) // the next second: a new window
	}
	for i := range 100 {
		if err := oneReset(); err != nil {
			t.Fatalf("reset %d of 100 in the last second ended the connection: %v", i+1, err)
		}
	}
	if err := oneReset(); err == nil {
		t.Fatal("the 101st body-limit reset in a second did not end the connection")
	}
	goaways, rst := sinks889(x.Processor)
	if len(goaways) != 1 || goaways[0].code != http2.ErrCodeEnhanceYourCalm {
		t.Fatalf("GOAWAYs %+v, want one ENHANCE_YOUR_CALM", goaways)
	}
	if len(rst) != 400 {
		t.Errorf("%d RST_STREAMs, want 400 (the 101st is not answered with one, the connection ends)", len(rst))
	}
}
