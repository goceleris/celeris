package stream

import (
	"fmt"
	"testing"

	"golang.org/x/net/http2"
)

// celeris#889 (review r1 of the PR): RFC 9113 section 8.2.1 says a field name
// "MUST NOT contain characters in the ranges 0x00-0x20, 0x41-0x5a, or 0x7f-0xff"
// and that a name other than a pseudo-header's "MUST NOT contain a colon"; a
// request that has one is malformed (section 8.1.1). Only the upper case was
// checked, so a name with CR LF, NUL, a space, a colon or a byte above 0x7f
// reached the handler on the native engines.
var badFieldNames889 = []string{
	"x-a\r\nset-cookie", "foo\x00", "\x00", "foo bar", " foo", "foo\t", "foo:bar", "foo:", "fo\xc3\xa9",
	"foo\x7f", "foo\xff", "foo\n", "foo\r", "f(o)o", "foo,bar", "foo/bar", "foo\"", "",
}

func TestValidateRequestHeadersRefusesBadFieldNames889(t *testing.T) {
	for _, name := range badFieldNames889 {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			hs := append(reqHeaders944(), [2]string{name, "x"})
			if err := validateRequestHeaders(hs); err == nil {
				t.Fatal("a request with that field name was accepted")
			}
			if err := validateTrailerHeaders([][2]string{{name, "x"}}); err == nil {
				t.Fatal("a trailer with that field name was accepted")
			}
		})
	}
	// Controls: names that are tokens in lower case stay valid, and so do the
	// pseudo-headers.
	for _, name := range []string{"x-a", "content-type", "a.b", "x_y~z", "0", "x-a1", "x!#$%&'*+-.^_`|~"} {
		hs := append(reqHeaders944(), [2]string{name, "x"})
		if err := validateRequestHeaders(hs); err != nil {
			t.Errorf("name %q refused: %v", name, err)
		}
	}
	if err := validateRequestHeaders(reqHeaders944()); err != nil {
		t.Fatalf("a plain request refused: %v", err)
	}
}

// feedMalformed889 sends a request on stream id that validateRequestHeaders
// refuses (a NUL in the :path), as a complete request.
func feedMalformed889(t *testing.T, x *proc944, path string, id uint32) error {
	t.Helper()
	hs := reqHeaders944()
	hs[2][1] = "/p\x00e"
	return feed944(t, x, path, id, true, encodeHeaders(t, hs))
}

// RFC 9113 section 8.1.1: a malformed request is a stream error. The stream is
// reset with PROTOCOL_ERROR and the connection stays: the next request on it is
// served. (The native engines used to return the error out of the frame loop,
// which closes the connection with the other streams on it; net/http, so std,
// resets the stream only.)
func TestMalformedRequestIsAStreamError889(t *testing.T) {
	for _, path := range []string{"raw", "frame"} {
		t.Run(path, func(t *testing.T) {
			x, _ := newClocked889()
			if err := feedMalformed889(t, x, path, 1); err != nil {
				t.Fatalf("a malformed request ended the connection: %v", err)
			}
			goaways, rst := sinks889(x.Processor)
			if len(rst) != 1 || rst[0].streamID != 1 || rst[0].code != http2.ErrCodeProtocol {
				t.Fatalf("RST_STREAMs %+v, want one PROTOCOL_ERROR on stream 1", rst)
			}
			if len(goaways) != 0 {
				t.Fatalf("GOAWAYs %+v, want none", goaways)
			}
			if x.runs.Load() != 0 {
				t.Fatal("the handler ran for a malformed request")
			}
			if _, ok := x.manager.GetStream(1); ok {
				t.Fatal("the refused stream is still in the manager (its slot is not free)")
			}
			// The connection serves the next request.
			if err := feed944(t, x, path, 3, true, encodeHeaders(t, reqHeaders944())); err != nil {
				t.Fatalf("the next request: %v", err)
			}
			if got := x.runs.Load(); got != 1 {
				t.Fatalf("the handler ran %d times, want 1 (the clean request)", got)
			}
		})
	}
}

// The other malformed-request refusals are stream errors too: a bad trailer
// and a Content-Length that does not match a request that ends with its
// HEADERS.
func TestMalformedTrailersAndLengthAreStreamErrors889(t *testing.T) {
	t.Run("content-length mismatch", func(t *testing.T) {
		x, _ := newClocked889()
		hs := append(reqHeaders944(), [2]string{"content-length", "5"})
		if err := feed944(t, x, "raw", 1, true, encodeHeaders(t, hs)); err != nil {
			t.Fatalf("ended the connection: %v", err)
		}
		_, rst := sinks889(x.Processor)
		if len(rst) != 1 || rst[0].streamID != 1 || rst[0].code != http2.ErrCodeProtocol {
			t.Fatalf("RST_STREAMs %+v, want one PROTOCOL_ERROR on stream 1", rst)
		}
		if err := feed944(t, x, "raw", 3, true, encodeHeaders(t, reqHeaders944())); err != nil || x.runs.Load() != 1 {
			t.Fatalf("the next request: err %v, runs %d", err, x.runs.Load())
		}
	})
	t.Run("trailers", func(t *testing.T) {
		x, _ := newClocked889()
		if err := feed944(t, x, "raw", 1, false, encodeHeaders(t, reqHeaders944())); err != nil {
			t.Fatal(err)
		}
		bad := encodeHeaders(t, [][2]string{{"x-t", "a\x00b"}})
		if err := feed944(t, x, "raw", 1, true, bad); err != nil {
			t.Fatalf("ended the connection: %v", err)
		}
		_, rst := sinks889(x.Processor)
		if len(rst) != 1 || rst[0].streamID != 1 || rst[0].code != http2.ErrCodeProtocol {
			t.Fatalf("RST_STREAMs %+v, want one PROTOCOL_ERROR on stream 1", rst)
		}
		if x.runs.Load() != 0 {
			t.Fatal("the handler ran")
		}
	})
}

// A peer that sends malformed requests for ever is told to calm down, as for
// header lists over the bound: a burst a second, then GOAWAY(ENHANCE_YOUR_CALM).
func TestMalformedRequestFloodEndsTheConnection889(t *testing.T) {
	x, clock := newClocked889()
	id := uint32(1)
	var err error
	n := 0
	for ; err == nil && n < 1000; n++ {
		err = feedMalformed889(t, x, "raw", id)
		id += 2
	}
	if err == nil {
		t.Fatal("1000 malformed requests in one second never ended the connection")
	}
	if n <= 50 || n > 200 {
		t.Fatalf("the connection ended after %d malformed requests in a second, want a burst of about 100", n)
	}
	goaways, _ := sinks889(x.Processor)
	if len(goaways) != 1 || goaways[0].code != http2.ErrCodeEnhanceYourCalm {
		t.Fatalf("GOAWAYs %+v, want one ENHANCE_YOUR_CALM", goaways)
	}
	_ = clock
}

// The next second is a new window: a peer that stays within the burst is never cut off.
func TestMalformedRequestsWithinTheBurstAreServedEverySecond889(t *testing.T) {
	x, clock := newClocked889()
	id := uint32(1)
	for round := range 3 {
		for i := range 50 {
			if err := feedMalformed889(t, x, "raw", id); err != nil {
				t.Fatalf("round %d request %d: %v", round, i, err)
			}
			id += 2
		}
		clock.sec.Add(1)
	}
	if goaways, _ := sinks889(x.Processor); len(goaways) != 0 {
		t.Fatalf("GOAWAYs %+v, want none", goaways)
	}
}
