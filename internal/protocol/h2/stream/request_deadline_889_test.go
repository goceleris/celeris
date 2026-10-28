package stream

import (
	"context"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// The request deadline and the header-block deadline of the processor
// (celeris#889). What reaches a client is pinned in test/server
// (h2_request_deadline_889_linux_test.go).

// recWriter889 is a response writer that records the statuses it is asked to
// write: the 408 of an expired request.
type recWriter889 struct {
	*testResponseWriter
	mu       sync.Mutex
	statuses map[uint32]int
}

func newRecWriter889() *recWriter889 {
	return &recWriter889{testResponseWriter: newTestResponseWriter(), statuses: map[uint32]int{}}
}

func (w *recWriter889) WriteResponse(s *Stream, status int, _ [][2]string, _ []byte) error {
	w.mu.Lock()
	w.statuses[s.ID] = status
	w.mu.Unlock()
	return nil
}

func deadlineProc889(t *testing.T, timeout time.Duration) (*Processor, *testFrameWriter, *recWriter889) {
	t.Helper()
	fw := newTestFrameWriter()
	rw := newRecWriter889()
	ran := HandlerFunc(func(context.Context, *Stream) error { return nil })
	p := NewProcessor(ran, fw, rw)
	p.InlineWriter = rw
	p.GetManager().SetRequestTimeout(timeout)
	return p, fw, rw
}

func postHeaders889(t *testing.T, id uint32, endStream bool) http2.Frame {
	t.Helper()
	block := encodeHeaders(t, [][2]string{{":method", "POST"}, {":scheme", "http"}, {":path", "/u"}, {":authority", "x"}})
	return makeHeadersFrame(t, id, endStream, true, block)
}

func TestRequestDeadlineArmedOnlyWhenTheRequestIsNotWhole889(t *testing.T) {
	p, _, _ := deadlineProc889(t, time.Second)
	m := p.GetManager()
	ctx := context.Background()
	if err := p.ProcessFrame(ctx, postHeaders889(t, 1, true)); err != nil { // a request whole at its HEADERS
		t.Fatal(err)
	}
	if m.reqArmed.Load() != 0 || m.NextRequestDeadline() != 0 {
		t.Fatalf("a request whole at its HEADERS armed a deadline: armed %d, hint %d", m.reqArmed.Load(), m.NextRequestDeadline())
	}
	if err := p.ProcessFrame(ctx, postHeaders889(t, 3, false)); err != nil {
		t.Fatal(err)
	}
	s, ok := m.GetStream(3)
	if !ok || s.flags.Load()&flagReqDeadline == 0 || s.StartTimeNs == 0 {
		t.Fatalf("a request open at its HEADERS is not armed (stream %v, ok %v)", s, ok)
	}
	if m.reqArmed.Load() != 1 || m.NextRequestDeadline() != s.StartTimeNs+int64(time.Second) {
		t.Fatalf("armed %d, hint %d, want 1 and %d", m.reqArmed.Load(), m.NextRequestDeadline(), s.StartTimeNs+int64(time.Second))
	}
	// A DATA frame that does not end the request does not move the deadline.
	at := s.StartTimeNs
	if err := p.ProcessFrame(ctx, makeDataFrame(t, 3, false, []byte("x"))); err != nil {
		t.Fatal(err)
	}
	if s.StartTimeNs != at {
		t.Fatalf("DATA moved the deadline: %d to %d", at, s.StartTimeNs)
	}
	// Its END_STREAM disarms it, and the handler sees an unset start time.
	if err := p.ProcessFrame(ctx, makeDataFrame(t, 3, true, nil)); err != nil {
		t.Fatal(err)
	}
	if m.reqArmed.Load() != 0 || s.StartTimeNs != 0 {
		t.Fatalf("after END_STREAM: armed %d, StartTimeNs %d, want 0 and 0", m.reqArmed.Load(), s.StartTimeNs)
	}
}

func TestRequestDeadlineOffWithoutATimeout889(t *testing.T) {
	p, _, _ := deadlineProc889(t, 0)
	if err := p.ProcessFrame(context.Background(), postHeaders889(t, 1, false)); err != nil {
		t.Fatal(err)
	}
	if p.GetManager().reqArmed.Load() != 0 {
		t.Fatal("a deadline was armed with no ReadTimeout")
	}
}

func TestRequestDeadlineDisarmedWhenTheStreamLeaves889(t *testing.T) {
	p, _, _ := deadlineProc889(t, time.Second)
	m := p.GetManager()
	ctx := context.Background()
	for id := uint32(1); id <= 7; id += 2 {
		if err := p.ProcessFrame(ctx, postHeaders889(t, id, false)); err != nil {
			t.Fatal(err)
		}
	}
	if m.reqArmed.Load() != 4 {
		t.Fatalf("armed %d, want 4", m.reqArmed.Load())
	}
	if err := p.ProcessFrame(ctx, makeRSTStreamFrame(t, 1, http2.ErrCodeCancel)); err != nil { // the peer resets
		t.Fatal(err)
	}
	m.DeleteStream(3)                                       // deleted
	_ = p.handleGoAway(&http2.GoAwayFrame{LastStreamID: 5}) // the peer's GOAWAY ends 7
	if m.reqArmed.Load() != 1 {
		t.Fatalf("after a reset, a delete and a GOAWAY: armed %d, want 1 (stream 5)", m.reqArmed.Load())
	}
	// A hint left stale by the disarms costs one scan, and settles.
	ids := m.ExpiredRequests(time.Now().UnixNano()+int64(time.Hour), nil)
	if len(ids) != 0 {
		t.Fatalf("no cap, no ids: %v", ids)
	}
	ids = m.ExpiredRequests(time.Now().UnixNano()+int64(time.Hour), make([]uint32, 0, 8))
	if len(ids) != 1 || ids[0] != 5 {
		t.Fatalf("expired %v, want [5]", ids)
	}
	if m.NextRequestDeadline() != 0 {
		t.Fatalf("hint %d after the last armed stream expired, want 0", m.NextRequestDeadline())
	}
}

func TestExpireRequestAnswers408AndRemembersTheStream889(t *testing.T) {
	p, fw, rw := deadlineProc889(t, time.Second)
	m := p.GetManager()
	ctx := context.Background()
	if err := p.ProcessFrame(ctx, postHeaders889(t, 1, false)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixNano() + int64(2*time.Second)
	for _, id := range m.ExpiredRequests(now, make([]uint32, 0, 8)) {
		p.ExpireRequest(id, now)
	}
	// An expiry is not a refusal: none of the flood budgets is charged.
	if p.refusedHeaderLists != 0 || p.refusedStreams != 0 || p.refusedMalformed != 0 || p.refusedBodies != 0 || p.emptyDataFrames != 0 {
		t.Fatalf("an expiry charged a flood budget: header lists %d, streams %d, malformed %d, bodies %d, empty DATA %d",
			p.refusedHeaderLists, p.refusedStreams, p.refusedMalformed, p.refusedBodies, p.emptyDataFrames)
	}
	rw.mu.Lock()
	status := rw.statuses[1]
	rw.mu.Unlock()
	fw.mu.Lock()
	rsts := append([]rstStreamRecord(nil), fw.rstStreamsSent...)
	fw.mu.Unlock()
	if status != 408 || len(rsts) != 1 || rsts[0].streamID != 1 || rsts[0].code != http2.ErrCodeNo {
		t.Fatalf("status %d, resets %+v, want 408 and one RST_STREAM(NO_ERROR) for stream 1", status, rsts)
	}
	if _, ok := m.GetStream(1); ok || m.activeStreams.Load() != 0 || m.requestOpen.Load() != 0 || m.reqArmed.Load() != 0 {
		t.Fatalf("the expired stream is still counted: active %d, request open %d, armed %d", m.activeStreams.Load(), m.requestOpen.Load(), m.reqArmed.Load())
	}
	// Its DATA and trailers in flight, however old the stream: ignored, the
	// connection's window credited, no error. The reset window forgets stream 1
	// once a reset 2048 identifiers newer is marked; the tombstone does not.
	p.resetIDs.mark(1 + 2*(serverResetWindow+10))
	if p.resetIDs.has(1) {
		t.Fatal("the test's premise: the reset window should have forgotten stream 1")
	}
	if err := p.ProcessFrame(ctx, makeDataFrame(t, 1, false, []byte("late"))); err != nil {
		t.Fatalf("DATA on an expired request ended the connection: %v", err)
	}
	if !p.TakeIgnoredActivity() {
		t.Fatal("an ignored DATA frame is not counted as activity")
	}
	trailers := makeHeadersFrame(t, 1, true, true, encodeHeaders(t, [][2]string{{"x-t", "1"}}))
	if err := p.ProcessFrame(ctx, trailers); err != nil {
		t.Fatalf("trailers on an expired request ended the connection: %v", err)
	}
	if p.requestExpired(1) {
		t.Fatal("the tombstone outlived the client's END_STREAM")
	}
	// Forgotten (the client ended it): a further DATA is the connection error it always was.
	if err := p.ProcessFrame(ctx, makeDataFrame(t, 1, false, []byte("again"))); err == nil {
		t.Fatal("DATA on a stream the client had ended did not end the connection")
	}
}

func TestExpiredRequestTombstonesRunOut889(t *testing.T) {
	p, _, _ := deadlineProc889(t, time.Second)
	ctx := context.Background()
	if err := p.ProcessFrame(ctx, postHeaders889(t, 1, false)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixNano() + int64(2*time.Second)
	for _, id := range p.GetManager().ExpiredRequests(now, make([]uint32, 0, 8)) {
		p.ExpireRequest(id, now)
	}
	if p.NextTombstoneDeadline() != now+2*int64(time.Second) {
		t.Fatalf("tombstone deadline %d, want %d", p.NextTombstoneDeadline(), now+2*int64(time.Second))
	}
	p.DropTombstones(now + int64(time.Second))
	if !p.requestExpired(1) {
		t.Fatal("the tombstone ran out early")
	}
	p.DropTombstones(now + 3*int64(time.Second))
	if p.requestExpired(1) || p.NextTombstoneDeadline() != 0 {
		t.Fatal("the tombstone did not run out")
	}
}

func TestRequestDeadlineNeverAllocatesOnTheRequestPath889(t *testing.T) {
	m := NewManager()
	m.SetRequestTimeout(time.Second)
	s, _ := m.TryOpenStream(1)
	now := time.Now().UnixNano()
	if n := testing.AllocsPerRun(1000, func() {
		m.ArmRequest(s, now)
		m.DisarmRequest(s)
	}); n != 0 {
		t.Fatalf("arm and disarm allocate %v times", n)
	}
	// A stream that never armed pays one load.
	g, _ := m.TryOpenStream(3)
	if n := testing.AllocsPerRun(1000, func() { m.DisarmRequest(g) }); n != 0 {
		t.Fatalf("disarm of an unarmed stream allocates %v times", n)
	}
}

func TestServerOwesIgnoresAnOpenRequestWhenItHasADeadline889(t *testing.T) {
	m := NewManager()
	m.TryOpenStream(1) // request open
	owes889(t, m, false, "with a request open and no request deadline")
	m.SetRequestTimeout(time.Second)
	owes889(t, m, true, "with a request open and a request deadline (the stream is bounded one by one)")
	m.outboundHeld.Add(1)
	owes889(t, m, false, "with a response waiting for the client's window")
}

func TestHeaderBlockDeadline889(t *testing.T) {
	p, _, rw := deadlineProc889(t, 0)
	p.SetHeaderBlockTimeout(time.Second)
	ctx := context.Background()
	block := encodeHeaders(t, [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {":authority", "x"}})
	if err := p.ProcessFrame(ctx, makeHeadersFrame(t, 1, true, false, block[:3])); err != nil { // no END_HEADERS
		t.Fatal(err)
	}
	dl := p.NextHeaderBlockDeadline()
	if dl == 0 || dl < time.Now().UnixNano() {
		t.Fatalf("header block deadline %d, want one in the future", dl)
	}
	var buf []byte
	_ = buf
	p.ExpireHeaderBlock()
	rw.mu.Lock()
	ga := append([]goAwayRecord(nil), rw.goAwaysSent...)
	rw.mu.Unlock()
	if len(ga) != 1 || ga[0].code != http2.ErrCodeEnhanceYourCalm {
		t.Fatalf("GOAWAYs %+v, want one ENHANCE_YOUR_CALM", ga)
	}
	if p.NextHeaderBlockDeadline() != 0 {
		t.Fatal("the deadline is still armed after it expired")
	}
	// Off by default.
	q, _, _ := deadlineProc889(t, 0)
	if err := q.ProcessFrame(ctx, makeHeadersFrame(t, 1, true, false, block[:3])); err != nil {
		t.Fatal(err)
	}
	if q.NextHeaderBlockDeadline() != 0 {
		t.Fatal("a header block armed a deadline with no ReadHeaderTimeout")
	}
}
