package conn

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h1"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// celeris#1029: a chunked request body that does not arrive in one ProcessH1
// call. The head and every parsed chunk were dropped when a chunk was
// incomplete, so the next call parsed chunk bytes as a request head and
// answered 400. These tests feed ProcessH1 with exact segment boundaries.

const (
	kiB1029 = 1 << 10
	miB1029 = 1 << 20
)

// rec1029 records every request the handler saw: the method+path and the body.
type rec1029 struct {
	paths  []string
	bodies [][]byte
}

func (r *rec1029) HandleStream(_ context.Context, s *stream.Stream) error {
	r.paths = append(r.paths, s.Method+" "+s.Path)
	r.bodies = append(r.bodies, append([]byte(nil), s.GetData()...))
	return nil
}

// pattern1029 returns n bytes that differ by position, so a body that is
// shifted, truncated or reordered does not compare equal.
func pattern1029(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + (i*7+i>>9)%26)
	}
	return b
}

const head1029 = "POST /up HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n"

// chunked1029 encodes body in chunks of size chunk (the last one shorter),
// each with ext as its chunk extension (";x=1" or ""), then the terminating
// chunk and trailer (each trailer line ends in CRLF).
func chunked1029(body []byte, chunk int, ext, trailer string) []byte {
	var b bytes.Buffer
	for off := 0; off < len(body); off += chunk {
		end := min(off+chunk, len(body))
		fmt.Fprintf(&b, "%x%s\r\n", end-off, ext)
		b.Write(body[off:end])
		b.WriteString("\r\n")
	}
	b.WriteString("0\r\n" + trailer + "\r\n")
	return b.Bytes()
}

// feed1029 runs ProcessH1 on segs in order and stops at the first error.
func feed1029(st *H1State, h *rec1029, out *bytes.Buffer, segs ...[]byte) (int, error) {
	for i, seg := range segs {
		// A fresh copy per call: the engines reuse their read buffer, so a
		// state that kept a view of an earlier segment would read garbage.
		cp := append([]byte(nil), seg...)
		err := ProcessH1(context.Background(), cp, st, h, func(b []byte) { out.Write(b) })
		for j := range cp {
			cp[j] = 0xEE
		}
		if err != nil {
			return i, err
		}
	}
	return len(segs), nil
}

// cut1029 splits b into segments of at most k bytes.
func cut1029(b []byte, k int) [][]byte {
	var segs [][]byte
	for len(b) > 0 {
		n := min(k, len(b))
		segs = append(segs, b[:n])
		b = b[n:]
	}
	return segs
}

func wantOne1029(t *testing.T, h *rec1029, out *bytes.Buffer, err error, body []byte) {
	t.Helper()
	if err != nil {
		t.Fatalf("ProcessH1: %v (answer %q)", err, firstLine889(out.String()))
	}
	if len(h.bodies) != 1 {
		t.Fatalf("handler ran %d times, want 1 (answer %q)", len(h.bodies), firstLine889(out.String()))
	}
	if !bytes.Equal(h.bodies[0], body) {
		t.Fatalf("handler saw %d body bytes %.40q..., want %d bytes %.40q...", len(h.bodies[0]), h.bodies[0], len(body), body)
	}
	if strings.Contains(out.String(), " 400 ") {
		t.Fatalf("answer %q", firstLine889(out.String()))
	}
}

