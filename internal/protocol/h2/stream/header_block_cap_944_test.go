package stream

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// celeris#944, review round 2: the cap on a header block still being
// assembled from CONTINUATION frames (handleContinuation) counts WIRE bytes,
// and an HPACK encoder may spend more wire bytes on a field than the field
// has: RFC 7541 lets it Huffman-code a string even when the code is longer
// (a Huffman code is up to 30 bits for a byte, 19 for a printable one such as
// '\\'), and a string length takes up to 4 bytes. A cap of "the bound, in wire
// bytes" refused a legal list inside the bound with a connection-level
// GOAWAY, when the same list in one HEADERS frame was served.

// hpackInt appends the HPACK integer n with an N-bit prefix (RFC 7541 §5.1)
// to dst, the prefix byte carrying first in its high bits.
func hpackInt(dst []byte, first byte, prefixBits uint, n int) []byte {
	max := 1<<prefixBits - 1
	if n < max {
		return append(dst, first|byte(n))
	}
	dst = append(dst, first|byte(max))
	for n -= max; n >= 128; n /= 128 {
		dst = append(dst, byte(n%128)|0x80)
	}
	return append(dst, byte(n))
}

// literalHuffman944 is one field as "literal without indexing, new name",
// both strings Huffman-coded whether or not that is shorter: legal HPACK,
// and the longest encoding a field has.
func literalHuffman944(dst []byte, name, value string) []byte {
	dst = append(dst, 0x00)
	for _, s := range []string{name, value} {
		h := hpack.AppendHuffmanString(nil, s)
		dst = hpackInt(dst, 0x80, 7, len(h))
		dst = append(dst, h...)
	}
	return dst
}

// listCost944 is what the header list bound charges for fields.
func listCost944(fields [][2]string) int {
	var n int
	for _, f := range fields {
		n += len(f[0]) + len(f[1]) + headerFieldOverhead
	}
	return n
}

// padFieldMax944 is the longest value expandedBlock944 puts in one field: the
// decoder refuses a string whose ENCODED length is over 64 KiB
// (SetMaxStringLength), and 16000 bytes of the longest Huffman code (30 bits)
// are 60,000 bytes encoded. A list near the bound is therefore many fields.
const padFieldMax944 = 16000

// expandedBlock944 is the header block of reqHeaders944 plus fields named
// "x-pad" whose values are padByte repeated, Huffman-coded, so that the whole
// list costs exactly listSize.
func expandedBlock944(t *testing.T, padByte byte, listSize int) []byte {
	t.Helper()
	base := reqHeaders944()
	const perField = len("x-pad") + headerFieldOverhead
	rest := listSize - listCost944(base)
	k := (rest + padFieldMax944 + perField - 1) / (padFieldMax944 + perField)
	total := rest - k*perField // the pad bytes in all
	if k < 1 || total < k {
		t.Fatalf("list size %d leaves no room for the pad", listSize)
	}
	block := encodeHeaders(t, base)
	fields := append([][2]string(nil), base...)
	for i := range k {
		n := total / k
		if i < total%k {
			n++
		}
		pad := string(bytes.Repeat([]byte{padByte}, n))
		block = literalHuffman944(block, "x-pad", pad)
		fields = append(fields, [2]string{"x-pad", pad})
	}
	if got := listCost944(fields); got != listSize {
		t.Fatalf("self-check: the list costs %d, want %d", got, listSize)
	}
	return block
}

// worstHuffmanByte944 is the byte with the longest Huffman code, and the code's
// length in bits.
func worstHuffmanByte944() (b byte, bits uint64) {
	for i := range 256 {
		// HuffmanEncodeLength counts whole bytes; 8 copies are 8 x bits bits,
		// that is exactly bits bytes, so nothing is rounded.
		if l := hpack.HuffmanEncodeLength(strings.Repeat(string([]byte{byte(i)}), 8)); l > bits {
			b, bits = byte(i), l
		}
	}
	return b, bits
}

