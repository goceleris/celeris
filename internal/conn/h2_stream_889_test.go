package conn

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// celeris#889 items 37, 39 and 49 at the connection layer: what the peer sees
// of ProcessH2 for a header block over the wire cap, and for a stream the
// server reset while the peer's side of it was still open.

// literal889 is one field as "literal without indexing, new name" (RFC 7541
// section 6.2.2), neither string Huffman-coded: its length on the wire is
// known exactly, and it adds nothing to the dynamic table.
func literal889(dst []byte, name, value string) []byte {
	dst = append(dst, 0x00, byte(len(name)))
	dst = append(dst, name...)
	n := len(value)
	if n < 127 {
		dst = append(dst, byte(n))
	} else {
		dst = append(dst, 127)
		for n -= 127; n >= 128; n /= 128 {
			dst = append(dst, byte(n%128)|0x80)
		}
		dst = append(dst, byte(n))
	}
	return append(dst, value...)
}

// fieldLen889 is the length on the wire of literal889(name, a value of v bytes).
func fieldLen889(name string, v int) int {
	prefix := 1
	if v >= 127 {
		prefix = 2
		for r := v - 127; r >= 128; r /= 128 {
			prefix++
		}
	}
	return 2 + len(name) + prefix + v
}

// blockOfLen889 is the header block of a request (:method GET, :scheme http,
// :path / from the static table, and :authority) followed by "x-pad-NNN"
// literals, whose total length on the wire is exactly n. No value is longer
// than 16000 bytes, well inside the decoder's own string bound.
func blockOfLen889(t *testing.T, n int) []byte {
	t.Helper()
	block := []byte{0x82, 0x86, 0x84} // :method GET, :scheme http, :path /
	block = literal889(block, ":authority", "example.com")
	for i := 0; len(block) < n; i++ {
		name := fmt.Sprintf("x-pad-%03d", i)
		rest := n - len(block)
		v := 16000
		switch {
		case rest <= fieldLen889(name, 16000): // the last field: the value that fills the block exactly
			v = rest
			for fieldLen889(name, v) > rest {
				v--
			}
		case rest-fieldLen889(name, 16000) < fieldLen889("x-pad-000", 0):
			v = 15000 // leave room for a last field
		}
		block = literal889(block, name, strings.Repeat("a", v))
	}
	if len(block) != n {
		t.Fatalf("self-check: block of %d bytes, want %d", len(block), n)
	}
	return block
}

// The header list a server accepts is bounded (SETTINGS_MAX_HEADER_LIST_SIZE,
// 64 KiB), and so is the block that carries it: no list within the bound has a
// block over 4 x the bound (headerBlockExpansion). A block assembled from
// CONTINUATION frames that passes that cap is a connection error
// (GOAWAY ENHANCE_YOUR_CALM). A block that is complete in one HEADERS frame
// was decoded in full, up to the 1 MiB SETTINGS_MAX_FRAME_SIZE, and refused
// afterwards with a stream error: every such frame cost the server the decode
// of a megabyte. RFC 9113 section 10.5.1 lets a server that does not process a
// field block end the connection. It must not decode it and go on, as the
// HPACK state would drift; it ends the connection, as for the assembled block.
func TestProcessH2_OneFrameHeaderBlockOverTheCapEndsTheConnection889(t *testing.T) {
	const capBytes = 4 * 64 << 10
	for _, tc := range []struct {
		name  string
		flags http2.Flags
		pre   []byte // a PRIORITY field in front of the block: not the HEADERS fast path
	}{
		{"fast-path", http2.FlagHeadersEndHeaders | http2.FlagHeadersEndStream, nil},
		{"priority-field", http2.FlagHeadersEndHeaders | http2.FlagHeadersEndStream | http2.FlagHeadersPriority, []byte{0, 0, 0, 0, 15}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &captureHandler{}
			p := newPeer889(t, h, H2Config{})
			if err := p.send(preface889(t)); err != nil {
				t.Fatal(err)
			}
			block := append(append([]byte(nil), tc.pre...), blockOfLen889(t, capBytes+1)...) // the fragment, without the PRIORITY field, is over the cap
			err := p.send(rawFrame889(t, http2.FrameHeaders, tc.flags, 1, block))
			if err == nil {
				t.Error("ProcessH2 returned nil for a one-frame header block over the cap, want the connection error")
			}
			gas := p.goAways()
			if len(gas) != 1 || gas[0].ErrCode != http2.ErrCodeEnhanceYourCalm {
				t.Errorf("GOAWAYs = %v, want one ENHANCE_YOUR_CALM", gas)
			}
			if rs := p.rsts(); len(rs) != 0 {
				t.Errorf("RST_STREAMs = %v, want none: the block must not be decoded and refused as a stream error", rs)
			}
			if h.called {
				t.Error("the handler ran")
			}
		})
	}
}

