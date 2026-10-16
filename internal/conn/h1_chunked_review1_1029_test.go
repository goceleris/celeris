package conn

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h1"
)

// celeris#1029 review round 1: pipelined bursts, the size-line bound at the
// CR/LF cut, the memory a chunked upload in flight holds, and the buffer a
// large upload leaves behind.

// TestScanChunkedDoneLeavesTheBuffer1029: when the terminator is decoded and
// something follows it, scanChunked reports where the request ends and leaves
// state.buffer as it was. Moving what follows the request down over the decoded chunks (the first
// version did) costs O(n) per chunked request, so k pipelined chunked requests
// in one buffer cost O(k*n); the caller drops the request with Next(end).
func TestScanChunkedDoneLeavesTheBuffer1029(t *testing.T) {
	body := pattern1029(300)
	req := append([]byte(head1029), chunked1029(body, 100, ";e=1", "X-T: 1\r\n")...)
	tail := []byte("GET /next HTTP/1.1\r\nHost: a\r\n\r\nGET /after HTTP/1.1\r\nHost: a\r\n\r\n")
	st := NewH1State()
	st.buffer.Write(req)
	st.buffer.Write(tail)
	st.chunkHeadLen = len(head1029)
	before := append([]byte(nil), st.buffer.Bytes()...)
	done, end, err := st.scanChunked(func([]byte) {})
	if err != nil || !done {
		t.Fatalf("scanChunked: done=%v err=%v", done, err)
	}
	if end != len(req) {
		t.Fatalf("end = %d, want %d (head + chunks + trailer)", end, len(req))
	}
	if !bytes.Equal(st.buffer.Bytes(), before) {
		t.Fatal("scanChunked moved bytes in state.buffer after the terminator was decoded")
	}
	if !bytes.Equal(st.chunkBuf, body) {
		t.Fatalf("decoded body %d bytes, want %d", len(st.chunkBuf), len(body))
	}
}

// TestProcessH1ChunkedPipelinedBurst1029: k chunked requests (with different
// bodies) pipelined in one buffer, delivered whole, split at the boundary of
// every request, and in segments that cut requests in the middle, are all
// served in order with their own bodies.
func TestProcessH1ChunkedPipelinedBurst1029(t *testing.T) {
	const k = 40
	var wire bytes.Buffer
	var bodies [][]byte
	for i := 0; i < k; i++ {
		body := pattern1029(1 + i*3)
		bodies = append(bodies, body)
		fmt.Fprintf(&wire, "POST /r%d HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n", i)
		wire.Write(chunked1029(body, 1+i%7, "", ""))
		if i%5 == 0 {
			wire.WriteString("GET /g HTTP/1.1\r\nHost: a\r\n\r\n")
			bodies = append(bodies, nil)
		}
	}
	for _, seg := range []int{wire.Len(), 8192, 1460, 97, 13, 1} {
		st := NewH1State()
		h := &rec1029{}
		var out bytes.Buffer
		if _, err := feed1029(st, h, &out, cut1029(wire.Bytes(), seg)...); err != nil {
			t.Fatalf("segments of %d: %v (answer %q)", seg, err, firstLine889(out.String()))
		}
		if len(h.bodies) != len(bodies) {
			t.Fatalf("segments of %d: handler ran %d times, want %d (answer %q)", seg, len(h.bodies), len(bodies), firstLine889(out.String()))
		}
		for i := range bodies {
			if !bytes.Equal(h.bodies[i], bodies[i]) {
				t.Fatalf("segments of %d: request %d (%s) saw %q, want %q", seg, i, h.paths[i], h.bodies[i], bodies[i])
			}
		}
		if st.buffer.Len() != 0 || st.chunkedInProgress {
			t.Fatalf("segments of %d: state not idle after the burst", seg)
		}
	}
}