// TestProcessH1ChunkedEverySplit1029 splits one small chunked request at every
// single offset (two segments), at every pair of offsets (three segments) and
// byte by byte, for several shapes: one chunk, many chunks, a chunk
// extension, a trailer, and the head alone in the first segment.
func TestProcessH1ChunkedEverySplit1029(t *testing.T) {
	body := pattern1029(40)
	shapes := []struct {
		name string
		wire []byte
		body []byte
	}{
		{"one-chunk", chunked1029(body, 64, "", ""), body},
		{"three-chunks", chunked1029(body, 16, "", ""), body},
		{"one-byte-chunks", chunked1029(body, 1, "", ""), body},
		{"chunk-extension", chunked1029(body, 16, ";name=value", ""), body},
		{"trailer", chunked1029(body, 16, "", "X-Sum: 1\r\nX-Other: two\r\n"), body},
		{"empty-body", chunked1029(nil, 1, "", ""), nil},
	}
	for _, sh := range shapes {
		req := append([]byte(head1029), sh.wire...)
		run := func(t *testing.T, segs ...[]byte) {
			st := NewH1State()
			h := &rec1029{}
			var out bytes.Buffer
			_, err := feed1029(st, h, &out, segs...)
			wantOne1029(t, h, &out, err, sh.body)
			if st.chunkedInProgress || st.buffer.Len() != 0 {
				t.Fatalf("state after the request: chunkedInProgress=%v buffered=%d, want idle", st.chunkedInProgress, st.buffer.Len())
			}
		}
		t.Run(sh.name+"/one-segment", func(t *testing.T) { run(t, req) })
		t.Run(sh.name+"/byte-by-byte", func(t *testing.T) { run(t, cut1029(req, 1)...) })
		t.Run(sh.name+"/every-offset", func(t *testing.T) {
			for i := 1; i < len(req); i++ {
				if t.Failed() {
					return
				}
				t.Run(fmt.Sprintf("at%d", i), func(t *testing.T) { run(t, req[:i], req[i:]) })
			}
		})
		t.Run(sh.name+"/every-offset-pair", func(t *testing.T) {
			// The head alone is the first segment in the first iteration; the
			// pairs after it cover a cut inside every line of the body.
			for i := 1; i < len(req); i += 3 {
				for j := i + 1; j < len(req); j += 5 {
					if t.Failed() {
						return
					}
					run(t, req[:i], req[i:j], req[j:])
				}
			}
		})
	}
}

// TestProcessH1ChunkedSizes1029 is the table of the issue: bodies of 1 B,
// 4 KiB, 64 KiB, 1 MiB and 4 MiB in one chunk and in 4 KiB chunks, delivered
// in 1 segment, 2 segments, and many (8 KiB, 1 KiB, 7 bytes up to 64 KiB of
// body) segments.
func TestProcessH1ChunkedSizes1029(t *testing.T) {
	sizes := []int{1, 4 * kiB1029, 64 * kiB1029, miB1029, 4 * miB1029}
	for _, n := range sizes {
		body := pattern1029(n)
		for _, ch := range []int{n, 4 * kiB1029} {
			wire := append([]byte(head1029), chunked1029(body, ch, "", "")...)
			for _, seg := range []struct {
				name string
				k    int
			}{{"1seg", len(wire)}, {"2seg", (len(wire) + 1) / 2}, {"64KiB-segs", 64 * kiB1029}, {"8KiB-segs", 8 * kiB1029}, {"1KiB-segs", kiB1029}, {"7B-segs", 7}} {
				if seg.k == 7 && n > 64*kiB1029 {
					continue
				}
				if seg.k == kiB1029 && n > miB1029 {
					continue
				}
				t.Run(fmt.Sprintf("body%d/chunk%d/%s", n, ch, seg.name), func(t *testing.T) {
					st := NewH1State()
					st.MaxRequestBodySize = 4 * miB1029
					h := &rec1029{}
					var out bytes.Buffer
					_, err := feed1029(st, h, &out, cut1029(wire, seg.k)...)
					wantOne1029(t, h, &out, err, body)
				})
			}
		}
	}
}