// A block exactly at the cap is still decoded, and refused as before when its
// list is over the bound (a stream error, the connection goes on): the cap
// is the same one the assembled block is held to (appendContinuation: ">").
func TestProcessH2_OneFrameHeaderBlockAtTheCapIsStillAStreamError889(t *testing.T) {
	const capBytes = 4 * 64 << 10
	h := &captureHandler{}
	p := newPeer889(t, h, H2Config{})
	if err := p.send(preface889(t)); err != nil {
		t.Fatal(err)
	}
	err := p.send(rawFrame889(t, http2.FrameHeaders, http2.FlagHeadersEndHeaders|http2.FlagHeadersEndStream, 1, blockOfLen889(t, capBytes)))
	if err != nil {
		t.Fatalf("a block of exactly the cap: %v", err)
	}
	if gas := p.goAways(); len(gas) != 0 {
		t.Errorf("GOAWAYs = %v, want none", gas)
	}
	rs := p.rsts()
	if len(rs) != 1 || rs[0].StreamID != 1 || rs[0].ErrCode != http2.ErrCodeEnhanceYourCalm {
		t.Errorf("RST_STREAMs = %v, want one ENHANCE_YOUR_CALM on stream 1 (header list over the bound)", rs)
	}
}

// post889 is the HEADERS frame of a POST on stream id (END_STREAM when endStream).
func (p *peer889) post889(id uint32, endStream bool) []byte {
	fl := http2.FlagHeadersEndHeaders
	if endStream {
		fl |= http2.FlagHeadersEndStream
	}
	return rawFrame889(p.t, http2.FrameHeaders, fl, id, p.req889("POST"))
}

// trailers889 is the trailer block of a request as frames: one HEADERS frame
// with END_HEADERS, or (continuation) a HEADERS frame without it and a
// CONTINUATION that ends the block. The block adds an entry to the peer's
// dynamic table, so a request that references it checks the HPACK state of the
// connection.
func (p *peer889) trailers889(id uint32, continuation bool, name string) []byte {
	p.enc.Reset()
	_ = p.henc.WriteField(hpack.HeaderField{Name: name, Value: "kept-in-the-table"})
	block := append([]byte(nil), p.enc.Bytes()...)
	if !continuation {
		return rawFrame889(p.t, http2.FrameHeaders, http2.FlagHeadersEndHeaders|http2.FlagHeadersEndStream, id, block)
	}
	mid := len(block) / 2
	out := rawFrame889(p.t, http2.FrameHeaders, http2.FlagHeadersEndStream, id, block[:mid])
	return append(out, rawFrame889(p.t, http2.FrameContinuation, http2.FlagContinuationEndHeaders, id, block[mid:])...)
}

// answered889 reports whether the server answered stream id with a HEADERS frame.
func (p *peer889) answered889(id uint32) bool {
	for _, got := range p.headers() {
		if got == id {
			return true
		}
	}
	return false
}

