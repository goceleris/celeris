package stream

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// celeris#889 items 17, 19, 20, 25, 26 and 30: the bounds on what a peer can
// make the HTTP/2 processor do on one connection without ever paying for a
// stream. The tests here are written against the processor's behaviour, not
// its constants, so the ones that pin a defect compile on main and fail
// there; the constants are pinned by refusal_889_const_test.go.

// sinks889 returns what the peer has received from p so far: the GOAWAYs and
// the RST_STREAMs, whichever sink they went through.
func sinks889(p *Processor) (goaways []goAwayRecord, rst []rstStreamRecord) {
	fw := p.writer.(*testFrameWriter)
	conn := p.connWriter.(*rstConn944)
	conn.testResponseWriter.mu.Lock()
	goaways = append(goaways, conn.goAwaysSent...)
	conn.testResponseWriter.mu.Unlock()
	fw.mu.Lock()
	goaways = append(goaways, fw.goAwaysSent...)
	rst = append(rst, fw.rstStreamsSent...)
	fw.mu.Unlock()
	rst = append(rst, conn.resets()...)
	return goaways, rst
}

// connCredit889 is the connection-level WINDOW_UPDATE credit p has sent.
func connCredit889(p *Processor) uint32 {
	fw := p.writer.(*testFrameWriter)
	fw.mu.Lock()
	defer fw.mu.Unlock()
	return fw.windowUpdates[0]
}

// hugeListBlock889 is the review's dynamic-table block (a 4000-byte literal
// then 12 KB of one-byte references to it): a request header block of about
// 16 KB that decodes to a header list of megabytes. A block built by a fresh
// encoder can be sent again and again on one connection: it names its own
// entry as the newest one, whatever the decoder's table held before.
func hugeListBlock889() []byte {
	var hb bytes.Buffer
	return bigFieldBlock944(hpack.NewEncoder(&hb), &hb, "x-pad", 4000, 12<<10)
}

// staticBlock889 is a request of three static-table fields: a refused one of
// these allocates nothing in the decoder.
var staticBlock889 = []byte{0x82, 0x86, 0x84}

// openStream889 opens stream 1 with a request whose body has not come (no
// END_STREAM), so the stream holds a MAX_CONCURRENT_STREAMS slot.
func openStream889(t *testing.T, p *Processor, e *enc981) {
	t.Helper()
	if err := p.ProcessRawHeaders(1, false, e.req()); err != nil {
		t.Fatal(err)
	}
}

// A peer that sends requests above SETTINGS_MAX_CONCURRENT_STREAMS at wire
// speed gets REFUSED_STREAM for each, and, past a burst no honest client
// makes, a GOAWAY(ENHANCE_YOUR_CALM). The burst has to be generous: a client
// that has not read the server's SETTINGS yet may assume no limit at all
// (RFC 9113 section 6.5.2: "Initially, there is no limit"), and every request
// above the limit is refused.
func TestStreamLimitRefusalsEscalate889(t *testing.T) {
	const (
		honestBurst = 900 // 1000 streams opened at once, before SETTINGS, against a limit of 100
		attempts    = 40000
	)
	p := newProc944()
	p.manager.SetMaxConcurrentStreams(1)
	openStream889(t, p.Processor, newEnc981())

	id := uint32(3)
	var refusals int
	var err error
	for ; refusals < attempts; refusals++ {
		if err = p.ProcessRawHeaders(id, true, staticBlock889); err != nil {
			break
		}
		id += 2
	}
	goaways, rst := sinks889(p.Processor)
	if err == nil {
		t.Fatalf("%d refused streams on one connection ended nothing: no GOAWAY (%d RST_STREAMs sent)", refusals, len(rst))
	}
	if refusals < honestBurst {
		t.Errorf("the connection was ended after %d refusals, a client that bursts %d streams before it reads SETTINGS is honest", refusals, honestBurst)
	}
	if len(goaways) != 1 || goaways[0].code != http2.ErrCodeEnhanceYourCalm {
		t.Fatalf("GOAWAYs = %+v, want one ENHANCE_YOUR_CALM", goaways)
	}
	// Stream 1 is the only stream the server opened: the GOAWAY says so.
	if goaways[0].lastStreamID != 1 {
		t.Errorf("GOAWAY names last stream %d, want 1 (the one stream the server opened)", goaways[0].lastStreamID)
	}
}