// TestProcessH1ChunkedBoundaries1029 cuts at the structural points of the
// wire format: inside the size line, between the CR and the LF of the size
// line, inside chunk data, between the data and its CRLF, between the CR and
// the LF of that CRLF, inside the terminating "0\r\n\r\n", in the trailer, and
// with the request ending exactly at a segment boundary.
func TestProcessH1ChunkedBoundaries1029(t *testing.T) {
	body := pattern1029(300)
	wireTrailer := "X-A: 1\r\n"
	wire := chunked1029(body, 150, ";e=1", wireTrailer)
	req := append([]byte(head1029), wire...)
	at := func(sub string) int {
		i := bytes.Index(req, []byte(sub))
		if i < 0 {
			t.Fatalf("no %q in the wire", sub)
		}
		return i
	}
	size1 := at("96;e=1\r\n") // "96" is 150 in hex
	data1 := size1 + len("96;e=1\r\n")
	end1 := data1 + 150
	term := at("0\r\nX-A")
	trail := term + len("0\r\n")
	cuts := map[string][]int{
		"inside-head":            {10, len(head1029) - 2, len(head1029) - 1},
		"head-alone":             {len(head1029)},
		"inside-size-line":       {size1 + 1, size1 + 3},
		"between-CR-LF-of-size":  {size1 + len("96;e=1\r")},
		"after-size-line":        {data1},
		"inside-data":            {data1 + 1, data1 + 75, end1 - 1},
		"data-end-before-CRLF":   {end1},
		"between-CR-LF-of-data":  {end1 + 1},
		"after-chunk":            {end1 + 2},
		"inside-terminal-size":   {term},
		"after-terminal-size":    {trail},
		"inside-trailer":         {trail + 3},
		"between-CR-LF-trailer":  {trail + len("X-A: 1\r")},
		"before-final-CRLF":      {trail + len("X-A: 1\r\n")},
		"between-final-CR-LF":    {len(req) - 1},
		"ends-on-boundary-multi": {size1, end1 + 2, trail},
	}
	for name, offs := range cuts {
		t.Run(name, func(t *testing.T) {
			var segs [][]byte
			prev := 0
			for _, o := range offs {
				segs = append(segs, req[prev:o])
				prev = o
			}
			segs = append(segs, req[prev:])
			st := NewH1State()
			h := &rec1029{}
			var out bytes.Buffer
			_, err := feed1029(st, h, &out, segs...)
			wantOne1029(t, h, &out, err, body)
		})
	}
}

// TestProcessH1ChunkedKeepAlive1029: after a chunked request that spans calls
// the connection serves the next requests, pipelined behind the terminator in
// the same segment or sent after it, and a second split chunked upload.
func TestProcessH1ChunkedKeepAlive1029(t *testing.T) {
	b1, b2 := pattern1029(5000), pattern1029(3000)
	r1 := append([]byte(head1029), chunked1029(b1, 1000, "", "")...)
	next := []byte("GET /next HTTP/1.1\r\nHost: a\r\n\r\n")
	r2 := append([]byte(strings.Replace(head1029, "/up", "/two", 1)), chunked1029(b2, 700, "", "")...)

	t.Run("pipelined-in-the-last-segment", func(t *testing.T) {
		st := NewH1State()
		h := &rec1029{}
		var out bytes.Buffer
		all := append(append(append([]byte(nil), r1...), next...), r2...)
		mid := len(head1029) + 1500
		_, err := feed1029(st, h, &out, all[:mid], all[mid:])
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(h.paths); got != "[POST /up GET /next POST /two]" {
			t.Fatalf("handler saw %s", got)
		}
		if !bytes.Equal(h.bodies[0], b1) || len(h.bodies[1]) != 0 || !bytes.Equal(h.bodies[2], b2) {
			t.Fatal("a body differs")
		}
	})
	t.Run("sent-after", func(t *testing.T) {
		st := NewH1State()
		h := &rec1029{}
		var out bytes.Buffer
		for _, k := range []int{1, 13, 997, len(r1)} {
			h.paths, h.bodies = nil, nil
			segs := append(cut1029(r1, k), next)
			segs = append(segs, cut1029(r2, k)...)
			if _, err := feed1029(st, h, &out, segs...); err != nil {
				t.Fatalf("segment size %d: %v", k, err)
			}
			if got := fmt.Sprint(h.paths); got != "[POST /up GET /next POST /two]" {
				t.Fatalf("segment size %d: handler saw %s", k, got)
			}
			if !bytes.Equal(h.bodies[0], b1) || !bytes.Equal(h.bodies[2], b2) {
				t.Fatalf("segment size %d: a body differs", k)
			}
		}
	})
	t.Run("terminator-then-next-request-split", func(t *testing.T) {
		// The terminating CRLF and the first bytes of the next request in one
		// segment, the rest of the next request in the following one.
		st := NewH1State()
		h := &rec1029{}
		var out bytes.Buffer
		cut := len(r1) - 2 // before the final CRLF
		s1 := r1[:cut]
		s2 := append(append([]byte(nil), r1[cut:]...), next[:10]...)
		if _, err := feed1029(st, h, &out, s1, s2, next[10:]); err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(h.paths); got != "[POST /up GET /next]" {
			t.Fatalf("handler saw %s", got)
		}
	})
}

