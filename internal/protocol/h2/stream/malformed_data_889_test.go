package stream

import (
	"bytes"
	"context"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// celeris#889 (review r2 of the PR).

// feedData889 processes a DATA frame on stream id as the frame path does.
func feedData889(t *testing.T, x *proc944, id uint32, endStream bool, data []byte) error {
	t.Helper()
	var buf bytes.Buffer
	if err := http2.NewFramer(&buf, nil).WriteData(id, endStream, data); err != nil {
		t.Fatal(err)
	}
	r := http2.NewFramer(nil, &buf)
	f, err := r.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	return x.ProcessFrame(context.Background(), f)
}

// A Content-Length the body does not match, found when the DATA that ends the
// request arrives (the ordinary shape of a request with a body), is a stream
// error as it is when the request ends with its HEADERS: RST_STREAM, the stream
// freed, the connection kept.
func TestContentLengthMismatchOnDATAIsAStreamError889(t *testing.T) {
	x, _ := newClocked889()
	hs := append(reqHeaders944(), [2]string{"content-length", "5"})
	hs[0][1] = "POST"
	if err := feed944(t, x, "raw", 1, false, encodeHeaders(t, hs)); err != nil {
		t.Fatal(err)
	}
	if err := feedData889(t, x, 1, true, []byte("abc")); err != nil {
		t.Fatalf("a Content-Length mismatch ended the connection: %v", err)
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
		t.Fatal("the refused stream is still in the manager")
	}
	if err := feed944(t, x, "raw", 3, true, encodeHeaders(t, reqHeaders944())); err != nil || x.runs.Load() != 1 {
		t.Fatalf("the next request: err %v, runs %d", err, x.runs.Load())
	}
}

// The flood bound covers it: more than the burst in a second ends the connection.
func TestContentLengthMismatchOnDATAFlood889(t *testing.T) {
	x, _ := newClocked889()
	id := uint32(1)
	var err error
	n := 0
	for ; err == nil && n < 1000; n++ {
		hs := append(reqHeaders944(), [2]string{"content-length", "5"})
		if err = feed944(t, x, "raw", id, false, encodeHeaders(t, hs)); err != nil {
			break
		}
		err = feedData889(t, x, id, true, []byte("abc"))
		id += 2
	}
	if err == nil || n <= 50 || n > 200 {
		t.Fatalf("after %d mismatched requests in a second: err %v, want the connection to end after about 100", n, err)
	}
	if goaways, _ := sinks889(x.Processor); len(goaways) != 1 || goaways[0].code != http2.ErrCodeEnhanceYourCalm {
		t.Fatalf("GOAWAYs %+v, want one ENHANCE_YOUR_CALM", goaways)
	}
}

// RFC 9113 section 5.1 (closed): "An endpoint MUST ignore frames that it
// receives on closed streams after it has sent a RST_STREAM frame". A client
// that sent the trailers of a request before it saw the stream reset gets
// no GOAWAY: its trailer block is decoded (HPACK is the connection's, and the
// block may add to the dynamic table) and dropped.
func TestTrailersOnAStreamTheServerResetAreIgnored889(t *testing.T) {
	for _, path := range []string{"raw", "frame"} {
		t.Run(path, func(t *testing.T) {
			x, _ := newClocked889()
			// A request with a value that is not allowed, still open.
			bad := append(reqHeaders944(), [2]string{"x-bad", "a\x00b"})
			bad[0][1] = "POST"
			if err := feed944(t, x, path, 1, false, encodeHeaders(t, bad)); err != nil {
				t.Fatalf("the malformed request: %v", err)
			}
			if err := feedData889(t, x, 1, false, []byte("abc")); err != nil {
				t.Fatalf("DATA on the reset stream: %v", err)
			}
			// Its trailers, in flight: they add an entry to the dynamic table.
			var tr bytes.Buffer
			enc := newHpackEncoder889(&tr)
			trailer := enc.block(t, [][2]string{{"x-trailer-entry", "kept-in-the-table"}})
			if err := feed944(t, x, path, 1, true, trailer); err != nil {
				t.Fatalf("the trailers of a reset stream ended the connection: %v", err)
			}
			goaways, rst := sinks889(x.Processor)
			if len(goaways) != 0 {
				t.Fatalf("GOAWAYs %+v, want none", goaways)
			}
			if len(rst) != 1 || rst[0].streamID != 1 || rst[0].code != http2.ErrCodeProtocol {
				t.Fatalf("RST_STREAMs %+v, want the one for the malformed request", rst)
			}
			// HPACK is in step: the next request refers to the entry the
			// trailers added.
			next := enc.block(t, append(reqHeaders944(), [2]string{"x-trailer-entry", "kept-in-the-table"}))
			if err := feed944(t, x, path, 3, true, next); err != nil {
				t.Fatalf("the next request: %v", err)
			}
			if x.runs.Load() != 1 {
				t.Fatalf("the handler ran %d times, want 1 (the next request)", x.runs.Load())
			}
		})
	}
}

// A peer cannot make the server decode HEADERS on a reset stream for ever: the
// first HEADERS frame without END_STREAM after the request (RFC 9113 section
// 8.1: malformed) ends the connection, and so does a second block. One block
// of trailers, with END_STREAM, is ignored per reset stream (before, a block
// without END_STREAM was dropped and the stream stayed open for the next one:
// 99 replays on a malformed request, 1990 on a refused stream).
func TestHEADERSOnAResetStreamAreBounded889(t *testing.T) {
	for _, second := range []string{"no-end-stream", "after-the-trailers"} {
		t.Run(second, func(t *testing.T) {
			x, _ := newClocked889()
			bad := append(reqHeaders944(), [2]string{"x-bad", "a\x00b"})
			if err := feed944(t, x, "raw", 1, false, encodeHeaders(t, bad)); err != nil {
				t.Fatal(err)
			}
			if second == "after-the-trailers" {
				if err := feed944(t, x, "raw", 1, true, encodeHeaders(t, [][2]string{{"x-t", "v"}})); err != nil {
					t.Fatalf("the trailers: %v", err)
				}
			}
			err := feed944(t, x, "raw", 1, second == "after-the-trailers", encodeHeaders(t, [][2]string{{"x-t", "v"}}))
			if err == nil {
				t.Fatal("a HEADERS frame past the one block of trailers was ignored")
			}
			if goaways, _ := sinks889(x.Processor); len(goaways) != 1 || goaways[0].code != http2.ErrCodeProtocol {
				t.Fatalf("GOAWAYs %+v, want one PROTOCOL_ERROR", goaways)
			}
		})
	}
}

// A HEADERS frame on a stream that was closed in order, not reset, is still a
// connection error (RFC 9113 5.1; h2spec 5.1).
func TestHEADERSOnAClosedStreamIsStillAConnectionError889(t *testing.T) {
	x, _ := newClocked889()
	if err := feed944(t, x, "raw", 1, true, encodeHeaders(t, reqHeaders944())); err != nil {
		t.Fatal(err)
	}
	if err := feed944(t, x, "raw", 1, true, encodeHeaders(t, reqHeaders944())); err == nil {
		t.Fatal("HEADERS on a closed stream was accepted")
	}
}

// hpackEncoder889 is one HPACK encoder for a connection: its blocks share a
// dynamic table, so a later block may refer to what an earlier one added.
type hpackEncoder889 struct {
	buf *bytes.Buffer
	enc *hpack.Encoder
}

func newHpackEncoder889(buf *bytes.Buffer) *hpackEncoder889 {
	return &hpackEncoder889{buf: buf, enc: hpack.NewEncoder(buf)}
}

func (e *hpackEncoder889) block(t *testing.T, hs [][2]string) []byte {
	t.Helper()
	e.buf.Reset()
	for _, h := range hs {
		if err := e.enc.WriteField(hpack.HeaderField{Name: h[0], Value: h[1]}); err != nil {
			t.Fatal(err)
		}
	}
	return append([]byte(nil), e.buf.Bytes()...)
}

// The trailers end the stream: a HEADERS frame after them is a connection error.
// And one that ended with its HEADERS is not one whose trailers are ignored (a
// stream refused for MAX_CONCURRENT_STREAMS or its header list, or reset for its
// body, is, while its client side is open: celeris#889, the conn tests).
func TestOnlyTheTrailersOfAnOpenResetStreamAreIgnored889(t *testing.T) {
	x, _ := newClocked889()
	bad := append(reqHeaders944(), [2]string{"x-bad", "a\x00b"})
	if err := feed944(t, x, "raw", 1, false, encodeHeaders(t, bad)); err != nil {
		t.Fatal(err)
	}
	if err := feed944(t, x, "raw", 1, true, encodeHeaders(t, [][2]string{{"x-t", "v"}})); err != nil {
		t.Fatalf("the trailers: %v", err)
	}
	if err := feed944(t, x, "raw", 1, true, encodeHeaders(t, [][2]string{{"x-t", "v"}})); err == nil {
		t.Fatal("a HEADERS frame after the trailers was ignored")
	}

	y, _ := newClocked889()
	bad2 := append(reqHeaders944(), [2]string{"x-bad", "a\x00b"})
	if err := feed944(t, y, "raw", 1, true, encodeHeaders(t, bad2)); err != nil { // ended with its HEADERS
		t.Fatal(err)
	}
	if err := feed944(t, y, "raw", 1, true, encodeHeaders(t, [][2]string{{"x-t", "v"}})); err == nil {
		t.Fatal("HEADERS on a stream that ended with its HEADERS was ignored")
	}
}