// TestProcessH1ChunkedSizeLineBoundSegmentation1029: a size line of n bytes
// (before its CRLF) is served or refused the same way whether it arrives whole,
// with its CR in and its LF not yet, or byte by byte. The bound is
// h1.MaxChunkLineSize: a line of exactly that many bytes is served.
func TestProcessH1ChunkedSizeLineBoundSegmentation1029(t *testing.T) {
	for _, n := range []int{h1.MaxChunkLineSize - 2, h1.MaxChunkLineSize - 1, h1.MaxChunkLineSize, h1.MaxChunkLineSize + 1, h1.MaxChunkLineSize + 2} {
		wire := []byte(head1029 + "1;" + strings.Repeat("x", n-2) + "\r\nQ\r\n0\r\n\r\n")
		crEnd := len(head1029) + n + 1 // just after the CR of the size line
		want := n <= h1.MaxChunkLineSize
		for name, segs := range map[string][][]byte{
			"whole":          {wire},
			"cut-after-CR":   {wire[:crEnd], wire[crEnd:]},
			"cut-before-CR":  {wire[:crEnd-1], wire[crEnd-1:]},
			"cut-after-LF":   {wire[:crEnd+1], wire[crEnd+1:]},
			"byte-by-byte":   cut1029(wire, 1),
			"head-then-rest": {wire[:len(head1029)], wire[len(head1029):]},
		} {
			st := NewH1State()
			h := &rec1029{}
			var out bytes.Buffer
			_, err := feed1029(st, h, &out, segs...)
			served := err == nil && len(h.bodies) == 1 && string(h.bodies[0]) == "Q"
			if served != want {
				t.Errorf("line of %d bytes, %s: served=%v err=%v answer %q; want served=%v", n, name, served, err, firstLine889(out.String()), want)
			}
			if !want && !strings.HasPrefix(out.String(), "HTTP/1.1 400") {
				t.Errorf("line of %d bytes, %s: answer %q, want 400", n, name, firstLine889(out.String()))
			}
		}
	}
}

// chunkedUpload1029 feeds a chunked upload of the given chunk sizes, each
// delivered in pieces of at most piece bytes, and returns the peak of
// cap(chunkBuf) and of cap(chunkBuf)+buffer.Cap() seen between calls.
func chunkedUpload1029(t *testing.T, limit int64, sizes []int, piece int, tail string) (peakChunk, peakBoth int, st *H1State, h *rec1029) {
	t.Helper()
	st = NewH1State()
	st.MaxRequestBodySize = limit
	h = &rec1029{}
	var wire bytes.Buffer
	wire.WriteString(head1029)
	for _, n := range sizes {
		fmt.Fprintf(&wire, "%x\r\n", n)
		wire.Write(bytes.Repeat([]byte{'x'}, n))
		wire.WriteString("\r\n")
	}
	wire.WriteString("0\r\n\r\n" + tail)
	var out bytes.Buffer
	for _, seg := range cut1029(wire.Bytes(), piece) {
		if _, err := feed1029(st, h, &out, seg); err != nil {
			t.Fatalf("upload %v: %v (answer %q)", sizes, err, firstLine889(out.String()))
		}
		peakChunk = max(peakChunk, cap(st.chunkBuf))
		peakBoth = max(peakBoth, cap(st.chunkBuf)+st.buffer.Cap())
	}
	return peakChunk, peakBoth, st, h
}

// TestProcessH1ChunkedUploadMemoryWithinLimit1029: the decoded body of a
// chunked upload in flight never has more capacity than the limit (doubling
// without a clamp reached 2x the limit: a chunk of limit-1 bytes and then one
// of 1 byte made a 2*limit-2 slice), and chunkBuf plus state.buffer stay below
// 2x the limit plus framing for several chunk sequences.
func TestProcessH1ChunkedUploadMemoryWithinLimit1029(t *testing.T) {
	const limit = 1 << 20
	for _, sizes := range [][]int{
		{limit - 1, 1},
		{limit / 3, 2 * limit / 3},
		{limit / 2, limit / 2},
		{1, limit - 1},
		{limit - 1000, 1000},
		{limit / 4, limit / 4, limit / 4, limit / 4},
		{70000, 70000, 70000, limit - 3*70000},
	} {
		peakChunk, peakBoth, _, h := chunkedUpload1029(t, limit, sizes, 16<<10, "")
		if len(h.bodies) != 1 {
			t.Fatalf("%v: handler ran %d times", sizes, len(h.bodies))
		}
		t.Logf("%v: peak cap(chunkBuf) = %.3fx limit, cap(chunkBuf)+cap(buffer) = %.3fx", sizes, float64(peakChunk)/limit, float64(peakBoth)/limit)
		if peakChunk > limit {
			t.Errorf("%v: cap(chunkBuf) reached %d, over the limit %d", sizes, peakChunk, limit)
		}
		if peakBoth > 2*limit+64<<10 {
			t.Errorf("%v: chunkBuf + buffer reached %d, over 2x the limit plus framing", sizes, peakBoth)
		}
	}
}