// TestProcessH1ChunkedExpectContinue1029: a split chunked body behind
// "Expect: 100-continue" is answered with exactly one 100 and the callback
// runs once, in one segment and over several.
func TestProcessH1ChunkedExpectContinue1029(t *testing.T) {
	body := pattern1029(2500)
	req := append([]byte("POST /up HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\nTransfer-Encoding: chunked\r\n\r\n"), chunked1029(body, 500, "", "")...)
	for _, k := range []int{len(req), len(req) / 2, 100, 1} {
		t.Run(fmt.Sprintf("segments-of-%d", k), func(t *testing.T) {
			st := NewH1State()
			calls := 0
			st.OnExpectContinue = func(string, string, [][2]string) bool { calls++; return true }
			h := &rec1029{}
			var out bytes.Buffer
			_, err := feed1029(st, h, &out, cut1029(req, k)...)
			wantOne1029(t, h, &out, err, body)
			if n := strings.Count(out.String(), "100 Continue"); n != 1 {
				t.Fatalf("%d 100 Continue answers, want 1", n)
			}
			if calls != 1 {
				t.Fatalf("OnExpectContinue ran %d times, want 1", calls)
			}
		})
	}
}

// TestProcessH1ChunkedLimit1029: the limit is enforced as the size is known.
func TestProcessH1ChunkedLimit1029(t *testing.T) {
	const limit = 64 * kiB1029
	t.Run("declared-size-over-the-limit-before-any-data", func(t *testing.T) {
		for _, line := range []string{
			fmt.Sprintf("%x\r\n", limit+1),
			"ffffffff\r\n",
			"7fffffffffffffff\r\n", // must not overflow the arithmetic
			"7ffffffffffffffe\r\n",
		} {
			st := NewH1State()
			st.MaxRequestBodySize = limit
			h := &rec1029{}
			var out bytes.Buffer
			// the head, then the oversized size line alone: refused on this call
			i, err := feed1029(st, h, &out, []byte(head1029), []byte(line))
			if err == nil || i != 1 || !strings.HasPrefix(out.String(), "HTTP/1.1 413") || len(h.bodies) != 0 {
				t.Fatalf("size line %q: stopped at segment %d, err %v, answer %q, handler calls %d; want 413 on segment 1", strings.TrimSpace(line), i, err, firstLine889(out.String()), len(h.bodies))
			}
		}
	})
	t.Run("sum-of-chunks-over-the-limit", func(t *testing.T) {
		st := NewH1State()
		st.MaxRequestBodySize = limit
		h := &rec1029{}
		var out bytes.Buffer
		body := pattern1029(limit + 1)
		wire := append([]byte(head1029), chunked1029(body, 8*kiB1029, "", "")...)
		_, err := feed1029(st, h, &out, cut1029(wire, 5000)...)
		if err == nil || !strings.HasPrefix(out.String(), "HTTP/1.1 413") || len(h.bodies) != 0 {
			t.Fatalf("err %v, answer %q, handler calls %d; want 413", err, firstLine889(out.String()), len(h.bodies))
		}
	})
	t.Run("one-oversize-chunk-spread-over-calls-buffers-nothing-past-the-limit", func(t *testing.T) {
		st := NewH1State()
		st.MaxRequestBodySize = limit
		h := &rec1029{}
		var out bytes.Buffer
		if _, err := feed1029(st, h, &out, []byte(head1029+fmt.Sprintf("%x\r\n", limit+1))); err == nil {
			t.Fatal("the oversize size line was accepted")
		}
		if got := st.buffer.Len(); got > 2*kiB1029 {
			t.Fatalf("state buffered %d bytes after refusing a %d byte chunk, want only the head", got, limit+1)
		}
	})
	t.Run("exactly-the-limit-spread-over-calls", func(t *testing.T) {
		st := NewH1State()
		st.MaxRequestBodySize = limit
		h := &rec1029{}
		var out bytes.Buffer
		body := pattern1029(limit)
		wire := append([]byte(head1029), chunked1029(body, limit, "", "")...)
		_, err := feed1029(st, h, &out, cut1029(wire, 4000)...)
		wantOne1029(t, h, &out, err, body)
		// the buffered bytes of a chunk in flight never exceed the chunk
		// plus its framing
	})
	t.Run("buffer-bounded-while-a-chunk-is-in-flight", func(t *testing.T) {
		st := NewH1State()
		st.MaxRequestBodySize = limit
		h := &rec1029{}
		var out bytes.Buffer
		body := pattern1029(limit)
		wire := append([]byte(head1029), chunked1029(body, limit, "", "")...)
		maxBuf := 0
		for _, seg := range cut1029(wire, 3000) {
			if _, err := feed1029(st, h, &out, seg); err != nil {
				t.Fatal(err)
			}
			maxBuf = max(maxBuf, st.buffer.Len())
		}
		if maxBuf > limit+kiB1029 {
			t.Fatalf("state buffered up to %d bytes for a %d byte limit", maxBuf, limit)
		}
	})
	t.Run("size-line-without-CRLF-is-bounded", func(t *testing.T) {
		st := NewH1State()
		st.MaxRequestBodySize = limit
		h := &rec1029{}
		var out bytes.Buffer
		junk := bytes.Repeat([]byte{'0'}, 1000)
		if _, err := feed1029(st, h, &out, []byte(head1029)); err != nil {
			t.Fatal(err)
		}
		var err error
		sent := 0
		for i := 0; i < 200 && err == nil; i++ {
			_, err = feed1029(st, h, &out, junk)
			sent += len(junk)
		}
		if err == nil {
			t.Fatalf("200 KB of size line without CRLF accepted, buffered %d", st.buffer.Len())
		}
		if sent > 16*kiB1029 {
			t.Fatalf("the size line was refused only after %d bytes, want a bound of a few KiB", sent)
		}
	})
	t.Run("trailer-without-end-is-bounded", func(t *testing.T) {
		st := NewH1State()
		h := &rec1029{}
		var out bytes.Buffer
		if _, err := feed1029(st, h, &out, []byte(head1029+"0\r\n")); err != nil {
			t.Fatal(err)
		}
		line := []byte("X-Pad: " + strings.Repeat("p", 1000) + "\r\n")
		var err error
		sent := 0
		for i := 0; i < 200 && err == nil; i++ {
			_, err = feed1029(st, h, &out, line)
			sent += len(line)
		}
		if err == nil {
			t.Fatalf("200 KB of unterminated trailer accepted, buffered %d", st.buffer.Len())
		}
		if sent > h1.MaxHeaderSize+8*kiB1029 {
			t.Fatalf("the trailer was refused only after %d bytes, want about MaxHeaderSize (%d)", sent, h1.MaxHeaderSize)
		}
	})
	t.Run("unlimited", func(t *testing.T) {
		st := NewH1State() // MaxRequestBodySize 0: unlimited
		h := &rec1029{}
		var out bytes.Buffer
		body := pattern1029(miB1029)
		wire := append([]byte(head1029), chunked1029(body, 100*kiB1029, "", "")...)
		_, err := feed1029(st, h, &out, cut1029(wire, 30000)...)
		wantOne1029(t, h, &out, err, body)
	})
}

