package conn

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris/protocol/h2/frame"
	"github.com/goceleris/celeris/protocol/h2/stream"
)

// captureHandler records whether HandleStream was called and the stream seen.
type captureHandler struct {
	mu     sync.Mutex
	called bool
	method string
	path   string
	body   string
}

func (c *captureHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.called = true
	for _, h := range s.GetHeaders() {
		switch h[0] {
		case ":method":
			c.method = h[1]
		case ":path":
			c.path = h[1]
		}
	}
	if d := s.GetData(); len(d) > 0 {
		c.body = string(d)
	}
	// Respond so the lifecycle completes.
	if s.ResponseWriter != nil {
		return s.ResponseWriter.WriteResponse(s, 200, [][2]string{
			{"content-type", "text/plain"},
		}, []byte("ok"))
	}
	return nil
}

func TestNewH2StateFromUpgrade_Basic(t *testing.T) {
	// Settings: MAX_CONCURRENT_STREAMS=50, INITIAL_WINDOW_SIZE=131072.
	settings := []byte{
		0, 3, 0, 0, 0, 50,
		0, 4, 0, 2, 0, 0,
	}
	info := &UpgradeInfo{
		Settings: settings,
		Method:   "GET",
		URI:      "/hello",
		Headers: [][2]string{
			{"host", "example.com"},
			{"user-agent", "upgrade-test"},
		},
		Body: nil,
	}

	h := &captureHandler{}
	var writes []byte
	var mu sync.Mutex
	write := func(b []byte) {
		mu.Lock()
		writes = append(writes, b...)
		mu.Unlock()
	}
	state, err := NewH2StateFromUpgrade(h, H2Config{}, write, -1, info)
	if err != nil {
		t.Fatalf("NewH2StateFromUpgrade: %v", err)
	}
	if !state.serverPrefaceSent {
		t.Fatal("serverPrefaceSent not set")
	}
	if !h.called {
		t.Fatal("handler not invoked on stream 1")
	}
	if h.method != "GET" || h.path != "/hello" {
		t.Fatalf("method=%q path=%q, want GET /hello", h.method, h.path)
	}
	// Server preface + response must have been written.
	mu.Lock()
	got := writes
	mu.Unlock()
	if len(got) == 0 {
		t.Fatal("no bytes written (expected SETTINGS + response)")
	}
}

func TestProcessH2_PartialPreface(t *testing.T) {
	settings := []byte{0, 3, 0, 0, 0, 10}
	info := &UpgradeInfo{
		Settings: settings,
		Method:   "GET",
		URI:      "/",
		Headers:  [][2]string{{"host", "x"}},
	}
	h := &captureHandler{}
	var mu sync.Mutex
	var writes []byte
	write := func(b []byte) {
		mu.Lock()
		writes = append(writes, b...)
		mu.Unlock()
	}
	state, err := NewH2StateFromUpgrade(h, H2Config{}, write, -1, info)
	if err != nil {
		t.Fatalf("NewH2StateFromUpgrade: %v", err)
	}

	// Feed only the first half of the H2 client preface.
	half := []byte(frame.ClientPreface)[:10]
	if err := ProcessH2(context.Background(), half, state, h, write, H2Config{}); err != nil {
		t.Fatalf("ProcessH2 partial preface: %v", err)
	}
	if state.initialized {
		t.Fatal("should not be initialized on partial preface")
	}
	if !bytes.Equal(state.prefaceBuf, half) {
		t.Fatalf("prefaceBuf = %q, want %q", state.prefaceBuf, half)
	}

	// Deliver the rest.
	rest := []byte(frame.ClientPreface)[10:]
	if err := ProcessH2(context.Background(), rest, state, h, write, H2Config{}); err != nil {
		t.Fatalf("ProcessH2 rest: %v", err)
	}
	if !state.initialized {
		t.Fatal("should be initialized after full preface")
	}
	if len(state.prefaceBuf) != 0 {
		t.Fatalf("prefaceBuf not cleared: %q", state.prefaceBuf)
	}
}

func TestProcessH2_WrongPrefaceRejected(t *testing.T) {
	// No upgrade — plain H2 connection.
	h := &captureHandler{}
	write := func([]byte) {}
	state := NewH2State(h, H2Config{}, write, -1)
	garbage := []byte("GET / HTTP/1.1\r\n\r\n")
	err := ProcessH2(context.Background(), garbage, state, h, write, H2Config{})
	if err == nil {
		t.Fatal("expected error on invalid preface")
	}
}

