package stream

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// celeris#944, review round 1: the cookie join (and, before it, every header
// field the decoder emits) must be bounded. One HPACK byte can reference a
// 4 KB dynamic-table entry, so a header block of N wire bytes decodes to a
// header list of N x 4 KB; without a bound on the list, joining the cookie
// fields made one HEADERS frame into one cookie string 25,000 times its size
// (net/http answers the same request with 431).

// rstConn944 is a test connection that records the RST_STREAMs the processor
// sends through the connection writer (sendRSTStreamAndMarkClosed).
type rstConn944 struct {
	*testResponseWriter
	mu  sync.Mutex
	rst []rstStreamRecord
}

func (c *rstConn944) WriteRSTStreamPriority(id uint32, code http2.ErrCode) error {
	c.mu.Lock()
	c.rst = append(c.rst, rstStreamRecord{streamID: id, code: code})
	c.mu.Unlock()
	return nil
}

func (c *rstConn944) resets() []rstStreamRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]rstStreamRecord(nil), c.rst...)
}

// bigFieldBlock944 is the HPACK block of a request whose last field is one
// literal of valueLen bytes (name: "cookie" or another), indexed into the
// dynamic table, followed by n one-byte references (0xBE, the first dynamic
// entry) to it. The wire block is about valueLen + n bytes; the header list
// it decodes to is (n+1) x valueLen. enc is the block's HPACK encoder, so a
// later block of the same connection can reference the entry too.
func bigFieldBlock944(enc *hpack.Encoder, hb *bytes.Buffer, name string, valueLen, n int) []byte {
	hb.Reset()
	for _, f := range reqHeaders944() {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	_ = enc.WriteField(hpack.HeaderField{Name: name, Value: strings.Repeat("a", valueLen)})
	hb.Write(bytes.Repeat([]byte{0xBE}, n))
	return append([]byte(nil), hb.Bytes()...)
}

// decodedFields944 decodes block with a plain HPACK decoder (state: dec), and
// returns the number of fields named name: the self-check that 0xBE really
// references the entry.
func decodedFields944(t *testing.T, dec *hpack.Decoder, block []byte, name string) int {
	t.Helper()
	var n int
	dec.SetEmitFunc(func(hf hpack.HeaderField) {
		if hf.Name == name {
			n++
		}
	})
	if _, err := dec.Write(block); err != nil {
		t.Fatal(err)
	}
	return n
}

// feed944 delivers a header block to p on stream id the way path says, and
// returns the error of the last call. path: "raw", "frame", "continuation".
// endStream: the block ends the stream (a request without a body, or the
// trailers).
func feed944(t *testing.T, x *proc944, path string, id uint32, endStream bool, block []byte) error {
	t.Helper()
	err := feedRaw944(t, x.Processor, path, id, endStream, block)
	// A handler the processor sent to the worker pool counts in poolRunning
	// before the call returns: wait for it, so "the handler did not run" is
	// read after the fact, not before the goroutine started.
	for deadline := time.Now().Add(10 * time.Second); x.poolRunning.Load() > 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	return err
}

func feedRaw944(t *testing.T, p *Processor, path string, id uint32, endStream bool, block []byte) error {
	t.Helper()
	p.manager.SetLocalMaxFrameSize(1 << 20) // as NewH2State does (cfg.MaxFrameSize)
	switch path {
	case "raw":
		return p.ProcessRawHeaders(id, endStream, block)
	case "frame":
		var buf bytes.Buffer
		var fl http2.Flags = http2.FlagHeadersEndHeaders
		if endStream {
			fl |= http2.FlagHeadersEndStream
		}
		if err := http2.NewFramer(&buf, nil).WriteRawFrame(http2.FrameHeaders, fl, id, block); err != nil {
			t.Fatal(err)
		}
		r := http2.NewFramer(nil, &buf)
		r.SetMaxReadFrameSize(1 << 20)
		f, err := r.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		return p.ProcessFrame(context.Background(), f)
	case "continuation":
		var buf bytes.Buffer
		w := http2.NewFramer(&buf, nil)
		mid := len(block) / 2
		var fl http2.Flags
		if endStream {
			fl = http2.FlagHeadersEndStream
		}
		if err := w.WriteRawFrame(http2.FrameHeaders, fl, id, block[:mid]); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteRawFrame(http2.FrameContinuation, http2.FlagContinuationEndHeaders, id, block[mid:]); err != nil {
			t.Fatal(err)
		}
		r := http2.NewFramer(nil, &buf)
		r.SetMaxReadFrameSize(1 << 20)
		var err error
		for range 2 {
			f, rerr := r.ReadFrame()
			if rerr != nil {
				t.Fatal(rerr)
			}
			if err = p.ProcessFrame(context.Background(), f); err != nil {
				return err
			}
		}
		return err
	}
	t.Fatalf("unknown path %q", path)
	return nil
}

// newProcessor944 is a Processor whose handler counts its runs and keeps the
// longest cookie value and the number of header fields it saw.
type proc944 struct {
	*Processor
	conn       *rstConn944
	runs       atomic.Int32
	longest    atomic.Int64
	maxFields  atomic.Int64
	lastCookie atomic.Value // string
}

func newProc944() *proc944 {
	x := &proc944{conn: &rstConn944{testResponseWriter: newTestResponseWriter()}}
	x.Processor = NewProcessor(HandlerFunc(func(_ context.Context, s *Stream) error {
		x.runs.Add(1)
		hs := s.GetHeaders()
		if int64(len(hs)) > x.maxFields.Load() {
			x.maxFields.Store(int64(len(hs)))
		}
		for _, h := range hs {
			if h[0] == "cookie" {
				x.lastCookie.Store(h[1])
				if int64(len(h[1])) > x.longest.Load() {
					x.longest.Store(int64(len(h[1])))
				}
			}
		}
		return nil
	}), newTestFrameWriter(), x.conn)
	return x
}

// TestHugeHeaderListIsRefused944 is the review's dynamic-table probe: one
// literal field of about 4 KB (the dynamic table's whole size), then one-byte
// references to it, in a request header block of 16 KB, which any peer may
// send. The header list it decodes to is megabytes (cookie: the joined value
// was 49 MB; any other name: thousands of 4 KB fields). The request is
// refused (RST_STREAM ENHANCE_YOUR_CALM, as net/http answers 431), its
// handler does not run, nothing is allocated in proportion to the list, the
// stream's slot is free, and the connection serves the next request - whose
// block references the entry the refused block added to the HPACK table.
func TestHugeHeaderListIsRefused944(t *testing.T) {
	for _, name := range []string{"cookie", "x-pad"} {
		for _, path := range []string{"raw", "frame", "continuation"} {
			t.Run(name+"/"+path, func(t *testing.T) {
				p := newProc944()
				var hb bytes.Buffer
				enc := hpack.NewEncoder(&hb)
				block := bigFieldBlock944(enc, &hb, name, 4000, 12<<10)
				if got := decodedFields944(t, hpack.NewDecoder(4096, nil), block, name); got != 12<<10+1 {
					t.Fatalf("self-check: the block decodes to %d %q fields, want %d (0xBE must reference the entry)", got, name, 12<<10+1)
				}

				runtime.GC()
				var a, b runtime.MemStats
				runtime.ReadMemStats(&a)
				err := feed944(t, p, path, 1, true, block)
				runtime.ReadMemStats(&b)
				alloc := b.TotalAlloc - a.TotalAlloc

				if err != nil {
					t.Errorf("a refused request is a stream error, not a connection error: %v", err)
				}
				if n := p.runs.Load(); n != 0 {
					t.Errorf("the handler ran %d times for a header list of %d bytes (longest cookie %d, %d fields)",
						n, 12<<10*4000, p.longest.Load(), p.maxFields.Load())
				}
				rst := p.conn.resets()
				if len(rst) != 1 || rst[0].streamID != 1 || rst[0].code != http2.ErrCodeEnhanceYourCalm {
					t.Errorf("RST_STREAMs %+v, want one ENHANCE_YOUR_CALM on stream 1", rst)
				}
				// The list is bounded, so what it costs is: a few hundred KB
				// at most for a 16 KB block (the bound's worth of fields and
				// a joined value), not megabytes.
				if alloc > 1<<20 {
					t.Errorf("a %d-byte header block allocated %d bytes (%.0fx)", len(block), alloc, float64(alloc)/float64(len(block)))
				}
				t.Logf("block %d bytes -> allocated %d bytes (%.1fx), handler runs %d", len(block), alloc, float64(alloc)/float64(len(block)), p.runs.Load())
				if _, ok := p.manager.GetStream(1); ok {
					t.Error("the refused stream is still open")
				}
				if n := p.manager.activeStreams.Load(); n != 0 {
					t.Errorf("%d streams still count against MAX_CONCURRENT_STREAMS", n)
				}

				// The HPACK state is in step: the next block, from the same
				// encoder, references the entry the refused block added.
				hb.Reset()
				for _, f := range reqHeaders944() {
					_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
				}
				_ = enc.WriteField(hpack.HeaderField{Name: name, Value: strings.Repeat("a", 4000)})
				if err := feed944(t, p, path, 3, true, append([]byte(nil), hb.Bytes()...)); err != nil {
					t.Fatalf("the next request on the connection: %v", err)
				}
				if n := p.runs.Load(); n != 1 {
					t.Fatalf("the next request's handler ran %d times, want 1 (emit must be back on, the HPACK table in step)", n)
				}
				if name == "cookie" && p.longest.Load() != 4000 {
					t.Errorf("the next request's cookie is %d bytes, want 4000", p.longest.Load())
				}
			})
		}
	}
}

// TestHugeTrailerListIsRefused944: the bound holds for a trailer block too.
func TestHugeTrailerListIsRefused944(t *testing.T) {
	// Not "continuation": trailers split over CONTINUATION frames are read
	// as a request's header block (handleHeaders returns before it looks at
	// the stream), a separate defect of main, not this test's subject.
	for _, path := range []string{"raw", "frame"} {
		t.Run(path, func(t *testing.T) {
			p := newProc944()
			var hb bytes.Buffer
			enc := hpack.NewEncoder(&hb)
			req := bigFieldBlock944(enc, &hb, "x-small", 10, 0)
			if err := feed944(t, p, "raw", 1, false, req); err != nil { // a request with a body: the stream stays open
				t.Fatal(err)
			}
			hb.Reset()
			_ = enc.WriteField(hpack.HeaderField{Name: "x-trailer", Value: strings.Repeat("t", 4000)})
			hb.Write(bytes.Repeat([]byte{0xBE}, 12<<10))
			err := feed944(t, p, path, 1, true, append([]byte(nil), hb.Bytes()...))
			if err != nil {
				t.Errorf("a refused trailer block is a stream error: %v", err)
			}
			if n := p.runs.Load(); n != 0 {
				t.Errorf("the handler ran %d times after trailers over the bound", n)
			}
			rst := p.conn.resets()
			if len(rst) != 1 || rst[0].streamID != 1 || rst[0].code != http2.ErrCodeEnhanceYourCalm {
				t.Errorf("RST_STREAMs %+v, want one ENHANCE_YOUR_CALM on stream 1", rst)
			}
			if _, ok := p.manager.GetStream(1); ok {
				t.Error("the refused stream is still open")
			}
		})
	}
}

// TestHeaderListBoundIsExact944 pins the accounting: every field costs
// len(name)+len(value)+32 (what net/http charges), cookie fields included,
// whether they are joined or not. A request of exactly the bound is served; a
// byte less of bound and it is refused.
func TestHeaderListBoundIsExact944(t *testing.T) {
	fields := reqHeaders944(
		[2]string{"cookie", "a=1"}, [2]string{"cookie", "b=2"}, [2]string{"x-one", "1"}, [2]string{"cookie", "c=3"})
	var size int
	for _, f := range fields {
		size += len(f[0]) + len(f[1]) + 32
	}
	for _, tc := range []struct {
		name   string
		limit  int
		served bool
	}{{"at the bound", size, true}, {"one byte over", size - 1, false}} {
		for _, path := range []string{"raw", "frame", "continuation"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				p := newProc944()
				p.headerListMax = tc.limit
				if err := feed944(t, p, path, 1, true, encodeHeaders(t, fields)); err != nil {
					t.Fatal(err)
				}
				if got := p.runs.Load() == 1; got != tc.served {
					t.Errorf("limit %d for a list of %d: served=%v, want %v", tc.limit, size, got, tc.served)
				}
				if tc.served {
					if c, _ := p.lastCookie.Load().(string); c != "a=1; b=2; c=3" {
						t.Errorf("cookie %q, want %q", c, "a=1; b=2; c=3")
					}
				}
			})
		}
	}
}