// TestProcessH1ChunkedMalformedSplit1029: a malformed body is still refused
// when it is split.
func TestProcessH1ChunkedMalformedSplit1029(t *testing.T) {
	for name, wire := range map[string]string{
		"bad-size":       "zz\r\nab\r\n0\r\n\r\n",
		"bad-terminator": "2\r\nabXY0\r\n\r\n",
	} {
		for _, k := range []int{len(wire), 3, 1} {
			t.Run(fmt.Sprintf("%s/%d", name, k), func(t *testing.T) {
				st := NewH1State()
				h := &rec1029{}
				var out bytes.Buffer
				_, err := feed1029(st, h, &out, cut1029([]byte(head1029+wire), k)...)
				if err == nil || !strings.HasPrefix(out.String(), "HTTP/1.1 400") || len(h.bodies) != 0 {
					t.Fatalf("err %v answer %q handler calls %d, want 400", err, firstLine889(out.String()), len(h.bodies))
				}
			})
		}
	}
}

// TestProcessH1ChunkedHeaderDeadline1029: ReadHeaderTimeout bounds the head
// only. A chunked body that trickles in must not arm the header deadline.
func TestProcessH1ChunkedHeaderDeadline1029(t *testing.T) {
	st := NewH1State()
	st.ReadHeaderTimeoutNs = 50_000_000
	armed := 0
	st.OnHeaderDeadlineArmed = func() { armed++ }
	h := &rec1029{}
	var out bytes.Buffer
	body := pattern1029(3000)
	wire := append([]byte(head1029), chunked1029(body, 500, "", "")...)
	segs := cut1029(wire, 200)
	if _, err := feed1029(st, h, &out, segs[0]); err != nil {
		t.Fatal(err)
	}
	armed = 0
	for _, seg := range segs[1 : len(segs)-1] {
		if _, err := feed1029(st, h, &out, seg); err != nil {
			t.Fatal(err)
		}
		if st.HeaderDeadlineNs.Load() != 0 {
			t.Fatal("the header deadline is armed in the middle of a chunked body")
		}
	}
	if armed != 0 {
		t.Fatalf("OnHeaderDeadlineArmed ran %d times during the body", armed)
	}
	if _, err := feed1029(st, h, &out, segs[len(segs)-1]); err != nil {
		t.Fatal(err)
	}
	wantOne1029(t, h, &out, nil, body)
}