func TestInjectStreamHeaders_WithBody(t *testing.T) {
	info := &UpgradeInfo{
		Settings: nil,
		Method:   "POST",
		URI:      "/submit",
		Headers: [][2]string{
			{"host", "example.com"},
			{"content-type", "text/plain"},
		},
		Body: []byte("hello world"),
	}
	h := &captureHandler{}
	write := func([]byte) {}
	state, err := NewH2StateFromUpgrade(h, H2Config{}, write, -1, info)
	_ = state
	if err != nil {
		t.Fatalf("NewH2StateFromUpgrade: %v", err)
	}
	if !h.called {
		t.Fatal("handler not invoked")
	}
	if h.method != "POST" || h.path != "/submit" || h.body != "hello world" {
		t.Fatalf("method=%q path=%q body=%q", h.method, h.path, h.body)
	}
}

// respondHandler answers every stream with a fixed 200 response so the inline
// handler path runs to completion (END_STREAM sent) and reaches the stream-slot
// reclamation logic under test.
type respondHandler struct{ body []byte }

func (h respondHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}}, h.body)
}

func encodeH2Headers(t *testing.T, headers [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := hpack.NewEncoder(&buf)
	for _, h := range headers {
		if err := enc.WriteField(hpack.HeaderField{Name: h[0], Value: h[1]}); err != nil {
			t.Fatalf("hpack encode: %v", err)
		}
	}
	return buf.Bytes()
}

// TestProcessH2_PipelinedCompletedStreamsNotRefused is a regression test for the
// REFUSED_STREAM storm (probatorium grid get-json-64k-h2: ~50M stream errors).
// When a single recv buffer carries more pipelined, already-complete GET
// requests than SETTINGS_MAX_CONCURRENT_STREAMS, the server must NOT refuse the
// excess: inline handlers run sequentially, so each stream completes
// (END_STREAM) before the next opens — at most one stream is ever genuinely
// open. A completed stream must free its concurrency slot immediately even
// though its map removal is deferred to FlushInlineCleanup (hasMoreFrames=true
// for the whole batch). Before the fix, streams past the limit got
// RST_STREAM(REFUSED_STREAM); after, all are served.
func TestProcessH2_PipelinedCompletedStreamsNotRefused(t *testing.T) {
	const (
		maxStreams = 8
		nStreams   = 20 // > maxStreams: pre-fix refused streams 9..20
	)
	h := respondHandler{body: []byte("ok")}
	var mu sync.Mutex
	var writes []byte
	write := func(b []byte) {
		mu.Lock()
		writes = append(writes, b...)
		mu.Unlock()
	}
	cfg := H2Config{MaxConcurrentStreams: maxStreams}
	state := NewH2State(h, cfg, write, -1)

	// One buffer: client preface + empty client SETTINGS + nStreams complete
	// GET HEADERS frames (odd stream IDs). Delivering them in a single
	// ProcessH2 call sets hasMoreFrames=true for the whole batch — the
	// precondition for the deferred-cleanup bug.
	hdr := encodeH2Headers(t, [][2]string{
		{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {":authority", "x"},
	})
	var in bytes.Buffer
	in.WriteString(frame.ClientPreface)
	fr := http2.NewFramer(&in, nil)
	if err := fr.WriteSettings(); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	for i := 0; i < nStreams; i++ {
		sid := uint32(2*i + 1)
		if err := fr.WriteRawFrame(http2.FrameHeaders,
			http2.FlagHeadersEndStream|http2.FlagHeadersEndHeaders, sid, hdr); err != nil {
			t.Fatalf("WriteHeaders sid=%d: %v", sid, err)
		}
	}
	if err := ProcessH2(context.Background(), in.Bytes(), state, h, write, cfg); err != nil {
		t.Fatalf("ProcessH2: %v", err)
	}

	// Parse the server output: no stream may be REFUSED, and every request
	// must be answered.
	mu.Lock()
	out := append([]byte(nil), writes...)
	mu.Unlock()
	refused, served := 0, 0
	rf := http2.NewFramer(nil, bytes.NewReader(out))
	rf.SetMaxReadFrameSize(1 << 20)
	for {
		f, err := rf.ReadFrame()
		if err != nil {
			break
		}
		switch v := f.(type) {
		case *http2.RSTStreamFrame:
			if v.ErrCode == http2.ErrCodeRefusedStream {
				refused++
			}
		case *http2.HeadersFrame:
			served++
		}
	}
	if refused != 0 {
		t.Fatalf("got %d RST_STREAM(REFUSED_STREAM), want 0 — completed inline streams must free their MAX_CONCURRENT_STREAMS slot", refused)
	}
	if served != nStreams {
		t.Fatalf("server answered %d/%d pipelined requests", served, nStreams)
	}
}

// TestProcessH2_StalledStreamsReleaseSlot is a regression test for the
// MAX_CONCURRENT_STREAMS slot LEAK under sustained large responses that
// backpressure on flow control (probatorium grid get-json-64k-h2:
// ~410M stream errors / sustained REFUSED storm).
//
// With a small per-stream INITIAL_WINDOW_SIZE, every response STALLS: the
// handler buffers the un-sendable tail of the body and the inline path keeps
// the stream alive (state half-closed-remote, counting toward activeStreams)
// until a WINDOW_UPDATE opens the window. If the WINDOW_UPDATE flush path
// fails to transition the now-fully-sent stream to closed AND decrement the
// active count exactly once, the slot LEAKS. Over many sequential
// stalled-then-released streams the leak accumulates until activeStreams
// pins at the limit and every new HEADERS is RST'd with REFUSED_STREAM.
//
// Each stream here is delivered + released in its OWN ProcessH2 call (one
// recv buffer per stream), mirroring the live load where streams arrive
// over wall-time rather than all in one batch.
func TestProcessH2_StalledStreamsReleaseSlot(t *testing.T) {
	const (
		maxStreams = 8
		nStreams   = 256 // >> maxStreams: a leaking slot pins the limit fast
		window     = 16  // tiny window: body tail always stalls on flow control
	)
	// Body larger than the window so the response always buffers a tail and
	// stalls — the precondition for the leak.
	h := respondHandler{body: bytes.Repeat([]byte("x"), 64)}
	var mu sync.Mutex
	var writes []byte
	write := func(b []byte) {
		mu.Lock()
		writes = append(writes, b...)
		mu.Unlock()
	}
	cfg := H2Config{MaxConcurrentStreams: maxStreams}
	state := NewH2State(h, cfg, write, -1)

	hdr := encodeH2Headers(t, [][2]string{
		{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {":authority", "x"},
	})

	mgr := state.processor.GetManager()

	// Preface + client SETTINGS in the first buffer. The client advertises a
	// tiny SETTINGS_INITIAL_WINDOW_SIZE so OUR per-stream SEND window starts
	// small — every response then stalls on flow control, exactly as a 64 KB
	// body does against the RFC-default 65535 window on the live grid.
	var preface bytes.Buffer
	preface.WriteString(frame.ClientPreface)
	pf := http2.NewFramer(&preface, nil)
	if err := pf.WriteSettings(http2.Setting{
		ID: http2.SettingInitialWindowSize, Val: window,
	}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	if err := ProcessH2(context.Background(), preface.Bytes(), state, h, write, cfg); err != nil {
		t.Fatalf("ProcessH2 preface: %v", err)
	}

	for i := 0; i < nStreams; i++ {
		sid := uint32(2*i + 1)

		// 1) HEADERS (complete GET). Response stalls: body tail buffered,
		//    stream stays active pending WINDOW_UPDATE.
		var hb bytes.Buffer
		hf := http2.NewFramer(&hb, nil)
		if err := hf.WriteRawFrame(http2.FrameHeaders,
			http2.FlagHeadersEndStream|http2.FlagHeadersEndHeaders, sid, hdr); err != nil {
			t.Fatalf("WriteHeaders sid=%d: %v", sid, err)
		}
		if err := ProcessH2(context.Background(), hb.Bytes(), state, h, write, cfg); err != nil {
			t.Fatalf("ProcessH2 headers sid=%d: %v", sid, err)
		}

		// 2) WINDOW_UPDATEs to open the window fully so the buffered tail
		//    flushes with END_STREAM and the stream completes. Send enough
		//    increments to cover the whole body.
		var wb bytes.Buffer
		wf := http2.NewFramer(&wb, nil)
		for sent := window; sent < len(h.body); sent += window {
			if err := wf.WriteWindowUpdate(sid, uint32(window)); err != nil {
				t.Fatalf("WriteWindowUpdate sid=%d: %v", sid, err)
			}
		}
		if err := ProcessH2(context.Background(), wb.Bytes(), state, h, write, cfg); err != nil {
			t.Fatalf("ProcessH2 window-update sid=%d: %v", sid, err)
		}

		// After each stream is fully released, activeStreams must return to 0.
		// If the slot leaks, this climbs to maxStreams and never recovers.
		if got := mgr.CountActiveStreams(); got != 0 {
			t.Fatalf("after stream %d completed: activeStreams=%d, want 0 (slot leaked)", sid, got)
		}
	}

	// Belt-and-suspenders: no stream may ever have been REFUSED.
	mu.Lock()
	out := append([]byte(nil), writes...)
	mu.Unlock()
	refused := 0
	rf := http2.NewFramer(nil, bytes.NewReader(out))
	rf.SetMaxReadFrameSize(1 << 20)
	for {
		f, err := rf.ReadFrame()
		if err != nil {
			break
		}
		if v, ok := f.(*http2.RSTStreamFrame); ok && v.ErrCode == http2.ErrCodeRefusedStream {
			refused++
		}
	}
	if refused != 0 {
		t.Fatalf("got %d RST_STREAM(REFUSED_STREAM), want 0 — stalled streams must free their slot after flush", refused)
	}
}

// silentHandler never writes a response, leaving the stream active so a
// server-initiated RST fires while the stream still counts toward the limit.
type silentHandler struct{}

func (silentHandler) HandleStream(_ context.Context, _ *stream.Stream) error { return nil }

// TestProcessH2_ServerRSTFreesSlot is a regression test for the
// MAX_CONCURRENT_STREAMS slot LEAK that produced the sustained
// REFUSED_STREAM storm on the get-json-64k-h2 grid cell (~410M errors over
// 40 s, throughput degrading as activeStreams pinned at the limit).
//
// A server-initiated RST_STREAM on a still-active stream (here: request body
// exceeds MaxRequestBodySize) removed the stream from the manager map but
// never decremented activeStreams — the stream vanished yet its concurrency
// slot was leaked forever. Under sustained load a steady trickle of streams
// hit a transient RST condition, leaking one slot each, until activeStreams
// saturated at MaxConcurrentStreams and every new HEADERS was refused. The
// fix decrements the active count for any active stream removed via
// DeleteStream, regardless of which close path reached it.
//
// The smoking gun: after each RST the stream is GONE from the map
// (StreamCount → 0) but activeStreams stays pinned — map empty, limit full.
func TestProcessH2_ServerRSTFreesSlot(t *testing.T) {
	const (
		maxStreams = 8
		nStreams   = 40 // >> maxStreams: a leaked slot pins the limit fast
	)
	h := silentHandler{}
	write := func([]byte) {}
	cfg := H2Config{MaxConcurrentStreams: maxStreams, MaxRequestBodySize: 4}
	state := NewH2State(h, cfg, write, -1)
	mgr := state.processor.GetManager()

	var preface bytes.Buffer
	preface.WriteString(frame.ClientPreface)
	pf := http2.NewFramer(&preface, nil)
	if err := pf.WriteSettings(); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	if err := ProcessH2(context.Background(), preface.Bytes(), state, h, write, cfg); err != nil {
		t.Fatalf("ProcessH2 preface: %v", err)
	}

	hdr := encodeH2Headers(t, [][2]string{
		{":method", "POST"}, {":scheme", "http"}, {":path", "/"}, {":authority", "x"},
	})
	for i := 0; i < nStreams; i++ {
		sid := uint32(2*i + 1)
		// Open the stream (HEADERS, no END_STREAM → active), then send a DATA
		// frame whose body exceeds MaxRequestBodySize → server RST_STREAM via
		// sendRSTStreamAndMarkClosed on an active, in-map stream.
		var b bytes.Buffer
		fr := http2.NewFramer(&b, nil)
		if err := fr.WriteRawFrame(http2.FrameHeaders, http2.FlagHeadersEndHeaders, sid, hdr); err != nil {
			t.Fatalf("WriteHeaders sid=%d: %v", sid, err)
		}
		if err := fr.WriteData(sid, false, []byte("body-too-large")); err != nil {
			t.Fatalf("WriteData sid=%d: %v", sid, err)
		}
		// A frame-level error may surface as a ProcessH2 error on the body-cap
		// path; that is fine — we only care that the slot is reclaimed.
		_ = ProcessH2(context.Background(), b.Bytes(), state, h, write, cfg)

		if got := mgr.CountActiveStreams(); got != 0 {
			t.Fatalf("after RST of stream %d: activeStreams=%d, want 0 (slot leaked — streamCount=%d)",
				sid, got, mgr.StreamCount())
		}
	}
}

// countGoAwayFlowControl parses captured server output and returns how many
// GOAWAY frames carry FLOW_CONTROL_ERROR — the symptom of the connection-window
// overflow bug.
func countGoAwayFlowControl(t *testing.T, out []byte) int {
	t.Helper()
	rf := http2.NewFramer(nil, bytes.NewReader(out))
	rf.SetMaxReadFrameSize(1 << 20)
	n := 0
	for {
		f, err := rf.ReadFrame()
		if err != nil {
			break
		}
		if ga, ok := f.(*http2.GoAwayFrame); ok && ga.ErrCode == http2.ErrCodeFlowControl {
			n++
		}
	}
	return n
}

// TestProcessH2_ConnectionWindowNotOverflowed is the regression test for the
// 64 KB H2 "error storm" (probatorium grid get-json-64k-h2: ~300–560M loadgen
// errors). celeris never enforced/debited the CONNECTION-level SEND flow-control
// window (RFC 7540 §6.9): the send path debited only the per-stream window. A
// well-behaved client replenishes the connection window with WINDOW_UPDATE(0)
// credit equal to the DATA it has consumed; because the server never spent the
// connection window, that credit accumulated on top of a never-shrinking window
// and climbed 65535 → 2^31-1 → overflow, at which point celeris aborted the
// WHOLE connection with GOAWAY(FLOW_CONTROL_ERROR, lastStreamID=0), killing
// every in-flight stream. (Go net/http2 — chi/echo — debits the connection
// send window, so it stays bounded and never overflows.)
//
// The client here advertises a huge per-stream INITIAL_WINDOW_SIZE so the
// per-stream window is never the bottleneck — every large response is gated by
// the 65535-byte CONNECTION window exactly as a 64 KB body is on the live grid —
// and replenishes the connection window with credit EQUAL to the bytes it
// consumed (faithful client behaviour). With the fix the window is debited by
// every send, so credit only refills what was spent and the window stays pinned
// near its initial value; pre-fix it never shrinks, so the matching credit piles
// up unbounded (and the connection-stalled tail never even re-flushes, so bodies
// hang). Assertions: no FLOW_CONTROL GOAWAY, the window stays bounded, every
// body is delivered in full, and no concurrency slot leaks.
func TestProcessH2_ConnectionWindowNotOverflowed(t *testing.T) {
	const (
		nStreams  = 512      // pre-fix the window climbs by ~bodyLen each → ≫ bound
		bodyLen   = 64 << 10 // 64 KB responses (the live get-json-64k-h2 shape)
		streamWin = 1 << 30  // 1 GiB per-stream window: never the bottleneck
		initConn  = 65535    // RFC-default connection window celeris starts with
		creditPer = 64 << 10 // client credits exactly the body it consumed
	)
	h := respondHandler{body: bytes.Repeat([]byte("x"), bodyLen)}
	var mu sync.Mutex
	var writes []byte
	write := func(b []byte) {
		mu.Lock()
		writes = append(writes, b...)
		mu.Unlock()
	}
	cfg := H2Config{MaxConcurrentStreams: 100}
	state := NewH2State(h, cfg, write, -1)
	mgr := state.processor.GetManager()

	// Preface + client SETTINGS advertising a huge per-stream initial window.
	var preface bytes.Buffer
	preface.WriteString(frame.ClientPreface)
	pf := http2.NewFramer(&preface, nil)
	if err := pf.WriteSettings(http2.Setting{
		ID: http2.SettingInitialWindowSize, Val: streamWin,
	}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	if err := ProcessH2(context.Background(), preface.Bytes(), state, h, write, cfg); err != nil {
		t.Fatalf("ProcessH2 preface: %v", err)
	}

	hdr := encodeH2Headers(t, [][2]string{
		{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {":authority", "x"},
	})

	var totalData int
	for i := 0; i < nStreams; i++ {
		sid := uint32(2*i + 1)

		// HEADERS (complete GET). The response (64 KB) sends up to the current
		// connection window then stalls on it; the per-stream window has room.
		var hb bytes.Buffer
		hf := http2.NewFramer(&hb, nil)
		if err := hf.WriteRawFrame(http2.FrameHeaders,
			http2.FlagHeadersEndStream|http2.FlagHeadersEndHeaders, sid, hdr); err != nil {
			t.Fatalf("WriteHeaders sid=%d: %v", sid, err)
		}
		if err := ProcessH2(context.Background(), hb.Bytes(), state, h, write, cfg); err != nil {
			t.Fatalf("ProcessH2 headers sid=%d: %v", sid, err)
		}

		// Replenish the connection window with exactly what the body consumed,
		// as a real client does once it reads the 64 KB response off the wire.
		var wb bytes.Buffer
		wf := http2.NewFramer(&wb, nil)
		if err := wf.WriteWindowUpdate(0, creditPer); err != nil {
			t.Fatalf("WriteWindowUpdate(0) i=%d: %v", i, err)
		}
		if err := ProcessH2(context.Background(), wb.Bytes(), state, h, write, cfg); err != nil {
			t.Fatalf("ProcessH2 conn window-update i=%d: %v", i, err)
		}
	}

	mu.Lock()
	out := append([]byte(nil), writes...)
	mu.Unlock()

	// (a) No GOAWAY(FLOW_CONTROL) may ever be emitted.
	if n := countGoAwayFlowControl(t, out); n != 0 {
		t.Fatalf("got %d GOAWAY(FLOW_CONTROL_ERROR), want 0 — connection send window overflowed", n)
	}

	// (b) The connection window must stay bounded, NOT monotonically climb with
	//     each stream. With the fix every send debits it so the matching credit
	//     only refills the spend and it hovers near its initial value. Pre-fix
	//     it never shrinks, so nStreams × creditPer (~32 MB here) piles on top.
	if got := mgr.GetConnectionWindow(); got > initConn+creditPer {
		t.Fatalf("connection window climbed to %d (bound %d) — sends are not debiting it",
			got, initConn+creditPer)
	}

	// (c) Every body must be delivered in full. Pre-fix the connection-stalled
	//     tail of each response never re-flushes (the WINDOW_UPDATE(0) handler
	//     did not resume connection-stalled streams), so bytes go missing.
	rf := http2.NewFramer(nil, bytes.NewReader(out))
	rf.SetMaxReadFrameSize(1 << 20)
	for {
		f, err := rf.ReadFrame()
		if err != nil {
			break
		}
		if df, ok := f.(*http2.DataFrame); ok {
			totalData += len(df.Data())
		}
	}
	if want := nStreams * bodyLen; totalData != want {
		t.Fatalf("delivered %d DATA bytes across all streams, want %d (connection-stalled tails dropped)",
			totalData, want)
	}

	// (d) No concurrency slot may leak — every stream completed and released.
	if got := mgr.CountActiveStreams(); got != 0 {
		t.Fatalf("activeStreams=%d after all streams completed, want 0 (stalled streams leaked their slot)", got)
	}
}

// streamDataBytes parses captured output and returns total DATA payload bytes
// for streamID and whether a DATA frame carried END_STREAM.
func streamDataBytes(t *testing.T, out []byte, streamID uint32) (total int, sawEnd bool) {
	t.Helper()
	rf := http2.NewFramer(nil, bytes.NewReader(out))
	rf.SetMaxReadFrameSize(1 << 20)
	for {
		f, err := rf.ReadFrame()
		if err != nil {
			break
		}
		if df, ok := f.(*http2.DataFrame); ok && df.StreamID == streamID {
			total += len(df.Data())
			if df.StreamEnded() {
				sawEnd = true
			}
		}
	}
	return total, sawEnd
}

// TestProcessH2_ConnWindowStallResumes proves two things:
//
//  1. The server MUST NOT send more body bytes than the CONNECTION window has
//     authorized (RFC 7540 §6.9). Pre-fix the send path only consulted the
//     per-stream window, so with a huge per-stream window it shipped the entire
//     100 KB body even though the connection window only authorized 65535 bytes
//     — a flow-control violation that on a real client triggers the overflow
//     storm this PR fixes.
//  2. A stream that stalls on the connection window RESUMES and delivers its
//     full body once WINDOW_UPDATE(stream 0) credit arrives — the connection
//     window re-flush. Without it the stalled tail would hang forever (the
//     per-stream WINDOW_UPDATE path never fires; the peer only sends connection
//     credit).
func TestProcessH2_ConnWindowStallResumes(t *testing.T) {
	const (
		bodyLen   = 100 << 10 // 100 KB > the 65535-byte initial connection window
		streamWin = 1 << 30   // per-stream window never blocks
		initConn  = 65535     // RFC-default connection window celeris starts with
	)
	body := bytes.Repeat([]byte("y"), bodyLen)
	h := respondHandler{body: body}
	var mu sync.Mutex
	var writes []byte
	write := func(b []byte) {
		mu.Lock()
		writes = append(writes, b...)
		mu.Unlock()
	}
	cfg := H2Config{MaxConcurrentStreams: 100}
	state := NewH2State(h, cfg, write, -1)

	var preface bytes.Buffer
	preface.WriteString(frame.ClientPreface)
	pf := http2.NewFramer(&preface, nil)
	if err := pf.WriteSettings(http2.Setting{
		ID: http2.SettingInitialWindowSize, Val: streamWin,
	}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	if err := ProcessH2(context.Background(), preface.Bytes(), state, h, write, cfg); err != nil {
		t.Fatalf("ProcessH2 preface: %v", err)
	}

	hdr := encodeH2Headers(t, [][2]string{
		{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {":authority", "x"},
	})
	sid := uint32(1)
	var hb bytes.Buffer
	hf := http2.NewFramer(&hb, nil)
	if err := hf.WriteRawFrame(http2.FrameHeaders,
		http2.FlagHeadersEndStream|http2.FlagHeadersEndHeaders, sid, hdr); err != nil {
		t.Fatalf("WriteHeaders: %v", err)
	}
	if err := ProcessH2(context.Background(), hb.Bytes(), state, h, write, cfg); err != nil {
		t.Fatalf("ProcessH2 headers: %v", err)
	}

	// Before ANY connection credit: at most the initial connection window may
	// have been sent. Pre-fix the server ships the whole 100 KB body here
	// (per-stream window had room), exceeding what the connection authorized.
	mu.Lock()
	afterHeaders := append([]byte(nil), writes...)
	mu.Unlock()
	if sent, _ := streamDataBytes(t, afterHeaders, sid); sent > initConn {
		t.Fatalf("server sent %d body bytes with only %d connection window authorized — connection flow control not enforced",
			sent, initConn)
	}

	// Drip connection credit in chunks until the whole body is delivered. The
	// stalled tail must resume off the WINDOW_UPDATE(0) re-flush.
	for i := 0; i < 8; i++ {
		var wb bytes.Buffer
		wf := http2.NewFramer(&wb, nil)
		if err := wf.WriteWindowUpdate(0, 32<<10); err != nil {
			t.Fatalf("WriteWindowUpdate(0): %v", err)
		}
		if err := ProcessH2(context.Background(), wb.Bytes(), state, h, write, cfg); err != nil {
			t.Fatalf("ProcessH2 conn window-update %d: %v", i, err)
		}
	}

	mu.Lock()
	out := append([]byte(nil), writes...)
	mu.Unlock()

	if n := countGoAwayFlowControl(t, out); n != 0 {
		t.Fatalf("got %d GOAWAY(FLOW_CONTROL_ERROR), want 0", n)
	}

	dataBytes, sawEndStream := streamDataBytes(t, out, sid)
	if dataBytes != bodyLen {
		t.Fatalf("delivered %d body bytes, want %d (stream stalled on connection window never resumed fully)", dataBytes, bodyLen)
	}
	if !sawEndStream {
		t.Fatal("no END_STREAM DATA frame — response did not complete")
	}
}