// TestProcessH1ChunkedUploadMemoryRandom1029: the same bounds over random chunk
// sequences (a few chunks, one of them large, sizes summing to at most the
// limit) delivered in random piece sizes. Seeded, so a failure is reproducible.
func TestProcessH1ChunkedUploadMemoryRandom1029(t *testing.T) {
	const limit = 512 << 10
	rng := rand.New(rand.NewSource(1029))
	worst := 0.0
	for i := 0; i < 300; i++ {
		var sizes []int
		left := limit
		for left > 0 && len(sizes) < 6 {
			n := 1 + rng.Intn(left)
			if rng.Intn(3) == 0 {
				n = 1 + rng.Intn(min(left, 2000))
			}
			sizes = append(sizes, n)
			left -= n
		}
		piece := []int{1460, 4096, 16 << 10, 64 << 10}[rng.Intn(4)]
		peakChunk, peakBoth, _, h := chunkedUpload1029(t, limit, sizes, piece, "")
		if len(h.bodies) != 1 {
			t.Fatalf("iteration %d %v: handler ran %d times", i, sizes, len(h.bodies))
		}
		worst = max(worst, float64(peakBoth)/limit)
		if peakChunk > limit || peakBoth > 2*limit+64<<10 {
			t.Fatalf("iteration %d sizes %v piece %d: cap(chunkBuf) %d, chunkBuf+buffer %d (limit %d)", i, sizes, piece, peakChunk, peakBoth, limit)
		}
	}
	t.Logf("worst cap(chunkBuf)+cap(buffer) over 300 sequences: %.3fx the limit", worst)
}

// TestProcessH1ChunkedBufferReleasedWithATail1029: a large chunked upload gives
// its grown state.buffer back whether the terminator ends the segment or a
// pipelined request (or the start of one) follows it, and the pipelined bytes
// are kept and served.
func TestProcessH1ChunkedBufferReleasedWithATail1029(t *testing.T) {
	const get = "GET /next HTTP/1.1\r\nHost: a\r\n\r\n"
	for _, tail := range []string{"", "G", get[:20], get, get + get} {
		t.Run(fmt.Sprintf("tail=%.12q", tail), func(t *testing.T) {
			_, _, st, h := chunkedUpload1029(t, 4<<20, []int{3<<20 + 5}, 64<<10, tail)
			if c := st.buffer.Cap(); c > maxRetainedChunkedBuffer {
				t.Fatalf("state.buffer keeps %d bytes of capacity after the upload, want at most %d", c, maxRetainedChunkedBuffer)
			}
			if st.buffer.Len() != len(tail)-strings.Count(tail, get)*len(get) {
				t.Fatalf("state.buffer holds %d bytes, want the unserved part of the tail", st.buffer.Len())
			}
			if want := 1 + strings.Count(tail, get); len(h.bodies) != want {
				t.Fatalf("handler ran %d times, want %d", len(h.bodies), want)
			}
			// What was kept must still be a working start of the next request.
			if len(tail) > 0 && !strings.HasSuffix(tail, "\r\n\r\n") {
				var out bytes.Buffer
				rest := get[len(tail)%len(get):]
				if tail == "G" {
					rest = get[1:]
				}
				if _, err := feed1029(st, h, &out, []byte(rest)); err != nil || len(h.paths) != 2 || h.paths[1] != "GET /next" {
					t.Fatalf("the buffered start of the next request did not complete: err %v paths %v", err, h.paths)
				}
			}
		})
	}
}

// distinctCaps1029 feeds one chunked upload of a single chunk of n bytes in
// pieces and returns how many different capacities state.buffer had between
// calls, and the number of calls.
func distinctCaps1029(t *testing.T, limit int64, n, piece int) (caps, calls int) {
	t.Helper()
	st := NewH1State()
	st.MaxRequestBodySize = limit
	h := &rec1029{}
	var out bytes.Buffer
	wire := append([]byte(head1029), chunked1029(bytes.Repeat([]byte{'x'}, n), n, "", "")...)
	seen := map[int]bool{}
	for _, seg := range cut1029(wire, piece) {
		if _, err := feed1029(st, h, &out, seg); err != nil {
			t.Fatalf("limit %d: %v (answer %q)", limit, err, firstLine889(out.String()))
		}
		seen[st.buffer.Cap()] = true
		calls++
	}
	if len(h.bodies) != 1 || len(h.bodies[0]) != n {
		t.Fatalf("limit %d: handler saw %d requests", limit, len(h.bodies))
	}
	return len(seen), calls
}