// TestProcessH1ChunkedH2CUpgrade1029: an Upgrade: h2c request whose chunked
// body spans calls is handed to HTTP/2 whole, with the bytes behind it.
func TestProcessH1ChunkedH2CUpgrade1029(t *testing.T) {
	settings := base64.RawURLEncoding.EncodeToString([]byte{0, 3, 0, 0, 0, 100})
	head := "POST /up HTTP/1.1\r\nHost: a\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: " + settings + "\r\nTransfer-Encoding: chunked\r\n\r\n"
	body := pattern1029(9000)
	tail := []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
	wire := append(append([]byte(head), chunked1029(body, 3000, "", "")...), tail...)
	for _, k := range []int{len(wire), 1000, 7} {
		t.Run(fmt.Sprintf("segments-of-%d", k), func(t *testing.T) {
			st := NewH1State()
			st.EnableH2Upgrade = true
			h := &rec1029{}
			var out bytes.Buffer
			_, err := feed1029(st, h, &out, cut1029(wire, k)...)
			if !errors.Is(err, ErrUpgradeH2C) || st.UpgradeInfo == nil {
				t.Fatalf("err %v, UpgradeInfo %v; want ErrUpgradeH2C (answer %q)", err, st.UpgradeInfo, firstLine889(out.String()))
			}
			if !bytes.Equal(st.UpgradeInfo.Body, body) {
				t.Fatalf("upgrade body %d bytes, want %d", len(st.UpgradeInfo.Body), len(body))
			}
			// What the call that ended the request also carried: the whole tail
			// when it came in that segment, else its first bytes (the engine
			// reads the rest from the socket once HTTP/2 owns the connection).
			if !bytes.HasPrefix(tail, st.UpgradeInfo.Remaining) || (k >= len(wire) && !bytes.Equal(st.UpgradeInfo.Remaining, tail)) {
				t.Fatalf("upgrade remaining %q, want %q or a prefix of it", st.UpgradeInfo.Remaining, tail)
			}
		})
	}
}

