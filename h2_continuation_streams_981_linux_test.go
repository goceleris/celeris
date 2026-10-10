//go:build linux

package celeris_test

import (
	"bytes"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
)

// celeris#981: a request whose header block spans HEADERS + CONTINUATION
// opened its stream without the SETTINGS_MAX_CONCURRENT_STREAMS check and
// without the stream-ID order check, on epoll, io_uring and adaptive: a
// client that always splits its headers had no stream limit. The std engine
// (net/http) is the control: it applies both whichever way the headers come.

// wire981 is a client connection with a goroutine that reads what the server
// sends: the stream resets, the GOAWAY, and the :status of each response.
type wire981 struct {
	t    *testing.T
	conn net.Conn
	fr   *http2.Framer
	addr string
	hb   bytes.Buffer
	enc  *hpack.Encoder
	wmu  sync.Mutex // the framer's writes: the test's requests and the reader's SETTINGS ack

	mu     sync.Mutex
	rst    map[uint32]http2.ErrCode
	status map[uint32]string
	goaway string
}

func dial981(t *testing.T, addr string) *wire981 {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	w := &wire981{t: t, conn: conn, fr: http2.NewFramer(conn, conn), addr: addr,
		rst: map[uint32]http2.ErrCode{}, status: map[uint32]string{}}
	w.enc = hpack.NewEncoder(&w.hb)
	if err := w.fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	go w.read()
	return w
}

// read runs until the connection is closed.
func (w *wire981) read() {
	dec := hpack.NewDecoder(4096, nil)
	var cur uint32
	dec.SetEmitFunc(func(f hpack.HeaderField) {
		if f.Name == ":status" {
			w.mu.Lock()
			w.status[cur] = f.Value
			w.mu.Unlock()
		}
	})
	for {
		f, err := w.fr.ReadFrame()
		if err != nil {
			return
		}
		w.mu.Lock()
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				w.wmu.Lock()
				_ = w.fr.WriteSettingsAck()
				w.wmu.Unlock()
			}
		case *http2.RSTStreamFrame:
			w.rst[f.StreamID] = f.ErrCode
		case *http2.GoAwayFrame:
			w.goaway = fmt.Sprintf("%s (last stream %d)", f.ErrCode, f.LastStreamID)
		case *http2.HeadersFrame:
			cur = f.StreamID
			w.mu.Unlock()
			_, _ = dec.Write(f.HeaderBlockFragment())
			w.mu.Lock()
		}
		w.mu.Unlock()
	}
}

// get sends GET /hold on stream id: in one HEADERS frame, or, with split,
// HEADERS (END_STREAM) with the first 3 bytes of the block, then one
// CONTINUATION (END_HEADERS) with the rest.
func (w *wire981) get(id uint32, split bool) {
	w.t.Helper()
	if err := w.try(id, split); err != nil {
		w.t.Fatalf("stream %d: %v", id, err)
	}
}

// try is get, returning the write error.
func (w *wire981) try(id uint32, split bool) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	w.hb.Reset()
	for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":authority", w.addr}, {":path", "/hold"}} {
		_ = w.enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	blk := append([]byte(nil), w.hb.Bytes()...)
	if !split {
		return w.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: blk, EndStream: true, EndHeaders: true})
	}
	if err := w.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: blk[:3], EndStream: true, EndHeaders: false}); err != nil {
		return err
	}
	return w.fr.WriteContinuation(id, true, blk[3:])
}

func (w *wire981) state() (rst map[uint32]http2.ErrCode, status map[uint32]string, goaway string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rst, status = map[uint32]http2.ErrCode{}, map[uint32]string{}
	for k, v := range w.rst {
		rst[k] = v
	}
	for k, v := range w.status {
		status[k] = v
	}
	return rst, status, w.goaway
}