// TestProcessH1ChunkedBufferGrowthIsNotPerRecv1029: one large chunk arriving in
// small pieces reallocates state.buffer a few times, not once per recv, with a
// limit (the chunk's remainder is reserved when its size line is in) and
// without one (nothing reserves it: geometric growth). An exactly sized buffer
// that grew on every recv would copy the whole body once per recv.
func TestProcessH1ChunkedBufferGrowthIsNotPerRecv1029(t *testing.T) {
	const n = 8 << 20
	for _, limit := range []int64{0, 16 << 20} {
		caps, calls := distinctCaps1029(t, limit, n, 16<<10)
		t.Logf("limit %d: %d calls, %d distinct buffer capacities", limit, calls, caps)
		if caps > 20 {
			t.Errorf("limit %d: state.buffer had %d different capacities over %d calls, want a logarithmic number", limit, caps, calls)
		}
	}
}

// TestProcessH1ChunkedUploadCapAfterTheSecondChunk1029: the sequence a limit-1
// byte chunk followed by a 1-byte chunk, delivered so that the second chunk's
// data is in state.buffer when the measurement is taken: chunkBuf must not
// have doubled past the limit (it reached 2*limit-2 before the clamp).
func TestProcessH1ChunkedUploadCapAfterTheSecondChunk1029(t *testing.T) {
	const limit = 1 << 20
	st := NewH1State()
	st.MaxRequestBodySize = limit
	h := &rec1029{}
	var out bytes.Buffer
	var wire bytes.Buffer
	wire.WriteString(head1029)
	fmt.Fprintf(&wire, "%x\r\n", limit-1)
	wire.Write(bytes.Repeat([]byte{'x'}, limit-1))
	wire.WriteString("\r\n1\r\n")
	for _, seg := range cut1029(wire.Bytes(), 64<<10) {
		if _, err := feed1029(st, h, &out, seg); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := feed1029(st, h, &out, []byte("y\r\n")); err != nil {
		t.Fatal(err)
	}
	if c := cap(st.chunkBuf); c > limit || len(st.chunkBuf) != limit {
		t.Fatalf("after the second chunk: len(chunkBuf)=%d cap(chunkBuf)=%d, want len %d and cap at most the limit", len(st.chunkBuf), c, limit)
	}
	if both := cap(st.chunkBuf) + st.buffer.Cap(); both > 2*limit+64<<10 {
		t.Fatalf("chunkBuf + buffer = %d bytes, over 2x the limit plus framing", both)
	}
	if _, err := feed1029(st, h, &out, []byte("0\r\n\r\n")); err != nil || len(h.bodies) != 1 || len(h.bodies[0]) != limit {
		t.Fatalf("terminator: err %v, handler calls %d", err, len(h.bodies))
	}
}

// TestScanChunkedDoneWithNothingAfter1029: with nothing behind the terminator
// the decoded bytes are cut off the end of state.buffer, so only the head is
// left and the caller drops it (the layout 9ebc502's compaction left, which the
// A/B showed matters for the next requests: +9..17% on a split 4 KiB body
// without it).
func TestScanChunkedDoneWithNothingAfter1029(t *testing.T) {
	body := pattern1029(300)
	req := append([]byte(head1029), chunked1029(body, 100, "", "")...)
	st := NewH1State()
	st.buffer.Write(req)
	st.chunkHeadLen = len(head1029)
	done, end, err := st.scanChunked(func([]byte) {})
	if err != nil || !done {
		t.Fatalf("scanChunked: done=%v err=%v", done, err)
	}
	if end != len(head1029) || st.buffer.Len() != len(head1029) {
		t.Fatalf("end=%d buffered=%d, want both %d (the head)", end, st.buffer.Len(), len(head1029))
	}
	if !bytes.Equal(st.chunkBuf, body) {
		t.Fatalf("decoded body %d bytes, want %d", len(st.chunkBuf), len(body))
	}
}