// TestLargeHonestRequestIsServed944: a request well inside the bound but far
// beyond what the old tests used - a 40 KB cookie, split in 4 fields, and a
// 12 KB header - is served, and the join is exact.
func TestLargeHonestRequestIsServed944(t *testing.T) {
	var want []string
	var fields [][2]string
	for i := range 4 {
		v := strings.Repeat(string(rune('a'+i)), 10000)
		want = append(want, v)
		fields = append(fields, [2]string{"cookie", v})
	}
	fields = append(fields, [2]string{"x-big", strings.Repeat("z", 12000)})
	for _, path := range []string{"raw", "frame", "continuation"} {
		t.Run(path, func(t *testing.T) {
			p := newProc944()
			if err := feed944(t, p, path, 1, true, encodeHeaders(t, reqHeaders944(fields...))); err != nil {
				t.Fatal(err)
			}
			if p.runs.Load() != 1 {
				t.Fatalf("the handler ran %d times, want 1; RSTs %+v", p.runs.Load(), p.conn.resets())
			}
			if c, _ := p.lastCookie.Load().(string); c != strings.Join(want, "; ") {
				t.Errorf("cookie of %d bytes differs from the join (%d bytes)", len(c), len(strings.Join(want, "; ")))
			}
		})
	}
}

// TestHeaderBlockFloodIsRefused944: CONTINUATION frames with no END_HEADERS
// piled up without limit (each 16 KB, however many the peer cared to send)
// before the block was decoded. The block cannot decode to more than the
// bound, and no encoding of that list is longer than the list, so a block
// longer than the bound is refused at the CONTINUATION that crosses it, and
// nothing of it is kept.
func TestHeaderBlockFloodIsRefused944(t *testing.T) {
	p := newProc944()
	var buf bytes.Buffer
	w := http2.NewFramer(&buf, nil)
	first := encodeHeaders(t, reqHeaders944())
	if err := w.WriteRawFrame(http2.FrameHeaders, http2.FlagHeadersEndStream, 1, first); err != nil {
		t.Fatal(err)
	}
	const frames = 64
	frag := bytes.Repeat([]byte{0xBE}, 16<<10)
	for range frames {
		if err := w.WriteRawFrame(http2.FrameContinuation, 0, 1, frag); err != nil {
			t.Fatal(err)
		}
	}
	r := http2.NewFramer(nil, &buf)
	var sent int
	var perr error
	for {
		f, err := r.ReadFrame()
		if err != nil {
			break
		}
		sent++
		if perr = p.ProcessFrame(context.Background(), f); perr != nil {
			break
		}
	}
	if perr == nil {
		t.Fatalf("%d CONTINUATION frames (%d KB of block) were all accepted", sent-1, (sent-1)*16)
	}
	if sent > 1+(maxHeaderListSize/(16<<10))+2 {
		t.Errorf("refused only at frame %d (%d KB)", sent, (sent-1)*16)
	}
	if p.IsExpectingContinuation() {
		t.Error("the processor still expects CONTINUATION frames")
	}
	if g := p.conn.testResponseWriter; len(g.goAwaysSent) != 1 || g.goAwaysSent[0].code != http2.ErrCodeEnhanceYourCalm {
		t.Errorf("GOAWAYs %+v, want one ENHANCE_YOUR_CALM", g.goAwaysSent)
	}
	if p.runs.Load() != 0 {
		t.Error("the handler ran")
	}
}