// TestHuffmanExpandedListOverContinuationIsServed944 is the review's probe: a
// list at 55% of the bound whose values are Huffman-coded '\\' (19 bits for
// 8): the block on the wire is 2.4 times the list. In one HEADERS frame it is
// served; split over HEADERS and CONTINUATION it was refused with a GOAWAY
// that ends the connection.
func TestHuffmanExpandedListOverContinuationIsServed944(t *testing.T) {
	size := maxHeaderListSize * 55 / 100
	block := expandedBlock944(t, '\\', size)
	if len(block) <= maxHeaderListSize {
		t.Fatalf("precondition: the block (%d bytes) must be longer than the bound (%d) for this test to mean anything", len(block), maxHeaderListSize)
	}
	for _, path := range []string{"raw", "frame", "continuation"} {
		t.Run(path, func(t *testing.T) {
			p := newProc944()
			err := feed944(t, p, path, 1, true, block)
			if err != nil {
				t.Errorf("a list of %d bytes (bound %d) in a %d-byte block: %v", size, maxHeaderListSize, len(block), err)
			}
			if g := p.conn.testResponseWriter.goAwaysSent; len(g) != 0 {
				t.Errorf("GOAWAYs %+v for a list inside the bound", g)
			}
			if p.runs.Load() != 1 {
				t.Errorf("the handler ran %d times, want 1 (RSTs %+v)", p.runs.Load(), p.conn.resets())
			}
		})
	}
}

// TestWorstCaseExpansionAtTheBoundIsServed944: the largest block a list of
// exactly the bound can have: its pad made of the byte with the longest
// Huffman code (30 bits). Served over CONTINUATION, as it is in one frame.
// And a list one byte over the bound, in an equally expanded block, is a
// STREAM error (RST_STREAM ENHANCE_YOUR_CALM), not a connection one: the
// block cap never fires before the exact bound does for a block that long.
func TestWorstCaseExpansionAtTheBoundIsServed944(t *testing.T) {
	b, bits := worstHuffmanByte944()
	if bits < 30 {
		t.Fatalf("self-check: the longest Huffman code is %d bits, RFC 7541 Appendix B has 30", bits)
	}
	for _, tc := range []struct {
		name   string
		size   int
		served bool
	}{{"at the bound", maxHeaderListSize, true}, {"one byte over", maxHeaderListSize + 1, false}} {
		block := expandedBlock944(t, b, tc.size)
		for _, path := range []string{"raw", "frame", "continuation"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				p := newProc944()
				err := feed944(t, p, path, 1, true, block)
				t.Logf("list %d, block %d bytes (%.2fx the bound), err=%v, runs %d", tc.size, len(block), float64(len(block))/maxHeaderListSize, err, p.runs.Load())
				if err != nil {
					t.Errorf("a connection error: %v", err)
				}
				if g := p.conn.testResponseWriter.goAwaysSent; len(g) != 0 {
					t.Errorf("GOAWAYs %+v", g)
				}
				if got := p.runs.Load() == 1; got != tc.served {
					t.Errorf("served=%v, want %v", got, tc.served)
				}
				if !tc.served {
					if rst := p.conn.resets(); len(rst) != 1 || rst[0].streamID != 1 || rst[0].code != http2.ErrCodeEnhanceYourCalm {
						t.Errorf("RST_STREAMs %+v, want one ENHANCE_YOUR_CALM on stream 1", rst)
					}
				}
			})
		}
	}
}

