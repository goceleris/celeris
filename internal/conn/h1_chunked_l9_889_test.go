package conn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httputil"
	"strings"
	"testing"
	"unsafe"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// celeris#889 (lane L9): items 51, 52 and 53 of the polish list, on the
// chunked request body that #1045 lets span recvs.

// inline889 is a state as the engines run it before a conn is promoted: the
// handler runs on the worker, and a route named /async stops for the dispatch
// goroutine.
func inline889() (*H1State, *rec1029) {
	st := NewH1State()
	st.InlineMode = true
	st.RouteAsync = func(_, path string) bool { return path == "/async" }
	return st, &rec1029{}
}

const asyncReq889 = "GET /async HTTP/1.1\r\nHost: a\r\n\r\n"

// TestProcessH1ChunkedThenAsyncPipelined889: an Async route pipelined behind a
// chunked request, in the same recv, must stop for the dispatch goroutine like
// one that opens a recv. The request behind a chunked body took the buffered
// parse path, which has no RouteAsync check, so on main the handler of the
// Async route ran inline.
func TestProcessH1ChunkedThenAsyncPipelined889(t *testing.T) {
	st, h := inline889()
	var out bytes.Buffer
	body := pattern1029(300)
	wire := append(chunked1029(body, 100, "", ""), asyncReq889...)
	wire = append([]byte(head1029), wire...)
	_, err := feed1029(st, h, &out, wire)
	if !errors.Is(err, ErrAsyncDispatch) {
		t.Fatalf("err = %v, want ErrAsyncDispatch (handled %v)", err, h.paths)
	}
	if len(h.paths) != 1 || h.paths[0] != "POST /up" || !bytes.Equal(h.bodies[0], body) {
		t.Fatalf("handler saw %v, want the one chunked upload and not the Async route", h.paths)
	}
	if got := st.buffer.String(); got != asyncReq889 {
		t.Fatalf("buffer = %q, want the Async request alone, for the dispatch goroutine", got)
	}
	// The dispatch goroutine then runs it (InlineMode off).
	st.InlineMode = false
	if err := ProcessH1(context.Background(), st.TakeBufferedBytes(), st, h, func([]byte) {}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(h.paths) != 2 || h.paths[1] != "GET /async" {
		t.Fatalf("after the replay the handler saw %v", h.paths)
	}
}

// TestProcessH1SplitChunkedStaysInline889: a chunked body that spans recvs
// leaves state the engines read to decide whether to promote the conn to its
// dispatch goroutine. Neither predicate may say yes for it: it is neither
// partial headers nor a body that must be continued there (the continuation
// re-parses a head that was already checked against RouteAsync). And what is
// pipelined behind the terminator still reaches the goroutine.
func TestProcessH1SplitChunkedStaysInline889(t *testing.T) {
	body := pattern1029(5000)
	wire := append([]byte(head1029), chunked1029(body, 1000, "", "")...)
	for _, cut := range []int{len(head1029) + 10, len(head1029) + 1500, len(wire) - 3} {
		t.Run(fmt.Sprintf("cut-%d", cut), func(t *testing.T) {
			st, h := inline889()
			var out bytes.Buffer
			if _, err := feed1029(st, h, &out, wire[:cut]); err != nil {
				t.Fatal(err)
			}
			if !st.chunkedInProgress {
				t.Fatal("premise: the chunked body is not in progress after the first segment")
			}
			if st.HasPendingDispatchState() {
				t.Errorf("HasPendingDispatchState is true for a chunked body in progress: io_uring promotes the conn for life")
			}
			if st.NeedsDispatchGoroutine() {
				t.Errorf("NeedsDispatchGoroutine is true for a chunked body in progress: epoll promotes the conn for life")
			}
			if !st.HasPendingData() {
				t.Errorf("HasPendingData is false for a chunked body in progress: the revert and transplant gates need it")
			}
			// The rest of the body, then an Async request in the same recv.
			_, err := feed1029(st, h, &out, append(append([]byte(nil), wire[cut:]...), asyncReq889...))
			if !errors.Is(err, ErrAsyncDispatch) {
				t.Fatalf("err = %v, want ErrAsyncDispatch (handled %v)", err, h.paths)
			}
			if len(h.paths) != 1 || !bytes.Equal(h.bodies[0], body) {
				t.Fatalf("handled %v, want the one chunked upload with its %d body bytes", h.paths, len(body))
			}
			if got := st.buffer.String(); got != asyncReq889 {
				t.Fatalf("buffer = %q, want the Async request alone", got)
			}
		})
	}
}

// TestProcessH1SplitChunkedThenSyncRequests889: after a split chunked upload
// on a conn that is not promoted, small sync requests keep running inline and
// leave nothing pending.
func TestProcessH1SplitChunkedThenSyncRequests889(t *testing.T) {
	st, h := inline889()
	var out bytes.Buffer
	body := pattern1029(4000)
	wire := append([]byte(head1029), chunked1029(body, 700, "", "")...)
	if _, err := feed1029(st, h, &out, wire[:900], wire[900:2500], wire[2500:]); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := feed1029(st, h, &out, []byte("GET /ping HTTP/1.1\r\nHost: a\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		if st.HasPendingDispatchState() || st.NeedsDispatchGoroutine() || st.HasPendingData() {
			t.Fatalf("request %d left pending state", i)
		}
	}
	if len(h.paths) != 6 {
		t.Fatalf("handled %v", h.paths)
	}
}

// TestChunkedFramingBoundMatchesNetHTTP889 (item 53): non-data framing is
// bounded per request as net/http bounds it (16 KiB of excess: each chunk adds
// the size line and the CRLF after its data, and subtracts 16 plus twice its
// data, floored at 0). The same wire is given to net/http's chunked reader and
// to ProcessH1, in one recv and in several, and celeris must answer 400
// exactly when net/http reports an error. On main every one of these was
// served.
func TestChunkedFramingBoundMatchesNetHTTP889(t *testing.T) {
	type shape struct {
		name   string
		build  func(n int) []byte
		counts []int
	}
	ext := func(k int) string { return ";" + strings.Repeat("e", k-1) }
	chunks := func(n, size int, extLen int) []byte {
		var b bytes.Buffer
		for i := 0; i < n; i++ {
			e := ""
			if extLen > 0 {
				e = ext(extLen)
			}
			fmt.Fprintf(&b, "%x%s\r\n%s\r\n", size, e, strings.Repeat("x", size))
		}
		b.WriteString("0\r\n\r\n")
		return b.Bytes()
	}
	shapes := []shape{
		// 1-byte chunks, extension 100: 85 bytes of excess each, 16384/85 = 192.7.
		{"1B-ext100", func(n int) []byte { return chunks(n, 1, 100) }, []int{1, 100, 190, 191, 192, 193, 194, 200}},
		// 1-byte chunks, extension 4000: 3985 each, the 5th passes 16 KiB.
		{"1B-ext4000", func(n int) []byte { return chunks(n, 1, 4000) }, []int{1, 3, 4, 5, 6, 40}},
		// 1-byte chunks, no extension: 3 - 18 < 0, never any excess.
		{"1B-plain", func(n int) []byte { return chunks(n, 1, 0) }, []int{1, 500, 5000}},
		// Large chunks bank no credit: a 1 MiB-class chunk first (floored at
		// 0), then 1-byte chunks with extensions.
		{"big-then-small", func(n int) []byte {
			var b bytes.Buffer
			fmt.Fprintf(&b, "%x\r\n%s\r\n", 5000, strings.Repeat("y", 5000))
			b.Write(chunks(n, 1, 100))
			return b.Bytes()
		}, []int{100, 192, 194, 250}},
		// 16-byte chunks with a 64-byte extension: 64+1+... = 51 each, minus
		// the 2*16 the data buys back: 19 per chunk.
		{"16B-ext64", func(n int) []byte { return chunks(n, 16, 64) }, []int{800, 860, 870, 900}},
	}
	for _, sh := range shapes {
		for _, n := range sh.counts {
			t.Run(fmt.Sprintf("%s/%d", sh.name, n), func(t *testing.T) {
				chunkedWire := sh.build(n)
				_, nhErr := io.ReadAll(httputil.NewChunkedReader(bytes.NewReader(chunkedWire)))
				for _, piece := range []int{len(chunkedWire), 1500} {
					st := NewH1State()
					st.MaxRequestBodySize = 1 << 20
					h := &rec1029{}
					var out bytes.Buffer
					wire := append([]byte(head1029), chunkedWire...)
					_, err := feed1029(st, h, &out, cut1029(wire, piece)...)
					refused := err != nil
					if refused != (nhErr != nil) {
						t.Fatalf("piece %d: net/http error %v, celeris error %v (answer %q, handled %d)", piece, nhErr, err, firstLine889(out.String()), len(h.paths))
					}
					if refused && !strings.HasPrefix(out.String(), "HTTP/1.1 400") {
						t.Fatalf("piece %d: refused with %q, want 400", piece, firstLine889(out.String()))
					}
				}
			})
		}
	}
}

// TestChunkedFramingCountsResetPerRequest889: the excess is per request. Two
// pipelined requests that each stay under the bound are both served, in one
// recv and byte by byte.
func TestChunkedFramingCountsResetPerRequest889(t *testing.T) {
	one := chunked1029(bytes.Repeat([]byte{'z'}, 120), 1, ";"+strings.Repeat("e", 99), "")
	wire := append(append([]byte(head1029), one...), append([]byte(head1029), one...)...)
	for _, piece := range []int{len(wire), 997, 1} {
		st := NewH1State()
		st.MaxRequestBodySize = 1 << 20
		h := &rec1029{}
		var out bytes.Buffer
		if _, err := feed1029(st, h, &out, cut1029(wire, piece)...); err != nil {
			t.Fatalf("piece %d: %v (answer %q)", piece, err, firstLine889(out.String()))
		}
		if len(h.paths) != 2 {
			t.Fatalf("piece %d: served %d requests, want 2", piece, len(h.paths))
		}
	}
}

// capProbe889 records, when the handler runs (the moment both copies of a
// one-chunk body exist), the capacity held by the decoded body and by
// state.buffer.
type capProbe889 struct {
	st   *H1State
	body int
	held int
}

func (p *capProbe889) HandleStream(_ context.Context, s *stream.Stream) error {
	d := s.GetData()
	p.body = len(d)
	p.held = cap(d) + p.st.buffer.Cap()
	// A body that is a view of state.buffer's array is one copy, not two.
	if len(d) > 0 {
		avail := p.st.buffer.AvailableBuffer()
		end := uintptr(unsafe.Pointer(unsafe.SliceData(avail))) + uintptr(p.st.buffer.Available())
		start := end - uintptr(p.st.buffer.Cap())
		if at := uintptr(unsafe.Pointer(unsafe.SliceData(d))); at >= start && at < end {
			p.held = p.st.buffer.Cap()
		}
	}
	return nil
}

// TestChunkedSingleChunkCapacity889 (item 52): a chunked upload whose body is
// one large chunk, received in pieces, holds the undecoded chunk in
// state.buffer and, when it completes, the decoded copy too: about 2x the
// body at the moment the handler runs. The bound asserted is the body plus
// 64 KiB of slack for the head and framing.
func TestChunkedSingleChunkCapacity889(t *testing.T) {
	const limit = 1 << 20
	for _, n := range []int{limit / 2, limit - 1000} {
		for _, piece := range []int{4096, 64 << 10} {
			t.Run(fmt.Sprintf("%d/piece%d", n, piece), func(t *testing.T) {
				st := NewH1State()
				st.MaxRequestBodySize = limit
				p := &capProbe889{st: st}
				wire := append([]byte(head1029), chunked1029(pattern1029(n), n, "", "")...)
				var out bytes.Buffer
				for _, seg := range cut1029(wire, piece) {
					cp := append([]byte(nil), seg...)
					if err := ProcessH1(context.Background(), cp, st, p, func(b []byte) { out.Write(b) }); err != nil {
						t.Fatalf("ProcessH1: %v (answer %q)", err, firstLine889(out.String()))
					}
				}
				if p.body != n {
					t.Fatalf("handler saw %d body bytes, want %d", p.body, n)
				}
				if p.held > n+(64<<10) {
					t.Fatalf("cap(body)+cap(buffer) = %d for a %d byte body: %.2fx", p.held, n, float64(p.held)/float64(n))
				}
			})
		}
	}
}

// keepBody889 keeps every body it is given, as an async or detached handler
// keeps c.Body() past its return.
type keepBody889 struct{ bodies [][]byte }

func (k *keepBody889) HandleStream(_ context.Context, s *stream.Stream) error {
	k.bodies = append(k.bodies, s.GetData())
	return nil
}

// TestChunkedSingleChunkBodyStaysIntact889: a body that is a view of
// state.buffer's array (item 52) must not be overwritten by what the conn
// receives next. One large chunk, in one recv (the buffered path) and in
// several (the resume path), then more requests through the same state, with a
// pipelined tail in the recv that ends the body.
func TestChunkedSingleChunkBodyStaysIntact889(t *testing.T) {
	for _, n := range []int{200 << 10, 70 << 10} {
		for _, piece := range []int{1 << 30, 10000} {
			for _, tail := range []string{"", "POST /next HTTP/1.1\r\nHost: a\r\nContent-Length: 4\r\n\r\nWXYZ"} {
				t.Run(fmt.Sprintf("%d/piece%d/tail%d", n, piece, len(tail)), func(t *testing.T) {
					st := NewH1State()
					st.MaxRequestBodySize = 1 << 20
					k := &keepBody889{}
					body := pattern1029(n)
					wire := append(append([]byte(head1029), chunked1029(body, n, "", "")...), tail...)
					for _, seg := range cut1029(wire, piece) {
						cp := append([]byte(nil), seg...)
						if err := ProcessH1(context.Background(), cp, st, k, func([]byte) {}); err != nil {
							t.Fatal(err)
						}
						for j := range cp {
							cp[j] = 0xEE
						}
					}
					// More traffic through the same buffers: chunked and plain.
					for i := 0; i < 3; i++ {
						junk := bytes.Repeat([]byte{byte('A' + i)}, 3000)
						more := append([]byte(head1029), chunked1029(junk, 1000, "", "")...)
						if err := ProcessH1(context.Background(), more, st, k, func([]byte) {}); err != nil {
							t.Fatal(err)
						}
					}
					if len(k.bodies) < 4 || !bytes.Equal(k.bodies[0], body) {
						t.Fatalf("the first body changed after later requests (%d bodies)", len(k.bodies))
					}
				})
			}
		}
	}
}

// TestExpectContinuePanicLogged889 (item 46): a panicking OnExpectContinue is
// still a refusal, and now leaves a log record with the panic value and the
// stack, at both sites (a fixed-length body and a chunked one).
func TestExpectContinuePanicLogged889(t *testing.T) {
	for _, head := range []string{
		"POST /up HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\nContent-Length: 5\r\n\r\n",
		"POST /up HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\nTransfer-Encoding: chunked\r\n\r\n",
	} {
		var logbuf bytes.Buffer
		st := NewH1State()
		st.Logger = slog.New(slog.NewTextHandler(&logbuf, nil))
		st.OnExpectContinue = func(string, string, [][2]string) bool { panic("boom-expect-889") }
		h := &rec1029{}
		var out bytes.Buffer
		_, err := feed1029(st, h, &out, []byte(head))
		if err == nil || !strings.HasPrefix(out.String(), "HTTP/1.1 417") || len(h.paths) != 0 {
			t.Fatalf("err %v, answer %q, handled %v: want the 417 and the close kept", err, firstLine889(out.String()), h.paths)
		}
		got := logbuf.String()
		if !strings.Contains(got, "boom-expect-889") || !strings.Contains(got, "stack=") || !strings.Contains(got, "/up") {
			t.Fatalf("log %q names no panic value, path or stack", got)
		}
	}
}

// TestChunkedLimitCountsTheFirstChunk889: a complete chunk and the size line of
// the next one in the same call, together over the limit, are refused at the
// size line, before the second chunk's data arrives. (The first chunk is held
// back from chunkBuf while it may turn out to be the whole body.)
func TestChunkedLimitCountsTheFirstChunk889(t *testing.T) {
	for _, first := range []int{100, 20 << 10} {
		st := NewH1State()
		st.MaxRequestBodySize = int64(first) + 500
		h := &rec1029{}
		var out bytes.Buffer
		wire := fmt.Sprintf("%s%x\r\n%s\r\n%x\r\nabc", head1029, first, strings.Repeat("a", first), 501)
		_, err := feed1029(st, h, &out, []byte(wire))
		if err == nil || !strings.HasPrefix(out.String(), "HTTP/1.1 413") {
			t.Fatalf("first chunk %d: err %v answer %q, want 413 at the second size line", first, err, firstLine889(out.String()))
		}
	}
}