func waitUntil981(d time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

func splitName981(split bool) string {
	if split {
		return "headers+continuation"
	}
	return "plain-headers"
}

// TestH2MaxConcurrentStreamsHeadersContinuation981: the server advertises 4;
// the client opens 10 streams, each a GET whose handler blocks. Four handlers
// run, and the other six streams are refused (RST_STREAM), whether the
// request is one HEADERS frame or HEADERS + CONTINUATION. Then the handlers
// finish, the four answers arrive, and then, on the same connection,
// either one more request of the same shape is served (the slots were counted
// back: "slots-come-back"), or HEADERS on a refused stream's identifier is a
// connection error (a refused stream's identifier is used up:
// "refused-id-reused"; they are two runs because the first moves the last
// client stream past the refused identifiers).
func TestH2MaxConcurrentStreamsHeadersContinuation981(t *testing.T) {
	const limit, streams = 4, 10
	for _, e := range engines761 {
		for _, split := range []bool{false, true} {
			for _, tail := range []string{"slots-come-back", "refused-id-reused"} {
				name := e.name + "/" + splitName981(split) + "/" + tail
				t.Run(name, func(t *testing.T) {
					var cur, peak, entered atomic.Int64
					release := make(chan struct{})
					var once sync.Once
					defer once.Do(func() { close(release) })
					addr := startServerConfig761(t, celeris.Config{Engine: e.eng, MaxConcurrentStreams: limit}, func(s *celeris.Server) {
						s.GET("/hold", func(c *celeris.Context) error {
							entered.Add(1)
							n := cur.Add(1)
							for {
								p := peak.Load()
								if n <= p || peak.CompareAndSwap(p, n) {
									break
								}
							}
							<-release
							cur.Add(-1)
							return c.String(200, "ok")
						}).Async()
					})
					w := dial981(t, addr)
					for i := range streams {
						w.get(uint32(2*i+1), split)
					}
					// Four are admitted; the other six are refused, which is the end of the settling.
					waitUntil981(10*time.Second, func() bool { rst, _, _ := w.state(); return len(rst) >= streams-limit })
					time.Sleep(200 * time.Millisecond) // a handler that should not have run would have entered by now
					rst, _, goaway := w.state()
					var ids []int
					codes := map[http2.ErrCode]int{}
					for id, c := range rst {
						ids = append(ids, int(id))
						codes[c]++
					}
					sort.Ints(ids)
					t.Logf("%s: MaxConcurrentStreams %d; %d streams opened; handlers entered %d, peak concurrent %d; reset streams %v %v; GOAWAY %q",
						name, limit, streams, entered.Load(), peak.Load(), ids, codes, goaway)
					if peak.Load() > limit || entered.Load() > limit {
						t.Errorf("%d handlers entered, %d at once, with SETTINGS_MAX_CONCURRENT_STREAMS %d", entered.Load(), peak.Load(), limit)
					}
					if entered.Load() != limit {
						t.Errorf("%d handlers entered, want %d", entered.Load(), limit)
					}
					if goaway != "" {
						t.Errorf("the connection got a GOAWAY %q: a stream over the limit is a stream error", goaway)
					}
					for id := uint32(2*limit + 1); id < 2*streams+1; id += 2 {
						// net/http answers PROTOCOL_ERROR once the client acked its SETTINGS and REFUSED_STREAM before.
						c, ok := rst[id]
						if !ok {
							t.Errorf("stream %d (over the limit) got no RST_STREAM", id)
						} else if c != http2.ErrCodeRefusedStream && !(e.eng == celeris.Std && c == http2.ErrCodeProtocol) {
							t.Errorf("stream %d reset with %s, want REFUSED_STREAM", id, c)
						}
					}
					if len(rst) != streams-limit {
						t.Errorf("%d streams were reset, want %d", len(rst), streams-limit)
					}

					// The four finish; their slots come back; one more request is served.
					once.Do(func() { close(release) })
					if !waitUntil981(10*time.Second, func() bool {
						_, status, _ := w.state()
						n := 0
						for _, s := range status {
							if s == "200" {
								n++
							}
						}
						return n >= limit
					}) {
						_, status, _ := w.state()
						t.Fatalf("the %d admitted streams were not all answered: %v", limit, status)
					}
					if tail == "slots-come-back" {
						w.get(2*streams+1, split)
						if !waitUntil981(10*time.Second, func() bool { _, status, _ := w.state(); return status[2*streams+1] == "200" }) {
							rst, status, goaway := w.state()
							t.Errorf("stream %d after the others finished: status %v, resets %v, GOAWAY %q", 2*streams+1, status, rst, goaway)
						}
						return
					}
					w.get(2*limit+1, split)
					if !waitUntil981(10*time.Second, func() bool { _, _, g := w.state(); return strings.HasPrefix(g, http2.ErrCodeProtocol.String()) }) {
						_, _, goaway := w.state()
						t.Errorf("HEADERS on the refused stream %d again: GOAWAY %q, want PROTOCOL_ERROR", 2*limit+1, goaway)
					}
					if n := entered.Load(); n != limit {
						t.Errorf("%d handlers entered, want %d: the refused stream's second request must not run", n, limit)
					}
				})
			}
		}
	}
}

func TestH2StreamIDOrderHeadersContinuation981(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first []uint32 // streams answered before the offending one
		bad   uint32
	}{
		{"reused-id", []uint32{1}, 1},
		{"lower-id", []uint32{5}, 3},
	} {
		for _, e := range engines761 {
			for _, split := range []bool{false, true} {
				name := tc.name + "/" + e.name + "/" + splitName981(split)
				t.Run(name, func(t *testing.T) {
					var entered atomic.Int64
					addr := startServerConfig761(t, celeris.Config{Engine: e.eng}, func(s *celeris.Server) {
						s.GET("/hold", func(c *celeris.Context) error {
							entered.Add(1)
							return c.String(200, "ok")
						}).Async()
					})
					w := dial981(t, addr)
					for _, id := range tc.first {
						w.get(id, split)
						if !waitUntil981(10*time.Second, func() bool { _, status, _ := w.state(); return status[id] == "200" }) {
							t.Fatalf("stream %d was not answered", id)
						}
					}
					w.get(tc.bad, split)
					if !waitUntil981(10*time.Second, func() bool { _, _, g := w.state(); return g != "" }) {
						rst, status, _ := w.state()
						t.Errorf("stream %d (reused or lower) got no GOAWAY: resets %v, statuses %v", tc.bad, rst, status)
					}
					time.Sleep(100 * time.Millisecond)
					_, status, goaway := w.state()
					t.Logf("%s: handlers entered %d; GOAWAY %q; statuses %v", name, entered.Load(), goaway, status)
					if !strings.HasPrefix(goaway, http2.ErrCodeProtocol.String()) {
						t.Errorf("GOAWAY %q, want PROTOCOL_ERROR", goaway)
					}
					if int(entered.Load()) != len(tc.first) {
						t.Errorf("%d handlers ran, want %d (the offending stream's must not)", entered.Load(), len(tc.first))
					}
				})
			}
		}
	}
}