// TestFieldEncodingNeverExceedsTheBlockCap944 is the derivation of
// headerBlockExpansion as a test: for fields of every size class, in the
// longest encoding a field has (literal, new name, both strings Huffman-coded
// with the longest code, the length integers at their widest), the field's
// wire bytes are at most headerBlockExpansion x what the bound charges it.
// A list within the bound is then a block within the cap.
func TestFieldEncodingNeverExceedsTheBlockCap944(t *testing.T) {
	b, _ := worstHuffmanByte944()
	lens := []int{0, 1, 2, 7, 8, 100, 126, 127, 128, 1000, 16383, 16384, 20000, maxHeaderListSize - headerFieldOverhead - 1}
	for _, nl := range lens {
		for _, vl := range lens {
			if nl+vl+headerFieldOverhead > maxHeaderListSize {
				continue
			}
			name := string(bytes.Repeat([]byte{b}, nl))
			value := string(bytes.Repeat([]byte{b}, vl))
			wire := len(literalHuffman944(nil, name, value))
			if cost := nl + vl + headerFieldOverhead; wire > headerBlockExpansion*cost {
				t.Errorf("a field of name %d + value %d bytes costs %d in the list and %d on the wire (%.2fx > %dx)",
					nl, vl, cost, wire, float64(wire)/float64(cost), headerBlockExpansion)
			}
		}
	}
}

// TestHeaderBlockFloodStaysBoundedByTheCap944: the flood is still refused,
// at the CONTINUATION frame that crosses the cap, and the cap is what the
// constant says: headerBlockExpansion times the bound.
func TestHeaderBlockFloodStaysBoundedByTheCap944(t *testing.T) {
	p := newProc944()
	if err := feed944(t, p, "raw", 1, true, encodeHeaders(t, reqHeaders944())); err != nil || p.runs.Load() != 1 {
		t.Fatalf("the first request: %v, handler runs %d", err, p.runs.Load())
	}
	var buf bytes.Buffer
	w := http2.NewFramer(&buf, nil)
	if err := w.WriteRawFrame(http2.FrameHeaders, http2.FlagHeadersEndStream, 3, encodeHeaders(t, reqHeaders944())); err != nil {
		t.Fatal(err)
	}
	const frag = 16 << 10
	for range 2 * headerBlockExpansion * maxHeaderListSize / frag {
		if err := w.WriteRawFrame(http2.FrameContinuation, 0, 3, bytes.Repeat([]byte{0xBE}, frag)); err != nil {
			t.Fatal(err)
		}
	}
	r := http2.NewFramer(nil, &buf)
	var perr error
	var kept int
	for {
		f, err := r.ReadFrame()
		if err != nil {
			break
		}
		if perr = p.ProcessFrame(context.Background(), f); perr != nil {
			break
		}
		p.continuationStateMu.Lock()
		if p.continuationState != nil {
			kept = len(p.continuationState.headerBlock)
		}
		p.continuationStateMu.Unlock()
	}
	if perr == nil {
		t.Fatal("the flood was accepted")
	}
	if kept > headerBlockExpansion*maxHeaderListSize {
		t.Errorf("the server kept %d bytes of the block, above the cap %d", kept, headerBlockExpansion*maxHeaderListSize)
	}
	if p.IsExpectingContinuation() {
		t.Error("the processor still expects CONTINUATION frames")
	}
}