// A request whose body is over MaxRequestBodySize is reset (RST_STREAM CANCEL)
// and the connection goes on: every other stream on it is served. Before, the
// reset was followed by an error that ended the whole connection, so a body
// over the limit on one stream took its siblings with it (an h2c proxy that
// multiplexes clients on one upstream connection). The DATA already on the
// wire for the stream is ignored, and what the peer spent of the connection's
// window on it is credited back, or each refused upload would leak window until
// the connection stalls (RFC 9113 sections 5.1 and 6.9.1).
func TestProcessH2_BodyOverTheLimitResetsTheStreamNotTheConnection889(t *testing.T) {
	const limit = 4096
	p := newPeer889(t, respondHandler{body: []byte("ok")}, H2Config{MaxRequestBodySize: limit})
	over := bytes.Repeat([]byte("d"), 10000) // over the limit, and over the 8192 bytes at which window credit is sent
	if err := p.send(append(preface889(t), p.post889(1, false)...)); err != nil {
		t.Fatal(err)
	}
	before := p.connCredit()
	err := p.send(rawFrame889(t, http2.FrameData, 0, 1, over))
	if err != nil {
		t.Fatalf("DATA over MaxRequestBodySize ended the connection: %v", err)
	}
	if rs := p.rsts(); len(rs) != 1 || rs[0].StreamID != 1 || rs[0].ErrCode != http2.ErrCodeCancel {
		t.Fatalf("RST_STREAMs = %v, want one CANCEL on stream 1", rs)
	}
	if gas := p.goAways(); len(gas) != 0 {
		t.Fatalf("GOAWAYs = %v, want none", gas)
	}
	// The body's tail, in flight when the reset went out.
	tail := bytes.Repeat([]byte("t"), 9000)
	if err := p.send(rawFrame889(t, http2.FrameData, http2.FlagDataEndStream, 1, tail)); err != nil {
		t.Fatalf("the rest of the body of the reset stream: %v", err)
	}
	if got, want := p.connCredit()-before, uint32(len(over)+len(tail)); got != want {
		t.Errorf("WINDOW_UPDATE credit for the DATA of the reset stream = %d, want %d (everything the peer spent)", got, want)
	}
	if err := p.send(rawFrame889(t, http2.FrameHeaders, http2.FlagHeadersEndHeaders|http2.FlagHeadersEndStream, 3, p.req889("GET"))); err != nil {
		t.Fatalf("the next request: %v", err)
	}
	if !p.answered889(3) {
		t.Errorf("stream 3 was not answered; answered streams %v", p.headers())
	}
}

// RFC 9113 section 5.1 (closed): "An endpoint MUST ignore frames that it
// receives on closed streams after it has sent a RST_STREAM frame". The
// trailers of a request the server reset, sent before the peer saw the reset,
// are such a frame: the block is decoded and dropped (it may add to the
// dynamic table, which is the connection's), the connection goes on. It was a
// connection error (GOAWAY PROTOCOL_ERROR, "HEADERS on closed stream") for a
// request refused for MAX_CONCURRENT_STREAMS, for its header list, and for a
// body over MaxRequestBodySize.
func TestProcessH2_TrailersOfAStreamTheServerResetAreIgnored889(t *testing.T) {
	hugeList := func(p *peer889) []byte { // a header list over the bound, in a 16 KB block
		p.enc.Reset()
		for _, f := range [][2]string{{":method", "POST"}, {":scheme", "http"}, {":path", "/"}, {":authority", "example.com"}} {
			_ = p.henc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
		}
		_ = p.henc.WriteField(hpack.HeaderField{Name: "x-pad", Value: strings.Repeat("a", 4000)})
		blk := append(append([]byte(nil), p.enc.Bytes()...), bytes.Repeat([]byte{0xBE}, 12<<10)...)
		return rawFrame889(p.t, http2.FrameHeaders, http2.FlagHeadersEndHeaders, 1, blk)
	}
	for _, tc := range []struct {
		name  string
		cfg   H2Config
		setup func(p *peer889) []byte // the frames that make stream 1 or 3 the one the server resets
		id    uint32
		want  http2.ErrCode
	}{
		{"stream-limit", H2Config{MaxConcurrentStreams: 1}, func(p *peer889) []byte {
			return append(p.post889(1, false), p.post889(3, false)...) // stream 1 holds the only slot
		}, 3, http2.ErrCodeRefusedStream},
		{"header-list", H2Config{}, hugeList, 1, http2.ErrCodeEnhanceYourCalm},
		{"body-limit", H2Config{MaxRequestBodySize: 4}, func(p *peer889) []byte {
			return append(p.post889(1, false), rawFrame889(p.t, http2.FrameData, 0, 1, []byte("body-over-the-limit"))...)
		}, 1, http2.ErrCodeCancel},
	} {
		for _, continuation := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/continuation=%v", tc.name, continuation), func(t *testing.T) {
				p := newPeer889(t, respondHandler{body: []byte("ok")}, tc.cfg)
				in := append(preface889(t), tc.setup(p)...)
				if err := p.send(in); err != nil {
					t.Fatalf("the request the server resets: %v", err)
				}
				if rs := p.rsts(); len(rs) != 1 || rs[0].StreamID != tc.id || rs[0].ErrCode != tc.want {
					t.Fatalf("RST_STREAMs = %v, want one on stream %d with %v", rs, tc.id, tc.want)
				}
				// Its DATA and its trailers, in flight.
				in = rawFrame889(t, http2.FrameData, 0, tc.id, []byte("more"))
				in = append(in, p.trailers889(tc.id, continuation, "x-trailer-entry")...)
				if err := p.send(in); err != nil {
					t.Fatalf("the trailers of stream %d, reset by the server, ended the connection: %v", tc.id, err)
				}
				if gas := p.goAways(); len(gas) != 0 {
					t.Fatalf("GOAWAYs = %v, want none (%s)", gas, "RFC 9113 section 5.1: ignore frames on a stream this endpoint reset")
				}
				// HPACK is in step: the next request refers to the entry the
				// dropped trailers added to the dynamic table.
				p.enc.Reset()
				for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {":authority", "example.com"}, {"x-trailer-entry", "kept-in-the-table"}} {
					_ = p.henc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
				}
				next := tc.id + 2
				if tc.name == "stream-limit" {
					// Stream 1 ends, so a slot is free.
					if err := p.send(rawFrame889(t, http2.FrameData, http2.FlagDataEndStream, 1, []byte("x"))); err != nil {
						t.Fatal(err)
					}
					next = 5
				}
				if err := p.send(rawFrame889(t, http2.FrameHeaders, http2.FlagHeadersEndHeaders|http2.FlagHeadersEndStream, next, append([]byte(nil), p.enc.Bytes()...))); err != nil {
					t.Fatalf("the next request: %v", err)
				}
				if !p.answered889(next) {
					t.Errorf("stream %d was not answered; answered streams %v", next, p.headers())
				}
			})
		}
	}
}