// The same for a header list over the bound: a refusal costs the decode of
// the whole block (a 16 KB block allocated 270 KB on #979's head), so the
// burst allowed is smaller, and past it the connection ends, on every path a
// block can take to the processor.
func TestHeaderListRefusalsEscalate889(t *testing.T) {
	const (
		honestBurst = 20
		attempts    = 1500
	)
	for _, path := range []string{"raw", "frame", "continuation"} {
		t.Run(path, func(t *testing.T) {
			p := newProc944()
			p.manager.SetLocalMaxFrameSize(1 << 20)
			block := hugeListBlock889()
			id := uint32(1)
			var refusals int
			var err error
			for ; refusals < attempts; refusals++ {
				if err = feed981(t, p.Processor, path, hdr981(id, true, block)); err != nil {
					break
				}
				p.FlushInlineCleanup()
				id += 2
			}
			goaways, rst := sinks889(p.Processor)
			if err == nil {
				t.Fatalf("%d header lists over the bound on one connection ended nothing: no GOAWAY (%d RST_STREAMs sent)", refusals, len(rst))
			}
			if refusals < honestBurst {
				t.Errorf("the connection was ended after %d refusals, want at least %d", refusals, honestBurst)
			}
			if len(goaways) != 1 || goaways[0].code != http2.ErrCodeEnhanceYourCalm {
				t.Fatalf("GOAWAYs = %+v, want one ENHANCE_YOUR_CALM", goaways)
			}
			if p.runs.Load() != 0 {
				t.Errorf("the handler ran %d times for refused requests", p.runs.Load())
			}
			if goaways[0].lastStreamID > id {
				t.Errorf("GOAWAY names last stream %d, past the last stream sent (%d)", goaways[0].lastStreamID, id)
			}
		})
	}
}

// A few refusals are not an attack: well under the burst, nothing ends the
// connection, and the next request is served.
func TestRefusalsUnderTheBurstEndNothing889(t *testing.T) {
	p := newProc944()
	p.manager.SetMaxConcurrentStreams(1)
	e := newEnc981()
	openStream889(t, p.Processor, e)
	id := uint32(3)
	for range 250 { // more than the 200 RST_STREAMs a second a peer may send
		if err := p.ProcessRawHeaders(id, true, e.req()); err != nil {
			t.Fatalf("refusal %d: %v", (id-1)/2, err)
		}
		id += 2
	}
	big := hugeListBlock889()
	for range 5 {
		if err := p.ProcessRawHeaders(id, true, big); err != nil {
			t.Fatalf("header list over the bound: %v", err)
		}
		id += 2
	}
	if goaways, _ := sinks889(p.Processor); len(goaways) != 0 {
		t.Fatalf("GOAWAYs = %+v, want none", goaways)
	}
}

