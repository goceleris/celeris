package stream

import (
	"bytes"
	"context"
	"testing"

	"golang.org/x/net/http2"
)

// celeris#889 item 39 ("Related"): an empty DATA frame costs the peer 9 bytes
// and the server a frame parse and, on a stream the server reset, a round of
// the window-update books that sends nothing; it was counted nowhere. The
// frames that do nothing (no data, no END_STREAM) are counted per connection:
// a policy number, emptyDataBurstMax = 10000 a second, a maintainer-tunable
// default (net/http's server has no such budget; its answer to each frame is
// bounded by its control-frame queue instead). An honest client sends an empty
// DATA frame to end a stream (END_STREAM: not counted) or, rarely, to flush.
// feedPaddedEmpty889 is a DATA frame with PADDED set, pad length 0 and no data:
// Length 1, no data bytes, 10 bytes on the wire.
func feedPaddedEmpty889(t *testing.T, x *proc944, id uint32) error {
	t.Helper()
	var buf bytes.Buffer
	if err := http2.NewFramer(&buf, nil).WriteDataPadded(id, false, nil, []byte{}); err != nil {
		t.Fatal(err)
	}
	f, err := http2.NewFramer(nil, &buf).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if df := f.(*http2.DataFrame); f.Header().Length != 1 || len(df.Data()) != 0 {
		t.Fatalf("frame length %d, data %d: not a padding-only frame", f.Header().Length, len(df.Data()))
	}
	return x.ProcessFrame(context.Background(), f)
}

func TestEmptyDataFramesAreCounted889(t *testing.T) {
	const burst = 10000
	setup := func(t *testing.T) (*proc944, *clock889, uint32) {
		x, c := newClocked889()
		hdr := reqHeaders944()
		hdr[0][1] = "POST"
		// A reset stream (malformed request, client side open) and a live one.
		bad := append(append([][2]string(nil), hdr...), [2]string{"x-bad", "a\x00b"})
		if err := feed944(t, x, "raw", 1, false, encodeHeaders(t, bad)); err != nil {
			t.Fatal(err)
		}
		if err := feed944(t, x, "raw", 3, false, encodeHeaders(t, hdr)); err != nil {
			t.Fatal(err)
		}
		return x, c, 0
	}
	for _, padded := range []bool{false, true} { // a frame with no data bytes, with or without padding
		feed := func(t *testing.T, x *proc944, id uint32) error {
			if padded {
				return feedPaddedEmpty889(t, x, id)
			}
			return feedData889(t, x, id, false, nil)
		}
		for _, id := range []uint32{1, 3} { // 1: reset by the server; 3: live
			name := "live"
			if id == 1 {
				name = "reset"
			}
			if padded {
				name += "-padded"
			}
			t.Run(name, func(t *testing.T) {
				x, c, _ := setup(t)
				for i := range burst {
					if err := feed(t, x, id); err != nil {
						t.Fatalf("empty DATA %d of %d in a second: %v", i+1, burst, err)
					}
				}
				// END_STREAM on an empty frame is how a body ends: not counted.
				if g, _ := sinks889(x.Processor); len(g) != 0 {
					t.Fatalf("GOAWAYs %+v within the burst", g)
				}
				if err := feed(t, x, id); err == nil {
					t.Fatalf("empty DATA %d in a second did not end the connection", burst+1)
				}
				if g, _ := sinks889(x.Processor); len(g) != 1 || g[0].code != http2.ErrCodeEnhanceYourCalm {
					t.Fatalf("GOAWAYs %+v, want one ENHANCE_YOUR_CALM", g)
				}
				_ = c
			})
		}
	}
	t.Run("the-next-second-is-a-new-window-and-END_STREAM-is-free", func(t *testing.T) {
		x, c, _ := setup(t)
		for round := range 3 {
			for i := range burst {
				if err := feedData889(t, x, 1, false, nil); err != nil {
					t.Fatalf("round %d: empty DATA %d: %v", round, i+1, err)
				}
			}
			c.sec.Add(1)
		}
		if err := feedData889(t, x, 3, true, nil); err != nil {
			t.Fatalf("an empty DATA frame with END_STREAM: %v", err)
		}
	})
}