// An honest client that has not read the server's SETTINGS may open every
// stream it has queued ("Initially, there is no limit", RFC 9113 section
// 6.5.2), and the server refuses 2000 of them in a second
// (streamRefusalBurstMax) without ending the connection. The trailers of those
// streams, which the client sent before it saw the refusals, are ignored for
// all of them: the burst is not halved by the blocks that follow, and the
// reset set remembers every stream of it. 1500 refused streams, all their
// HEADERS and DATA first, then all their trailers.
func TestProcessH2_TrailersOfAWholeRefusedBurstAreIgnored889(t *testing.T) {
	const n = 1500
	p := newPeer889(t, respondHandler{body: []byte("ok")}, H2Config{MaxConcurrentStreams: 1})
	if err := p.send(append(preface889(t), p.post889(1, false)...)); err != nil { // stream 1 holds the only slot
		t.Fatal(err)
	}
	var in []byte
	for i := range n {
		id := uint32(3 + 2*i)
		in = append(in, p.post889(id, false)...)
		in = append(in, rawFrame889(t, http2.FrameData, 0, id, []byte("body"))...)
	}
	if err := p.send(in); err != nil {
		t.Fatalf("%d refused streams in one second: %v", n, err)
	}
	in = nil
	for i := range n {
		in = append(in, p.trailers889(uint32(3+2*i), false, fmt.Sprintf("x-t-%d", i))...)
	}
	if err := p.send(in); err != nil {
		t.Fatalf("the trailers of %d refused streams: %v", n, err)
	}
	if gas := p.goAways(); len(gas) != 0 {
		t.Fatalf("GOAWAYs = %v, want none", gas)
	}
	if rs := p.rsts(); len(rs) != n {
		t.Errorf("%d RST_STREAMs, want %d", len(rs), n)
	}
}