// RFC 9113 section 5.1 (closed): "An endpoint that sends a RST_STREAM frame
// on a stream that is in the open or half-closed (remote) state ... MUST be
// prepared to receive any frames that were sent before it processed the
// RST_STREAM". A request refused for MAX_CONCURRENT_STREAMS or for its header
// list whose body is already on the wire (a POST sent without waiting) must
// not end the connection, and its DATA must be counted against the
// connection's receive window like any other, or the window leaks.
func TestDataAfterRefusalIsIgnored889(t *testing.T) {
	// The payload of a padded DATA frame (the Pad Length byte and the
	// padding included) is what flow control counts (RFC 9113 section 6.1,
	// 6.9.1). 8000+1+200 is over the 8192 bytes at which credit is sent.
	data := bytes.Repeat([]byte("d"), 8000)
	const pad = 200
	const flowControlled = 8000 + 1 + pad

	refusals := []struct {
		name  string
		limit uint32
		block func(e *enc981) []byte
	}{
		{"stream-limit", 1, func(e *enc981) []byte { return e.req() }},
		{"header-list", 0, func(*enc981) []byte { return hugeListBlock889() }},
	}
	for _, rc := range refusals {
		for _, path := range []string{"raw", "frame", "continuation"} {
			t.Run(rc.name+"/"+path, func(t *testing.T) {
				p := newProc944()
				p.manager.SetLocalMaxFrameSize(1 << 20)
				if rc.limit > 0 {
					p.manager.SetMaxConcurrentStreams(rc.limit)
				}
				e := newEnc981()
				id := uint32(1)
				if rc.limit > 0 {
					openStream889(t, p.Processor, e) // holds the only slot
					id = 3
				}
				if err := feed981(t, p.Processor, path, hdr981(id, false, rc.block(e))); err != nil {
					t.Fatal(err)
				}
				if _, rst := sinks889(p.Processor); len(rst) != 1 || rst[0].streamID != id {
					t.Fatalf("RST_STREAMs = %+v, want one, for stream %d", rst, id)
				}
				before := connCredit889(p.Processor)

				err := processFrames981(t, p.Processor, func(w *http2.Framer) {
					if werr := w.WriteDataPadded(id, true, data, make([]byte, pad)); werr != nil {
						t.Fatal(werr)
					}
				})
				if err != nil {
					t.Fatalf("DATA on stream %d, reset by the server: %v", id, err)
				}
				if goaways, _ := sinks889(p.Processor); len(goaways) != 0 {
					t.Fatalf("GOAWAYs = %+v, want none", goaways)
				}
				if got := connCredit889(p.Processor) - before; got != flowControlled {
					t.Errorf("connection WINDOW_UPDATE credit for the ignored DATA = %d, want %d (the whole payload, padding included)", got, flowControlled)
				}
				// The connection goes on: the next request is served.
				if rc.limit > 0 {
					p.manager.DeleteStream(1) // the slot is free again
				}
				if err := feed981(t, p.Processor, path, hdr981(id+2, true, e.req())); err != nil {
					t.Fatalf("the next request: %v", err)
				}
				p.FlushInlineCleanup()
				if p.runs.Load() != 1 {
					t.Errorf("handler runs = %d, want 1", p.runs.Load())
				}
			})
		}
	}
}

// DATA the processor must still end the connection for: on a stream that was
// never opened (an identifier above the last the peer used), on an even
// identifier, and on stream 0.
func TestDataOnIdleStreamStillEndsTheConnection889(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   uint32
	}{
		{"above-the-last-stream", 9},
		{"even", 2},
		{"stream-0", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProc944()
			e := newEnc981()
			if err := p.ProcessRawHeaders(1, true, e.req()); err != nil { // stream 1 served and closed
				t.Fatal(err)
			}
			p.FlushInlineCleanup()
			f := &http2.DataFrame{FrameHeader: http2.FrameHeader{Type: http2.FrameData, StreamID: tc.id, Length: 4}}
			err := p.handleData(context.Background(), f)
			if err == nil {
				t.Fatal("DATA on an idle stream was ignored, want a connection error")
			}
			goaways, _ := sinks889(p.Processor)
			if len(goaways) != 1 || goaways[0].code != http2.ErrCodeProtocol {
				t.Fatalf("GOAWAYs = %+v, want one PROTOCOL_ERROR", goaways)
			}
		})
	}
}

