package stream

import (
	"bytes"
	"context"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// celeris#981: a request whose header block spans HEADERS + CONTINUATION
// opened its stream with Manager.GetOrCreateStream, which checks neither
// SETTINGS_MAX_CONCURRENT_STREAMS nor the order of stream identifiers and
// never advanced the last client stream. The three ways a header block
// reaches the processor, ProcessRawHeaders (the event loop's fast path for a
// plain HEADERS frame), a HEADERS frame through ProcessFrame, and HEADERS +
// CONTINUATION through ProcessFrame, now share one admission
// (Processor.admitHeaders). TestHeadersAdmission981 proves that: each
// scenario is a sequence of HEADERS (and PRIORITY) frames, driven through
// every path that can carry it, and what the processor did is compared with
// the explicit expectation, so the paths cannot differ from each other and
// cannot all be wrong the same way.

// h981 is the handler of the scenarios. It counts its runs and records
// whether a request carried the x-sync header; with async set it is a pool
// handler that holds until release is closed (so its stream stays open).
type h981 struct {
	async   bool
	release chan struct{}
	runs    atomic.Int32
	mu      sync.Mutex
	sawSync bool
}

func (h *h981) HandleStream(_ context.Context, s *Stream) error {
	h.mu.Lock()
	for _, kv := range s.GetHeaders() {
		if kv[0] == "x-sync" {
			h.sawSync = true
		}
	}
	h.mu.Unlock()
	h.runs.Add(1)
	if h.async {
		<-h.release
	}
	return nil
}
func (h *h981) RouteAsync(_, _ string) bool { return h.async }
func (h *h981) HasAsyncRoutes() bool        { return h.async }

// step981 is one frame a client sends: HEADERS with the given header block
// (end: END_STREAM; prio: the PRIORITY flag and fields), or a PRIORITY frame.
type step981 struct {
	priority bool
	id       uint32
	end      bool
	block    []byte
	prio     *http2.PriorityParam
}

// obs981 is what a scenario is compared by: where the connection ended, the
// GOAWAYs and RST_STREAMs the peer received, and the manager's books.
type obs981 struct {
	errAt      int // index of the first step that returned an error (the connection ends there); -1: none
	goaway     []goAwayRecord
	rst        []rstStreamRecord
	active     int
	streams    int
	lastClient uint32
	runs       int32
	sawSync    bool
}

type scenario981 struct {
	name string
	// limit is the SETTINGS_MAX_CONCURRENT_STREAMS the processor enforces (0: the default).
	limit uint32
	async bool
	// noRaw: the scenario uses a PRIORITY flag on HEADERS, which ProcessRawHeaders is never given.
	noRaw bool
	// noCounts: the scenario's stream is reset by a stream error that leaves it in the manager
	// (a separate matter, not what this test pins), so active and streams are not compared.
	noCounts bool
	steps    func(e *enc981) []step981
	want     obs981
}

// enc981 builds the scenarios' header blocks with one HPACK encoder, in the
// order the steps are sent: a block may reference what an earlier one added.
type enc981 struct {
	hb  bytes.Buffer
	enc *hpack.Encoder
}

func newEnc981() *enc981 {
	e := &enc981{}
	e.enc = hpack.NewEncoder(&e.hb)
	return e
}

func (e *enc981) block(fields ...[2]string) []byte {
	e.hb.Reset()
	for _, f := range fields {
		_ = e.enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	return append([]byte(nil), e.hb.Bytes()...)
}

func (e *enc981) req(extra ...[2]string) []byte {
	return e.block(append([][2]string{{":method", "GET"}, {":scheme", "http"}, {":authority", "example.com"}, {":path", "/"}}, extra...)...)
}

func hdr981(id uint32, end bool, block []byte) step981 {
	return step981{id: id, end: end, block: block}
}

func rst981(id uint32, code http2.ErrCode) rstStreamRecord {
	return rstStreamRecord{streamID: id, code: code}
}

// processFrames981 sends the frames written by write to p, one ProcessFrame
// each, and returns the first error.
func processFrames981(t *testing.T, p *Processor, write func(w *http2.Framer)) error {
	t.Helper()
	var buf bytes.Buffer
	write(http2.NewFramer(&buf, nil))
	r := http2.NewFramer(nil, &buf)
	r.SetMaxReadFrameSize(1 << 20)
	for buf.Len() > 0 {
		f, err := r.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if err := p.ProcessFrame(context.Background(), f); err != nil {
			return err
		}
	}
	return nil
}

// feed981 delivers one step to p the way path says: "raw" (ProcessRawHeaders),
// "frame" (one HEADERS frame) or "continuation" (HEADERS without END_HEADERS,
// then CONTINUATION).
func feed981(t *testing.T, p *Processor, path string, st step981) error {
	t.Helper()
	if st.priority {
		return processFrames981(t, p, func(w *http2.Framer) {
			if err := w.WritePriority(st.id, *st.prio); err != nil {
				t.Fatal(err)
			}
		})
	}
	switch path {
	case "raw":
		return p.ProcessRawHeaders(st.id, st.end, st.block)
	case "frame":
		return processFrames981(t, p, func(w *http2.Framer) {
			param := http2.HeadersFrameParam{StreamID: st.id, BlockFragment: st.block, EndStream: st.end, EndHeaders: true}
			if st.prio != nil {
				param.Priority = *st.prio
			}
			if err := w.WriteHeaders(param); err != nil {
				t.Fatal(err)
			}
		})
	case "continuation":
		return processFrames981(t, p, func(w *http2.Framer) {
			mid := len(st.block) / 2
			param := http2.HeadersFrameParam{StreamID: st.id, BlockFragment: st.block[:mid], EndStream: st.end, EndHeaders: false}
			if st.prio != nil {
				param.Priority = *st.prio
			}
			if err := w.WriteHeaders(param); err != nil {
				t.Fatal(err)
			}
			if err := w.WriteContinuation(st.id, true, st.block[mid:]); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Fatalf("unknown path %q", path)
	return nil
}

func run981(t *testing.T, sc scenario981, path string) obs981 {
	t.Helper()
	h := &h981{async: sc.async, release: make(chan struct{})}
	var once sync.Once
	releaseAll := func() { once.Do(func() { close(h.release) }) }
	defer releaseAll()
	fw := newTestFrameWriter()
	conn := &rstConn944{testResponseWriter: newTestResponseWriter()}
	p := NewProcessor(h, fw, conn)
	p.manager.SetLocalMaxFrameSize(1 << 20) // as NewH2State does (cfg.MaxFrameSize)
	if sc.limit > 0 {
		p.manager.SetMaxConcurrentStreams(sc.limit)
	}
	o := obs981{errAt: -1}
	for i, st := range sc.steps(newEnc981()) {
		err := feed981(t, p, path, st)
		// The end of the frame batch: inline-completed streams are released.
		p.FlushInlineCleanup()
		if err != nil {
			o.errAt = i
			break
		}
	}
	if sc.async { // a pool handler enters after the call returns
		for deadline := time.Now().Add(5 * time.Second); h.runs.Load() < sc.want.runs && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
	}
	o.runs = h.runs.Load()
	h.mu.Lock()
	o.sawSync = h.sawSync
	h.mu.Unlock()
	conn.testResponseWriter.mu.Lock()
	o.goaway = append(o.goaway, conn.goAwaysSent...)
	conn.testResponseWriter.mu.Unlock()
	fw.mu.Lock()
	o.goaway = append(o.goaway, fw.goAwaysSent...)
	o.rst = append(o.rst, fw.rstStreamsSent...)
	fw.mu.Unlock()
	o.rst = append(o.rst, conn.resets()...) // both sinks: the sink is not what is compared
	sort.Slice(o.rst, func(i, j int) bool { return o.rst[i].streamID < o.rst[j].streamID })
	if !sc.noCounts {
		o.active = int(p.manager.activeStreams.Load())
		p.manager.mu.RLock()
		o.streams = len(p.manager.streams)
		p.manager.mu.RUnlock()
	}
	o.lastClient = p.manager.GetLastClientStreamID()
	releaseAll()
	for deadline := time.Now().Add(10 * time.Second); p.PoolHandlersRunning() && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	p.FlushInlineCleanup()
	return o
}

func scenarios981() []scenario981 {
	protocol := http2.ErrCodeProtocol
	refused := http2.ErrCodeRefusedStream
	selfDep := &http2.PriorityParam{StreamDep: 1, Weight: 15}
	return []scenario981{
		{
			name: "limit-exceeded", limit: 2,
			steps: func(e *enc981) []step981 {
				return []step981{hdr981(1, false, e.req()), hdr981(3, false, e.req()), hdr981(5, true, e.req()), hdr981(7, true, e.req())}
			},
			want: obs981{errAt: -1, rst: []rstStreamRecord{rst981(5, refused), rst981(7, refused)}, active: 2, streams: 2, lastClient: 7},
		},
		{
			// The issue's reproducer at the processor: 10 streams against a limit of 4.
			name: "ten-streams-limit-four", limit: 4,
			steps: func(e *enc981) []step981 {
				var s []step981
				for id := uint32(1); id < 20; id += 2 {
					s = append(s, hdr981(id, false, e.req()))
				}
				return s
			},
			want: obs981{errAt: -1, active: 4, streams: 4, lastClient: 19, rst: []rstStreamRecord{
				rst981(9, refused), rst981(11, refused), rst981(13, refused), rst981(15, refused), rst981(17, refused), rst981(19, refused)}},
		},
		{
			// A refused stream's identifier is used up: HEADERS on it again is a connection error.
			name: "refused-id-reused", limit: 1,
			steps: func(e *enc981) []step981 {
				return []step981{hdr981(1, false, e.req()), hdr981(3, true, e.req()), hdr981(3, true, e.req())}
			},
			want: obs981{errAt: 2, goaway: []goAwayRecord{{3, protocol}}, rst: []rstStreamRecord{rst981(3, refused)}, active: 1, streams: 1, lastClient: 3},
		},
		{
			// ... and so is every lower one: the refusal advanced the last client stream.
			name: "lower-id-after-refused", limit: 1,
			steps: func(e *enc981) []step981 {
				return []step981{hdr981(1, false, e.req()), hdr981(5, true, e.req()), hdr981(3, true, e.req())}
			},
			want: obs981{errAt: 2, goaway: []goAwayRecord{{5, protocol}}, rst: []rstStreamRecord{rst981(5, refused)}, active: 1, streams: 1, lastClient: 5},
		},
		{
			// A refused block is decoded: the next request references the entry it added to the HPACK table.
			name: "refused-block-keeps-hpack-in-step", limit: 1,
			steps: func(e *enc981) []step981 {
				return []step981{
					hdr981(1, false, e.req()),
					hdr981(3, true, e.req([2]string{"x-sync", "v1"})),
					hdr981(1, true, e.block([2]string{"x-trailer", "t"})),
					hdr981(5, true, e.req([2]string{"x-sync", "v1"})),
				}
			},
			want: obs981{errAt: -1, rst: []rstStreamRecord{rst981(3, refused)}, lastClient: 5, runs: 2, sawSync: true},
		},
		{
			// A refused block that does not decode is a connection error; its GOAWAY names the last stream
			// the server opened (1), not 0, and the refused stream's identifier was used up (3).
			name: "refused-block-does-not-decode", limit: 1,
			steps: func(e *enc981) []step981 {
				return []step981{hdr981(1, false, e.req()), hdr981(3, true, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f, 0x80})}
			},
			want: obs981{errAt: 1, goaway: []goAwayRecord{{1, http2.ErrCodeCompression}}, active: 1, streams: 1, lastClient: 3},
		},
		{
			name: "id-reused-after-completion",
			steps: func(e *enc981) []step981 {
				return []step981{hdr981(1, true, e.req()), hdr981(1, true, e.req())}
			},
			want: obs981{errAt: 1, goaway: []goAwayRecord{{1, protocol}}, lastClient: 1, runs: 1},
		},
		{
			name: "lower-id",
			steps: func(e *enc981) []step981 {
				return []step981{hdr981(5, true, e.req()), hdr981(3, true, e.req())}
			},
			want: obs981{errAt: 1, goaway: []goAwayRecord{{5, protocol}}, lastClient: 5, runs: 1},
		},
		{
			name: "even-id",
			steps: func(e *enc981) []step981 {
				return []step981{hdr981(2, true, e.req())}
			},
			want: obs981{errAt: 0, goaway: []goAwayRecord{{0, protocol}}},
		},
		{
			// PRIORITY on an idle stream leaves a placeholder in the manager: HEADERS on its identifier
			// after a higher stream was opened is still an identifier out of order.
			name: "id-below-idle-priority-placeholder",
			steps: func(e *enc981) []step981 {
				return []step981{
					{priority: true, id: 3, prio: &http2.PriorityParam{StreamDep: 1, Weight: 15}},
					hdr981(5, true, e.req()),
					hdr981(3, true, e.req()),
				}
			},
			want: obs981{errAt: 2, goaway: []goAwayRecord{{5, protocol}}, lastClient: 5, runs: 1, streams: 1},
		},
		{
			name: "headers-on-half-closed-stream", async: true,
			steps: func(e *enc981) []step981 {
				return []step981{hdr981(1, true, e.req()), hdr981(1, true, e.req())}
			},
			want: obs981{errAt: 1, goaway: []goAwayRecord{{1, http2.ErrCodeStreamClosed}}, active: 1, streams: 1, lastClient: 1, runs: 1},
		},
		{
			name: "second-headers-without-end-stream",
			steps: func(e *enc981) []step981 {
				return []step981{hdr981(1, false, e.req()), hdr981(1, false, e.block([2]string{"x-trailer", "t"}))}
			},
			want: obs981{errAt: 1, goaway: []goAwayRecord{{1, protocol}}, active: 1, streams: 1, lastClient: 1},
		},
		{
			name: "trailers",
			steps: func(e *enc981) []step981 {
				return []step981{hdr981(1, false, e.req()), hdr981(1, true, e.block([2]string{"x-trailer", "t"}))}
			},
			want: obs981{errAt: -1, lastClient: 1, runs: 1},
		},
		{
			name: "invalid-request-headers", noCounts: true,
			steps: func(e *enc981) []step981 {
				return []step981{hdr981(1, true, e.block([][2]string{{":method", "GET"}, {":scheme", "http"}, {":authority", "example.com"}}...))}
			},
			want: obs981{errAt: 0, rst: []rstStreamRecord{rst981(1, protocol)}, lastClient: 1},
		},
		{
			name: "content-length-mismatch", noCounts: true,
			steps: func(e *enc981) []step981 {
				return []step981{hdr981(1, true, e.req([2]string{"content-length", "5"}))}
			},
			want: obs981{errAt: 0, rst: []rstStreamRecord{rst981(1, protocol)}, lastClient: 1},
		},
		{
			name: "priority-depends-on-itself", noRaw: true,
			steps: func(e *enc981) []step981 {
				return []step981{{id: 1, end: true, block: e.req(), prio: selfDep}}
			},
			want: obs981{errAt: 0, goaway: []goAwayRecord{{1, protocol}}, active: 1, streams: 1, lastClient: 1},
		},
	}
}

func TestHeadersAdmission981(t *testing.T) {
	for _, sc := range scenarios981() {
		for _, path := range []string{"raw", "frame", "continuation"} {
			if path == "raw" && sc.noRaw {
				continue
			}
			t.Run(sc.name+"/"+path, func(t *testing.T) {
				got := run981(t, sc, path)
				want := sc.want
				if sc.noCounts {
					want.active, want.streams = 0, 0
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s: the processor did\n   %+v\nwant\n   %+v", path, got, want)
				}
			})
		}
	}
}