// RFC 9113 section 8.1: after the HEADERS that open a request, a HEADERS frame
// without END_STREAM is malformed, and the only HEADERS frame a request may
// have after it is its trailers, which end the stream. A reset stream whose
// client side is open ignores that one block (the peer could not know of the
// reset yet); a block without END_STREAM, or a second block, is the peer
// breaking the protocol: the connection ends at the first, with no more work
// done for it. Before, such a block was decoded with emit off and the stream
// stayed open for the next one, 1990 replays a second on a stream refused for
// MAX_CONCURRENT_STREAMS at the cost of a full HPACK decode each.
func TestProcessH2_RepeatedHEADERSOnAResetStreamEndTheConnection889(t *testing.T) {
	bad := func(p *peer889) []byte { // a request the server resets as malformed, its client side open
		p.enc.Reset()
		for _, f := range [][2]string{{":method", "POST"}, {":scheme", "http"}, {":path", "/"}, {":authority", "example.com"}, {"x-bad", "a\x00b"}} {
			_ = p.henc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
		}
		return rawFrame889(p.t, http2.FrameHeaders, http2.FlagHeadersEndHeaders, 1, append([]byte(nil), p.enc.Bytes()...))
	}
	for _, tc := range []struct {
		name  string
		cfg   H2Config
		setup func(p *peer889) []byte
		id    uint32
	}{
		{"stream-limit", H2Config{MaxConcurrentStreams: 1}, func(p *peer889) []byte {
			return append(p.post889(1, false), p.post889(3, false)...)
		}, 3},
		{"malformed", H2Config{}, bad, 1},
		{"body-limit", H2Config{MaxRequestBodySize: 4}, func(p *peer889) []byte {
			return append(p.post889(1, false), rawFrame889(p.t, http2.FrameData, 0, 1, []byte("body-over-the-limit"))...)
		}, 1},
	} {
		for _, second := range []string{"no-end-stream", "after-the-trailers"} {
			t.Run(tc.name+"/"+second, func(t *testing.T) {
				p := newPeer889(t, respondHandler{body: []byte("ok")}, tc.cfg)
				if err := p.send(append(preface889(t), tc.setup(p)...)); err != nil {
					t.Fatalf("the request the server resets: %v", err)
				}
				block := func(end bool) []byte {
					p.enc.Reset()
					_ = p.henc.WriteField(hpack.HeaderField{Name: "x-replay", Value: "v"})
					fl := http2.FlagHeadersEndHeaders
					if end {
						fl |= http2.FlagHeadersEndStream
					}
					return rawFrame889(t, http2.FrameHeaders, fl, tc.id, append([]byte(nil), p.enc.Bytes()...))
				}
				if second == "after-the-trailers" {
					if err := p.send(block(true)); err != nil {
						t.Fatalf("the one trailer block of a reset stream: %v", err)
					}
				}
				err := p.send(block(second == "after-the-trailers"))
				if err == nil {
					t.Fatalf("a HEADERS frame (%s) on a reset stream was ignored, want the connection error at once", second)
				}
				if gas := p.goAways(); len(gas) != 1 || gas[0].ErrCode != http2.ErrCodeProtocol {
					t.Errorf("GOAWAYs = %v, want one PROTOCOL_ERROR", gas)
				}
			})
		}
	}
}

// The trailers end a stream: a HEADERS frame after them, or after the DATA
// that ended the stream, is a connection error (RFC 9113 section 5.1, closed).
// What a server ignores is what was on the wire before the peer saw the reset,
// and that ends with the peer's END_STREAM.
func TestProcessH2_HEADERSAfterTheEndOfAResetStreamAreAConnectionError889(t *testing.T) {
	bad := func(p *peer889) []byte { // a request the server resets as malformed, its client side open
		p.enc.Reset()
		for _, f := range [][2]string{{":method", "POST"}, {":scheme", "http"}, {":path", "/"}, {":authority", "example.com"}, {"x-bad", "a\x00b"}} {
			_ = p.henc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
		}
		return rawFrame889(p.t, http2.FrameHeaders, http2.FlagHeadersEndHeaders, 1, append([]byte(nil), p.enc.Bytes()...))
	}
	for _, tc := range []struct {
		name  string
		after func(p *peer889) []byte // the end of the stream, from the peer
	}{
		{"data-end-stream", func(p *peer889) []byte {
			return rawFrame889(p.t, http2.FrameData, http2.FlagDataEndStream, 1, []byte("x"))
		}},
		{"trailers", func(p *peer889) []byte { return p.trailers889(1, false, "x-t") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPeer889(t, respondHandler{body: []byte("ok")}, H2Config{})
			if err := p.send(append(preface889(t), bad(p)...)); err != nil {
				t.Fatalf("the malformed request: %v", err)
			}
			if err := p.send(tc.after(p)); err != nil {
				t.Fatalf("the end of the reset stream: %v", err)
			}
			err := p.send(p.trailers889(1, false, "x-again"))
			if err == nil {
				t.Fatal("a HEADERS frame on a stream whose client side has ended was ignored, want the connection error")
			}
			if gas := p.goAways(); len(gas) != 1 || gas[0].ErrCode != http2.ErrCodeProtocol {
				t.Errorf("GOAWAYs = %v, want one PROTOCOL_ERROR", gas)
			}
		})
	}
}