// DATA on a stream that is closed for any reason but the server's own reset is
// still a connection error: the exchange ended, or the peer reset the stream
// (RFC 9113 section 5.1, closed; h2spec 5.1 and 6.1 pin it). Only the frames
// of a stream the server reset are ignored.
func TestDataOnStreamsClosedOtherwiseStillEndsTheConnection889(t *testing.T) {
	data := func(id uint32) *http2.DataFrame {
		return &http2.DataFrame{FrameHeader: http2.FrameHeader{Type: http2.FrameData, StreamID: id, Length: 4}}
	}
	t.Run("the-exchange-ended", func(t *testing.T) {
		p := newProc944()
		if err := p.ProcessRawHeaders(1, true, newEnc981().req()); err != nil { // served and closed
			t.Fatal(err)
		}
		p.FlushInlineCleanup()
		if err := p.handleData(context.Background(), data(1)); err == nil {
			t.Fatal("DATA on a stream whose exchange ended was ignored, want a connection error")
		}
		if goaways, _ := sinks889(p.Processor); len(goaways) != 1 || goaways[0].code != http2.ErrCodeProtocol {
			t.Fatalf("GOAWAYs = %+v, want one PROTOCOL_ERROR", goaways)
		}
	})
	t.Run("reset-by-the-peer", func(t *testing.T) {
		p := newProc944()
		if err := p.ProcessRawHeaders(1, false, newEnc981().req()); err != nil {
			t.Fatal(err)
		}
		if err := processFrames981(t, p.Processor, func(w *http2.Framer) {
			if werr := w.WriteRSTStream(1, http2.ErrCodeCancel); werr != nil {
				t.Fatal(werr)
			}
		}); err != nil {
			t.Fatal(err)
		}
		if err := p.handleData(context.Background(), data(1)); err == nil {
			t.Fatal("DATA after the peer's RST_STREAM was ignored, want a connection error")
		}
		if goaways, _ := sinks889(p.Processor); len(goaways) != 1 {
			t.Fatalf("GOAWAYs = %+v, want one", goaways)
		}
	})
}

// A HEADERS frame without END_HEADERS starts a header block the server keeps
// until it ends: its first fragment is held as the later ones are, so it is
// capped as they are (headerListMax x headerBlockExpansion), not by the
// frame size alone (1 MiB by default).
func TestFirstFragmentIsCapped889(t *testing.T) {
	capBytes := maxHeaderListSize * headerBlockExpansion
	for _, tc := range []struct {
		name string
		size int
		ok   bool
	}{
		{"at-the-cap", capBytes, true},
		{"one-over", capBytes + 1, false},
		{"frame-size", 1 << 20, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProc944()
			p.manager.SetLocalMaxFrameSize(1 << 20)
			err := processFrames981(t, p.Processor, func(w *http2.Framer) {
				if werr := w.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: make([]byte, tc.size), EndHeaders: false}); werr != nil {
					t.Fatal(werr)
				}
			})
			goaways, _ := sinks889(p.Processor)
			if tc.ok {
				if err != nil || len(goaways) != 0 {
					t.Fatalf("a first fragment of the cap's size: err %v, GOAWAYs %+v, want neither", err, goaways)
				}
				if !p.IsExpectingContinuation() {
					t.Error("the block is not being assembled")
				}
				return
			}
			if err == nil {
				t.Fatalf("a first fragment of %d bytes was accepted, the cap is %d", tc.size, capBytes)
			}
			if len(goaways) != 1 || goaways[0].code != http2.ErrCodeEnhanceYourCalm {
				t.Fatalf("GOAWAYs = %+v, want one ENHANCE_YOUR_CALM", goaways)
			}
			if p.IsExpectingContinuation() {
				t.Error("the processor still holds the over-long block")
			}
		})
	}
}