// TestProcessH1ChunkedBoundaryFlags1029: a half-received chunked body is not a
// request boundary (the #383 transplant check) and it leaves the conn pending
// for the dispatch goroutine; a finished one is a boundary again.
func TestProcessH1ChunkedBoundaryFlags1029(t *testing.T) {
	st := NewH1State()
	h := &rec1029{}
	var out bytes.Buffer
	body := pattern1029(2000)
	wire := append([]byte(head1029), chunked1029(body, 700, "", "")...)
	for i, seg := range cut1029(wire, 500) {
		last := i == len(cut1029(wire, 500))-1
		if _, err := feed1029(st, h, &out, seg); err != nil {
			t.Fatal(err)
		}
		if !last && (st.AtRequestBoundary() || !st.HasPendingData() || !st.HasPendingDispatchState()) {
			t.Fatalf("segment %d: boundary=%v pending=%v pendingDispatch=%v  mid-body", i, st.AtRequestBoundary(), st.HasPendingData(), st.HasPendingDispatchState())
		}
	}
	if !st.AtRequestBoundary() || st.HasPendingData() {
		t.Fatal("not at a request boundary after the body completed")
	}
}

// TestProcessH1ChunkedOverflowSafeLimit1029: a size line near MaxInt64 after
// some bytes were decoded must not wrap the running total (decoded+size).
func TestProcessH1ChunkedOverflowSafeLimit1029(t *testing.T) {
	for _, line := range []string{"7fffffffffffffff", "7ffffffffffffffe", "7fffffffffffffff;x=y"} {
		st := NewH1State()
		st.MaxRequestBodySize = 64 * kiB1029
		h := &rec1029{}
		var out bytes.Buffer
		i, err := feed1029(st, h, &out, []byte(head1029+"1\r\na\r\n"), []byte(line+"\r\n"))
		if err == nil || i != 1 || !strings.HasPrefix(out.String(), "HTTP/1.1 413") || len(h.bodies) != 0 {
			t.Fatalf("size line %q after one decoded byte: stopped at segment %d, err %v, answer %q; want 413 on segment 1", line, i, err, firstLine889(out.String()))
		}
	}
}

// TestProcessH1ChunkedDeadlineArmedForTheNextHead1029: when the segment that
// ends a split chunked body also starts the next request's head, the header
// deadline is armed for that head (the entry of the call did not arm it).
func TestProcessH1ChunkedDeadlineArmedForTheNextHead1029(t *testing.T) {
	st := NewH1State()
	st.ReadHeaderTimeoutNs = 5_000_000_000
	h := &rec1029{}
	var out bytes.Buffer
	wire := append([]byte(head1029), chunked1029(pattern1029(100), 50, "", "")...)
	cut := len(wire) / 2
	if _, err := feed1029(st, h, &out, wire[:cut], append(append([]byte(nil), wire[cut:]...), "GET /n"...)); err != nil {
		t.Fatal(err)
	}
	if len(h.bodies) != 1 {
		t.Fatalf("handler ran %d times, want 1", len(h.bodies))
	}
	if st.HeaderDeadlineNs.Load() == 0 {
		t.Fatal("a partial next head is buffered and no header deadline is armed for it")
	}
}

// TestProcessH1ChunkedConnectionClose1029: a split chunked request that asked
// for Connection: close is served and then the call ends the connection, as
// for a request that arrives whole.
func TestProcessH1ChunkedConnectionClose1029(t *testing.T) {
	body := pattern1029(900)
	req := append([]byte("POST /up HTTP/1.1\r\nHost: a\r\nConnection: close\r\nTransfer-Encoding: chunked\r\n\r\n"), chunked1029(body, 300, "", "")...)
	for _, k := range []int{len(req), len(req) / 2, 50} {
		st := NewH1State()
		h := &rec1029{}
		var out bytes.Buffer
		_, err := feed1029(st, h, &out, cut1029(req, k)...)
		if !errors.Is(err, errConnectionClose) || len(h.bodies) != 1 || !bytes.Equal(h.bodies[0], body) {
			t.Fatalf("segments of %d: err %v, handler calls %d; want errConnectionClose after one served request", k, err, len(h.bodies))
		}
	}
}