// TestCookieJoinAllocatesInProportionToTheBlock944 replaces a wall-clock bound
// on the join (which no longer separates a pairwise join from a linear one,
// since the bound caps N): joined in one pass the work allocates a constant
// number of times however many fields there are; joined pairwise, once per
// field.
func TestCookieJoinAllocatesInProportionToTheBlock944(t *testing.T) {
	const n = 1500 // 1500 x (6+0+32) = 57 KB: inside the bound
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range reqHeaders944() {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	block := append(hb.Bytes(), bytes.Repeat([]byte{0xA0}, n)...) // 0xA0: the static table's empty cookie
	p := NewProcessor(HandlerFunc(func(context.Context, *Stream) error { return nil }), newTestFrameWriter(), newTestResponseWriter())
	var tr [][2]string
	p.ensureHPACKDecoder()
	run := func() {
		tr = tr[:0]
		p.beginHeaderDecode(&tr, true)
		if _, err := p.hpackDecoder.Write(block); err != nil {
			t.Fatal(err)
		}
		if err := p.hpackDecoder.Close(); err != nil {
			t.Fatal(err)
		}
		if p.endHeaderDecode() {
			t.Fatal("refused")
		}
	}
	run()
	var cookie string
	for _, h := range tr {
		if h[0] == "cookie" {
			cookie = h[1]
		}
	}
	if cookie != strings.Repeat("; ", n-1) {
		t.Fatalf("cookie of %d bytes, want %d", len(cookie), 2*(n-1))
	}
	allocs := testing.AllocsPerRun(20, run)
	if allocs > 8 {
		t.Fatalf("joining %d cookie fields allocates %.0f times per request: the join is not one pass", n, allocs)
	}
	t.Logf("%d one-byte cookie fields: %.0f allocations per request", n, allocs)
}

// TestTinyCookieFieldsAreBoundedAndFast944: 256Ki one-byte cookie fields (0xA0
// each, a 256 KB block) list at 9.9 MB for the bound's purposes. They are
// refused, in milliseconds, whatever the join cost.
func TestTinyCookieFieldsAreBoundedAndFast944(t *testing.T) {
	const n = 256 << 10
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range reqHeaders944() {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	block := append(hb.Bytes(), bytes.Repeat([]byte{0xA0}, n)...)
	p := newProc944()
	start := time.Now()
	err := p.ProcessRawHeaders(1, true, block)
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if p.runs.Load() != 0 {
		t.Errorf("a list of %d cookie fields reached the handler", n)
	}
	if rst := p.conn.resets(); len(rst) != 1 || rst[0].code != http2.ErrCodeEnhanceYourCalm {
		t.Errorf("RST_STREAMs %+v, want one ENHANCE_YOUR_CALM", rst)
	}
	if took > 2*time.Second {
		t.Errorf("refusing %d cookie fields took %v", n, took)
	}
	t.Logf("%d one-byte cookie fields refused in %v", n, took)
}