// TestGoAwayLastStreamIDDoesNotIncrease944: RFC 9113 §6.8, "endpoints MUST
// NOT increase the value they send in the last stream identifier". A graceful
// GOAWAY names stream 1 (H2State.GoAway: GetLastClientStreamID); the client's
// stream 3 is then opened (which moves the manager's highest stream ID to 3)
// and refused; a CONTINUATION flood on stream 5 gets a second GOAWAY, which
// named GetLastStreamID, 3.
func TestGoAwayLastStreamIDDoesNotIncrease944(t *testing.T) {
	p := newProc944()
	if err := feed944(t, p, "raw", 1, true, encodeHeaders(t, reqHeaders944())); err != nil || p.runs.Load() != 1 {
		t.Fatalf("stream 1: %v, runs %d", err, p.runs.Load())
	}
	if err := p.SendGoAway(p.manager.GetLastClientStreamID(), http2.ErrCodeNo, nil); err != nil {
		t.Fatal(err)
	}
	_ = feed944(t, p, "raw", 3, true, encodeHeaders(t, reqHeaders944()))
	if got := p.manager.GetLastStreamID(); got != 3 {
		t.Fatalf("self-check: the manager's highest stream ID is %d, want 3 (above the 1 the GOAWAY named)", got)
	}
	var buf bytes.Buffer
	w := http2.NewFramer(&buf, nil)
	if err := w.WriteRawFrame(http2.FrameHeaders, http2.FlagHeadersEndStream, 5, encodeHeaders(t, reqHeaders944())); err != nil {
		t.Fatal(err)
	}
	frag := bytes.Repeat([]byte{0xBE}, 16<<10)
	for range 2 * headerBlockExpansion * maxHeaderListSize / len(frag) {
		if err := w.WriteRawFrame(http2.FrameContinuation, 0, 5, frag); err != nil {
			t.Fatal(err)
		}
	}
	r := http2.NewFramer(nil, &buf)
	for {
		f, err := r.ReadFrame()
		if err != nil {
			break
		}
		if p.ProcessFrame(context.Background(), f) != nil {
			break
		}
	}
	g := p.conn.testResponseWriter.goAwaysSent
	if len(g) != 2 || g[0].code != http2.ErrCodeNo || g[1].code != http2.ErrCodeEnhanceYourCalm {
		t.Fatalf("GOAWAYs %+v, want the graceful one and then ENHANCE_YOUR_CALM", g)
	}
	if g[1].lastStreamID > g[0].lastStreamID {
		t.Errorf("the second GOAWAY names last stream %d, above the first's %d", g[1].lastStreamID, g[0].lastStreamID)
	}
}

// TestSendGoAwayNeverRaisesTheLastStreamID944 is the same rule for every
// site: whatever a caller names, the GOAWAY sent names at most what an
// earlier one did, and a lower value is kept.
func TestSendGoAwayNeverRaisesTheLastStreamID944(t *testing.T) {
	p := newProc944()
	for _, id := range []uint32{7, 9, 3, 5, 0, 11} {
		_ = p.SendGoAway(id, http2.ErrCodeNo, nil)
	}
	var got []uint32
	for _, g := range p.conn.testResponseWriter.goAwaysSent {
		got = append(got, g.lastStreamID)
	}
	want := []uint32{7, 7, 3, 3, 0, 0}
	if len(got) != len(want) {
		t.Fatalf("GOAWAYs %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("last stream IDs sent %v, want %v", got, want)
		}
	}
}

// TestNextBlockEmitsAfterAnAbandonedOverBoundBlock944: a block over the bound
// turns the decoder's emit off (hpackEmit), and endHeaderDecode turned it on
// again. A block that fails after that (a truncated one: Close reports it)
// returns a connection error before endHeaderDecode runs. Whatever decodes
// next on the processor must start with emit on all the same, so beginHeaderDecode
// is what turns it on, not the end of the previous block.
func TestNextBlockEmitsAfterAnAbandonedOverBoundBlock944(t *testing.T) {
	p := newProc944()
	var hb bytes.Buffer
	block := bigFieldBlock944(hpack.NewEncoder(&hb), &hb, "x-pad", 4000, 12<<10)
	block = append(block, 0x40, 0x05, 'a') // a literal cut short: Close fails

	var first [][2]string
	p.beginHeaderDecode(&first, true)
	if _, err := p.hpackDecoder.Write(block); err != nil {
		t.Fatal(err)
	}
	if err := p.hpackDecoder.Close(); err == nil {
		t.Fatal("self-check: the truncated block must fail Close")
	}
	if !p.headerListTooLarge {
		t.Fatal("self-check: the block must have tripped the bound")
	}
	// the caller's error path: no endHeaderDecode.

	var second [][2]string
	p.beginHeaderDecode(&second, true)
	if _, err := p.hpackDecoder.Write(encodeHeaders(t, reqHeaders944())); err != nil {
		t.Fatal(err)
	}
	if err := p.hpackDecoder.Close(); err != nil {
		t.Fatal(err)
	}
	if p.endHeaderDecode() {
		t.Error("a 4-field request is over the bound")
	}
	if len(second) != 4 {
		t.Errorf("the next block decoded to %d fields, want 4 (emit left off by the abandoned block)", len(second))
	}
}