// CONTINUATION frames are counted, not only their bytes: a frame with an empty
// fragment costs the server a frame read and a lock, and adds nothing to the
// byte count that ends a block.
func TestContinuationFramesAreCounted889(t *testing.T) {
	// A block of this many pieces is honest: the cap on the block is 256 KiB,
	// and a peer whose frames are 16 KiB sends 16.
	const honestPieces = 16
	const attempts = 20000

	t.Run("empty-flood", func(t *testing.T) {
		p := newProc944()
		// One reader framer for the whole block: it checks that a CONTINUATION
		// follows a HEADERS, which a framer made per frame cannot.
		var buf bytes.Buffer
		w := http2.NewFramer(&buf, nil)
		r := http2.NewFramer(nil, &buf)
		r.SetMaxReadFrameSize(1 << 20)
		next := func() error {
			f, err := r.ReadFrame()
			if err != nil {
				t.Fatal(err)
			}
			return p.ProcessFrame(context.Background(), f)
		}
		if err := w.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: staticBlock889[:1], EndHeaders: false}); err != nil {
			t.Fatal(err)
		}
		if err := next(); err != nil {
			t.Fatal(err)
		}
		var err error
		sent := 0
		for ; sent < attempts && err == nil; sent++ {
			if werr := w.WriteContinuation(1, false, nil); werr != nil {
				t.Fatal(werr)
			}
			err = next()
		}
		goaways, _ := sinks889(p.Processor)
		if err == nil {
			t.Fatalf("%d empty CONTINUATION frames on one header block ended nothing", sent)
		}
		if sent <= honestPieces {
			t.Errorf("the block was ended after %d CONTINUATION frames, an honest block has %d", sent, honestPieces)
		}
		if len(goaways) != 1 || goaways[0].code != http2.ErrCodeEnhanceYourCalm {
			t.Fatalf("GOAWAYs = %+v, want one ENHANCE_YOUR_CALM", goaways)
		}
		if p.IsExpectingContinuation() {
			t.Error("the processor still holds the block")
		}
	})

	t.Run("honest-split", func(t *testing.T) {
		p := newProc944()
		e := newEnc981()
		block := e.req([2]string{"x-pad", strings.Repeat("a", 200)})
		// honestPieces pieces of one block: a HEADERS and honestPieces-1 CONTINUATIONs.
		piece := len(block) / honestPieces
		if piece == 0 {
			t.Fatalf("self-check: the block of %d bytes cannot be split in %d", len(block), honestPieces)
		}
		perr := processFrames981(t, p.Processor, func(w *http2.Framer) {
			if werr := w.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: block[:piece], EndStream: true}); werr != nil {
				t.Fatal(werr)
			}
			rest := block[piece:]
			for i := 1; i < honestPieces; i++ {
				end := i == honestPieces-1
				frag := rest[:piece]
				if end {
					frag = rest
				}
				if werr := w.WriteContinuation(1, end, frag); werr != nil {
					t.Fatal(werr)
				}
				rest = rest[len(frag):]
			}
		})
		if perr != nil {
			t.Fatalf("a header block split in %d pieces: %v", honestPieces, perr)
		}
		p.FlushInlineCleanup()
		if p.runs.Load() != 1 {
			t.Errorf("handler runs = %d, want 1", p.runs.Load())
		}
	})
}

// PRIORITY on idle streams leaves a placeholder Stream in the manager (about
// 1.2 KB for a 14-byte frame). The lastClient+2048 window that was meant to
// bound them slides with every HEADERS, so a peer that interleaves PRIORITY
// on the next-but-one stream with a HEADERS frame on the current one leaves
// one placeholder behind per pair, for the life of the connection. The
// placeholders are bounded per connection, and the PRIORITY frames past the
// bound are ignored (priority signals are advisory, RFC 9113 section 5.3.2),
// not answered with a connection error.
func TestPriorityPlaceholdersAreBounded889(t *testing.T) {
	const pairs = 5000
	const boundedBy = 256 // the bound is a few dozen; the unbounded count is `pairs`
	p := newProc944()
	e := newEnc981()
	req := e.req()
	prio := &http2.PriorityParam{StreamDep: 0, Weight: 15}
	for i := range pairs {
		id := uint32(4*i + 1)
		if err := processFrames981(t, p.Processor, func(w *http2.Framer) {
			if werr := w.WritePriority(id+2, *prio); werr != nil {
				t.Fatal(werr)
			}
		}); err != nil {
			t.Fatalf("PRIORITY on stream %d: %v", id+2, err)
		}
		if err := p.ProcessRawHeaders(id, true, req); err != nil {
			t.Fatalf("HEADERS on stream %d: %v", id, err)
		}
		p.FlushInlineCleanup()
	}
	if got := p.runs.Load(); got != pairs {
		t.Errorf("handler runs = %d, want %d: a PRIORITY frame past the bound must not disturb the requests", got, pairs)
	}
	if n := p.manager.StreamCount(); n > boundedBy {
		t.Errorf("%d streams held after %d PRIORITY+HEADERS pairs, want at most %d", n, pairs, boundedBy)
	}
	if goaways, _ := sinks889(p.Processor); len(goaways) != 0 {
		t.Errorf("GOAWAYs = %+v, want none", goaways)
	}
}

// A PRIORITY frame on an identifier at or below the last client stream that
// the manager does not hold is a closed stream's: it creates nothing, in the
// manager or in the priority tree.
func TestPriorityOnClosedStreamIsIgnored889(t *testing.T) {
	p := newProc944()
	e := newEnc981()
	for _, id := range []uint32{1, 3} {
		if err := p.ProcessRawHeaders(id, true, e.req()); err != nil {
			t.Fatal(err)
		}
		p.FlushInlineCleanup()
	}
	if n := p.manager.StreamCount(); n != 0 {
		t.Fatalf("self-check: %d streams held, want 0", n)
	}
	err := processFrames981(t, p.Processor, func(w *http2.Framer) {
		if werr := w.WritePriority(3, http2.PriorityParam{StreamDep: 1, Weight: 99}); werr != nil {
			t.Fatal(werr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := p.manager.StreamCount(); n != 0 {
		t.Errorf("PRIORITY on closed stream 3 left %d streams in the manager", n)
	}
	if _, ok := p.manager.priorityTree.GetPriority(3); ok {
		t.Error("PRIORITY on closed stream 3 left an entry in the priority tree")
	}
}

// A PRIORITY placeholder is no stream the server opened: the GOAWAY of a
// connection error names the highest stream the server opened, not the
// placeholder's identifier. (The comments of refuseStream and
// completeHeaderBlock say "the last stream the server opened".)
func TestPlaceholderIsNotTheLastStream889(t *testing.T) {
	p := newProc944()
	p.manager.SetMaxConcurrentStreams(1)
	openStream889(t, p.Processor, newEnc981()) // stream 1 is open, and the only one
	if err := processFrames981(t, p.Processor, func(w *http2.Framer) {
		if werr := w.WritePriority(2001, http2.PriorityParam{StreamDep: 0, Weight: 15}); werr != nil {
			t.Fatal(werr)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if got := p.manager.GetLastStreamID(); got != 1 {
		t.Errorf("GetLastStreamID after a PRIORITY on idle stream 2001 = %d, want 1", got)
	}
	// Stream 3 is refused (the limit is 1), and its block does not decode.
	err := p.ProcessRawHeaders(3, true, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f, 0x80})
	if err == nil {
		t.Fatal("a header block that does not decode was accepted")
	}
	goaways, _ := sinks889(p.Processor)
	if len(goaways) != 1 || goaways[0].lastStreamID != 1 || goaways[0].code != http2.ErrCodeCompression {
		t.Errorf("GOAWAYs = %+v, want one COMPRESSION_ERROR naming stream 1", goaways)
	}
}

// The GOAWAY for a stream identifier out of order names the last stream the
// server opened, as the other sites do, not the last client stream, which a
// refused stream moves forward (celeris#889 item 30).
func TestIDOrderGoAwayNamesLastOpenedStream889(t *testing.T) {
	for _, path := range []string{"raw", "frame", "continuation"} {
		t.Run(path, func(t *testing.T) {
			p := newProc944()
			p.manager.SetLocalMaxFrameSize(1 << 20)
			p.manager.SetMaxConcurrentStreams(1)
			e := newEnc981()
			if err := feed981(t, p.Processor, path, hdr981(1, false, e.req())); err != nil { // opened
				t.Fatal(err)
			}
			if err := feed981(t, p.Processor, path, hdr981(3, true, e.req())); err != nil { // refused
				t.Fatal(err)
			}
			err := feed981(t, p.Processor, path, hdr981(3, true, e.req())) // reused
			if err == nil {
				t.Fatal("HEADERS on a used identifier was accepted")
			}
			goaways, _ := sinks889(p.Processor)
			if len(goaways) != 1 || goaways[0].code != http2.ErrCodeProtocol {
				t.Fatalf("GOAWAYs = %+v, want one PROTOCOL_ERROR", goaways)
			}
			if goaways[0].lastStreamID != 1 {
				t.Errorf("GOAWAY names last stream %d, want 1: stream 3 was refused, not processed", goaways[0].lastStreamID)
			}
		})
	}
}
