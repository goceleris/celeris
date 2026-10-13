//go:build linux

package celeris_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
)

// celeris#848 on HTTP/2 (h2c, prior knowledge). The docs (stream.Handler,
// Route.Sync, README) state two outcomes of a runtime.Goexit in a handler, by
// where the stream runs. Processor.runHandler sends a stream to the event loop
// when its request has ended (every frame path sets that before it runs the
// handler, so whether the request has a body does not matter), its route is
// not async and its connection is within the outbound budget. So:
//   - a stream of a sync route runs on the event loop's goroutine whatever
//     its request carries (a GET; a POST with a body; a header block split by
//     CONTINUATION frames; a body followed by trailers): the loop ends and the
//     engine logs the loop-exit record. Tests: TestH2InlineStreamGoexitIsLoud848
//     (GET) and TestH2InlineShapesGoexitAreLoud848 (the others);
//   - a stream on the h2 worker pool does not run on the loop: the loop keeps
//     serving, the stream is reset with RST_STREAM(INTERNAL_ERROR) and the
//     Goexit is logged with its stack as one Error record (celeris#889; it was
//     no answer and no record before), the connection keeps its other streams,
//     the pool goroutine that ran the handler is replaced, and the engine does
//     not report a loop exit. That happens for an async route
//     (TestH2AsyncStreamGoexitKeepsTheLoop848, a GET and a POST with a body)
//     and for a sync route on a connection that already holds OutboundBudget
//     (4 MiB) of response data (TestH2OverBudgetStreamGoexitKeepsTheLoop848).
//
// The inline stream's Goexit is logged and counted by the processor too, and
// the stream reset, but the loop that would send the RST_STREAM is the one that
// ended, so its client still gets no frame.

type h2rig848 struct {
	s      *celeris.Server
	addr   string
	logs   *capture848
	cancel context.CancelFunc
	done   chan error
	hits   atomic.Int64
}

func (r *h2rig848) newServer(eng celeris.EngineType) {
	r.s = celeris.New(celeris.Config{Engine: eng, Protocol: celeris.H2C, Workers: 2, Logger: slog.New(r.logs)})
	r.s.GET("/boom", func(c *celeris.Context) error {
		r.hits.Add(1)
		runtime.Goexit()
		return nil
	})
	r.s.GET("/aboom", func(c *celeris.Context) error {
		r.hits.Add(1)
		runtime.Goexit()
		return nil
	}).Async()
	r.s.POST("/pboom", func(c *celeris.Context) error {
		r.hits.Add(1)
		runtime.Goexit()
		return nil
	})
	r.s.POST("/apboom", func(c *celeris.Context) error {
		r.hits.Add(1)
		runtime.Goexit()
		return nil
	}).Async()
	r.s.GET("/fill", func(c *celeris.Context) error {
		return c.Blob(200, "application/octet-stream", fillBody848)
	})
	r.s.GET("/ok", func(c *celeris.Context) error { return c.String(200, "ok") })
}

func startH2848(t *testing.T, eng celeris.EngineType) *h2rig848 {
	t.Helper()
	r := &h2rig848{logs: &capture848{}}
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.newServer(eng)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		r.done = make(chan error, 1)
		go func() { r.done <- r.s.StartWithListenerAndContext(ctx, ln) }()
		if r.addr, err = c714WaitReady(r.s, r.done); err == nil {
			break
		}
		cancel()
		_ = ln.Close()
		// A start that fails only with ENOMEM (io_uring ring memory still
		// charged to RLIMIT_MEMLOCK) is retried with a new server.
		if !strings.Contains(err.Error(), "cannot allocate memory") || time.Now().After(deadline) {
			t.Fatalf("server did not start: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Cleanup(r.cancel)
	return r
}

func (r *h2rig848) stopWithin(d time.Duration) bool {
	r.cancel()
	select {
	case <-r.done:
		return true
	case <-time.After(d):
		return false
	}
}

// h2Probe848 sends GET /ok on fresh h2c connections, up to n, and returns how
// many were answered before the first one that was not (stopOnMiss), or in all.
// With two workers a new connection lands on either by the kernel's hash, so
// one probe cannot tell a dead loop from a live one; n of them can (all n on
// the live worker is 1 in 2^n).
func h2Probe848(addr string, n int, d time.Duration, stopOnMiss bool) (answered, tried int) {
	for range n {
		tried++
		if ok, _, _ := h2StreamEnds837(addr, "/ok", d); ok {
			answered++
		} else if stopOnMiss {
			return
		}
	}
	return
}

// freshAfterFault848 judges the fresh h2c conns made after a loop died. The
// dead loop's SO_REUSEPORT listener is shut when its goroutine ends
// (Engine.runWatched), so no conn lands on it: with a surviving loop every
// probe is answered, and with none (io_uring at the CI step's 8 MiB memlock
// funds one worker) the connect is refused. A listener left open in the group
// would hold a share of the probes unanswered, which is what this test used to
// read as proof that the loop had died; that is now the defect, and the proof
// that the loop died is the one record and, in the shape test, the faulted
// connection's stream 3.
func freshAfterFault848(t *testing.T, addr string, answered, tried int) {
	t.Helper()
	if answered == tried {
		return
	}
	// A miss: either no loop is left and the connect is refused, or the dead
	// listener was not yet shut (the report counts the exit before the worker
	// leaves the group) and a re-probe is answered once it is gone.
	for dl := time.Now().Add(2 * time.Second); time.Now().Before(dl); time.Sleep(20 * time.Millisecond) {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			if errors.Is(err, syscall.ECONNREFUSED) {
				return // no loop left, and no listener of the dead one
			}
			t.Errorf("a fresh conn after the fault failed with %v, want it answered or refused", err)
			return
		}
		_ = c.Close()
		if a, n := h2Probe848(addr, 10, 500*time.Millisecond, true); a == n {
			return
		}
	}
	t.Errorf("%d of %d fresh conns after the fault were answered, and the rest connected to a listener that answers nothing: "+
		"the dead loop's listener is still in the SO_REUSEPORT group (the Goexit items from celeris#848 on the checklists celeris#884 and celeris#885)", answered, tried)
}

func h2Class848(ended bool, err error) string {
	if ended {
		return "ended"
	}
	return goexitClass791(err)
}

func errorRecords848(c *capture848) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.recs {
		if r.level >= slog.LevelError {
			n++
		}
	}
	return n
}

func TestH2InlineStreamGoexitIsLoud848(t *testing.T) {
	for _, a := range arms848()[:2] { // epoll, io_uring (adaptive runs the same loops)
		t.Run(a.name, func(t *testing.T) {
			if !child848(t) {
				return
			}
			need848(t, a.eng)
			r := startH2848(t, a.eng)
			okBefore, _, errB := h2StreamEnds837(r.addr, "/ok", 2*time.Second)
			ended, _, err := h2StreamEnds837(r.addr, "/boom", 2*time.Second)
			var recs []rec848
			for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); time.Sleep(10 * time.Millisecond) {
				if recs = r.logs.exits(); len(recs) > 0 {
					break
				}
			}
			time.Sleep(300 * time.Millisecond) // a second record, if the defect doubled it
			recs = r.logs.exits()
			answered, tried := h2Probe848(r.addr, 20, 500*time.Millisecond, true)
			stopped := r.stopWithin(loopExitStop848)
			after := r.logs.exits()
			t.Logf("celeris848 RESULT H2-INLINE arm=%s hits=%d ok_before=%s boom=%s records=%d records_after_stop=%d probes_until_unanswered=%d/20 stopped=%v",
				a.name, r.hits.Load(), h2Class848(okBefore, errB), h2Class848(ended, err), len(recs), len(after), tried, stopped)
			if !okBefore {
				t.Fatalf("PREMISE: /ok before the fault was not answered: %s", h2Class848(okBefore, errB))
			}
			if r.hits.Load() != 1 {
				t.Fatalf("INJECTION: /boom ran %d times, want 1", r.hits.Load())
			}
			if len(recs) != 1 {
				t.Fatalf("celeris#848: an inline h2 stream's Goexit produced %d %q records, want exactly 1", len(recs), loopExitMsg848)
			}
			if got := attrStr848(recs[0].attrs["engine"]); !slices.Contains(a.engine, got) {
				t.Errorf("engine attr = %q, want one of %v", got, a.engine)
			}
			freshAfterFault848(t, r.addr, answered, tried)
			if !stopped {
				t.Errorf("the server did not stop within %v of cancel after the fault", loopExitStop848)
			}
			if len(after) != 1 {
				t.Errorf("after the stop there are %d records, want still 1", len(after))
			}
		})
	}
}

func TestH2AsyncStreamGoexitKeepsTheLoop848(t *testing.T) {
	for _, a := range arms848()[:2] {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH2848(t, a.eng)
			ended, _, err := h2StreamEnds837(r.addr, "/aboom", 2*time.Second)
			time.Sleep(300 * time.Millisecond)
			answered, tried := h2Probe848(r.addr, 20, time.Second, false)
			errs := errorRecords848(r.logs)
			stopped := r.stopWithin(loopExitStop848)
			exits := len(r.logs.exits())
			t.Logf("celeris848 RESULT H2-ASYNC arm=%s hits=%d aboom=%s answered_after=%d/%d error_records=%d loop_exit_records=%d stopped=%v",
				a.name, r.hits.Load(), h2Class848(ended, err), answered, tried, errs, exits, stopped)
			if r.hits.Load() != 1 {
				t.Fatalf("INJECTION: /aboom ran %d times, want 1", r.hits.Load())
			}
			if ended {
				t.Errorf("PREMISE: the stream whose handler Goexited was answered")
			}
			if err == nil || !strings.Contains(err.Error(), "stream reset: INTERNAL_ERROR") {
				t.Errorf("/aboom: err %v, want its stream reset with INTERNAL_ERROR (celeris#889), not left to the client's timeout", err)
			}
			if errs != 1 {
				t.Errorf("%d error records for one Goexit, want 1 (the processor's, with the stack)", errs)
			}
			if answered != tried {
				t.Errorf("an async h2 route's Goexit ended a loop: %d of %d fresh conns were answered afterwards", answered, tried)
			}
			if exits != 0 {
				t.Errorf("an async h2 stream's Goexit produced %d loop-exit records, want 0 (the loop did not exit)", exits)
			}
			if !stopped {
				t.Errorf("the server did not stop within %v of cancel", loopExitStop848)
			}
		})
	}
}

// fill848 is the size of /fill's response: over OutboundBudget (4 MiB) plus
// the 65,535-byte connection window the peer grants, so a client that grants
// no WINDOW_UPDATE leaves the connection holding more than the budget.
const fill848 = 5 << 20

var fillBody848 = body893(fill848)

// h2shape848 is one way a client can send a request on HTTP/2.
type h2shape848 struct {
	name   string
	method string
	path   string
	// frames
	split    bool   // the header block is split: HEADERS without END_HEADERS, then CONTINUATION
	body     []byte // DATA frames; nil = none
	trailers bool   // the request ends with a trailer HEADERS frame
	hdrEnd   bool   // END_STREAM on the HEADERS frame (a request without a body)
}

// send writes the request as stream id on fw, with the header block encoded
// by enc into hb.
func (sh h2shape848) send(fr *http2.Framer, enc *hpack.Encoder, hb *bytes.Buffer, id uint32, addr string) error {
	hb.Reset()
	for _, f := range [][2]string{{":method", sh.method}, {":scheme", "http"}, {":authority", addr}, {":path", sh.path}} {
		if err := enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]}); err != nil {
			return err
		}
	}
	block := slices.Clone(hb.Bytes())
	if sh.split {
		half := len(block) / 2
		if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: block[:half], EndStream: sh.hdrEnd, EndHeaders: false}); err != nil {
			return err
		}
		if err := fr.WriteContinuation(id, true, block[half:]); err != nil {
			return err
		}
	} else if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: block, EndStream: sh.hdrEnd, EndHeaders: true}); err != nil {
		return err
	}
	if sh.hdrEnd {
		return nil
	}
	if sh.trailers {
		if err := fr.WriteData(id, false, sh.body); err != nil {
			return err
		}
		hb.Reset()
		if err := enc.WriteField(hpack.HeaderField{Name: "x-trailer", Value: "t"}); err != nil {
			return err
		}
		return fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true})
	}
	return fr.WriteData(id, true, sh.body)
}

// h2OnOneConn848 opens one h2c connection, sends stream 1 as sh and waits d
// for stream 1 to end, then sends stream 3 (GET /ok) on the SAME connection
// and waits d for it. Each result is "ended", "timeout", "rst:<code>",
// "goaway:<code>" or "err:<text>".
func h2OnOneConn848(addr string, sh h2shape848, d time.Duration) (s1, s3 string) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return "err:" + err.Error(), ""
	}
	defer func() { _ = conn.Close() }()
	_, _ = conn.Write([]byte(http2.ClientPreface))
	fr := http2.NewFramer(conn, conn)
	_ = fr.WriteSettings()
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	wait := func(id uint32) string {
		_ = conn.SetReadDeadline(time.Now().Add(d))
		for {
			f, err := fr.ReadFrame()
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					return "timeout"
				}
				return "err:" + err.Error()
			}
			switch f := f.(type) {
			case *http2.SettingsFrame:
				if !f.IsAck() {
					_ = fr.WriteSettingsAck()
				}
			case *http2.HeadersFrame:
				if f.StreamID == id && f.StreamEnded() {
					return "ended"
				}
			case *http2.DataFrame:
				if f.StreamID == id && f.StreamEnded() {
					return "ended"
				}
			case *http2.RSTStreamFrame:
				if f.StreamID == id {
					return "rst:" + f.ErrCode.String()
				}
			case *http2.GoAwayFrame:
				return "goaway:" + f.ErrCode.String()
			}
		}
	}
	if err := sh.send(fr, enc, &hb, 1, addr); err != nil {
		return "err:" + err.Error(), ""
	}
	s1 = wait(1)
	ok := h2shape848{method: "GET", path: "/ok", hdrEnd: true}
	if err := ok.send(fr, enc, &hb, 3, addr); err != nil {
		return s1, "err:" + err.Error()
	}
	return s1, wait(3)
}

var inlineShapes848 = []h2shape848{
	{name: "post-body", method: "POST", path: "/pboom", body: []byte("hello body")},
	{name: "post-body-continuation", method: "POST", path: "/pboom", body: []byte("hello body"), split: true},
	{name: "get-continuation", method: "GET", path: "/boom", hdrEnd: true, split: true},
	{name: "post-body-trailers", method: "POST", path: "/pboom", body: []byte("hello body"), trailers: true},
}

// TestH2InlineShapesGoexitAreLoud848: a sync route's stream runs on the event
// loop whatever its request carries, so its Goexit ends the loop and is
// reported, as a GET's does (TestH2InlineStreamGoexitIsLoud848). The stream
// and the next stream of the same connection get no answer.
func TestH2InlineShapesGoexitAreLoud848(t *testing.T) {
	for _, sh := range inlineShapes848 {
		t.Run(sh.name, func(t *testing.T) {
			for _, a := range arms848()[:2] {
				t.Run(a.name, func(t *testing.T) {
					if !child848(t) {
						return
					}
					need848(t, a.eng)
					r := startH2848(t, a.eng)
					okBefore, _, errB := h2StreamEnds837(r.addr, "/ok", 2*time.Second)
					s1, s3 := h2OnOneConn848(r.addr, sh, 2*time.Second)
					var recs []rec848
					for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); time.Sleep(10 * time.Millisecond) {
						if recs = r.logs.exits(); len(recs) > 0 {
							break
						}
					}
					time.Sleep(300 * time.Millisecond) // a second record, if the defect doubled it
					recs = r.logs.exits()
					answered, tried := h2Probe848(r.addr, 20, 500*time.Millisecond, true)
					stopped := r.stopWithin(loopExitStop848)
					after := r.logs.exits()
					t.Logf("celeris848 RESULT H2-SHAPE shape=%s arm=%s hits=%d ok_before=%s stream1=%s stream3_same_conn=%s records=%d records_after_stop=%d probes_until_unanswered=%d/20 stopped=%v",
						sh.name, a.name, r.hits.Load(), h2Class848(okBefore, errB), s1, s3, len(recs), len(after), tried, stopped)
					if !okBefore {
						t.Fatalf("PREMISE: /ok before the fault was not answered: %s", h2Class848(okBefore, errB))
					}
					if r.hits.Load() != 1 {
						t.Fatalf("INJECTION: the handler ran %d times, want 1", r.hits.Load())
					}
					if len(recs) != 1 {
						t.Fatalf("celeris#848: a %s stream's Goexit produced %d %q records, want exactly 1 (it ran off the loop)", sh.name, len(recs), loopExitMsg848)
					}
					if got := attrStr848(recs[0].attrs["engine"]); !slices.Contains(a.engine, got) {
						t.Errorf("engine attr = %q, want one of %v", got, a.engine)
					}
					if s1 != "timeout" {
						t.Errorf("stream 1 = %s, want timeout (the stream is reset, but the loop that would send it ended)", s1)
					}
					if s3 != "timeout" && s3 != "err:EOF" {
						t.Errorf("stream 3 on the same connection = %s: the loop is dead, so it must not be answered", s3)
					}
					freshAfterFault848(t, r.addr, answered, tried)
					if !stopped {
						t.Errorf("the server did not stop within %v of cancel after the fault", loopExitStop848)
					}
					if len(after) != 1 {
						t.Errorf("after the stop there are %d records, want still 1", len(after))
					}
				})
			}
		})
	}
}

// TestH2AsyncPostGoexitKeepsTheLoop848: an async route's stream with a request
// body runs on the h2 worker pool: the loop and the connection keep serving.
func TestH2AsyncPostGoexitKeepsTheLoop848(t *testing.T) {
	for _, a := range arms848()[:2] {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH2848(t, a.eng)
			s1, s3 := h2OnOneConn848(r.addr, h2shape848{method: "POST", path: "/apboom", body: []byte("hello body")}, 2*time.Second)
			time.Sleep(300 * time.Millisecond)
			answered, tried := h2Probe848(r.addr, 20, time.Second, false)
			stopped := r.stopWithin(loopExitStop848)
			exits := len(r.logs.exits())
			t.Logf("celeris848 RESULT H2-ASYNC-POST arm=%s hits=%d stream1=%s stream3_same_conn=%s answered_after=%d/%d loop_exit_records=%d stopped=%v",
				a.name, r.hits.Load(), s1, s3, answered, tried, exits, stopped)
			if r.hits.Load() != 1 {
				t.Fatalf("INJECTION: /apboom ran %d times, want 1", r.hits.Load())
			}
			if s1 != "rst:INTERNAL_ERROR" {
				t.Errorf("stream 1 = %s, want rst:INTERNAL_ERROR (celeris#889)", s1)
			}
			if s3 != "ended" {
				t.Errorf("stream 3 on the same connection = %s, want ended: the connection keeps serving its other streams", s3)
			}
			if answered != tried {
				t.Errorf("an async h2 POST's Goexit ended a loop: %d of %d fresh conns were answered afterwards", answered, tried)
			}
			if exits != 0 {
				t.Errorf("an async h2 stream's Goexit produced %d loop-exit records, want 0", exits)
			}
			if !stopped {
				t.Errorf("the server did not stop within %v of cancel", loopExitStop848)
			}
		})
	}
}

// TestH2OverBudgetStreamGoexitKeepsTheLoop848: a sync route's stream on a
// connection that holds OutboundBudget bytes of response data runs on the h2
// worker pool (Processor.runHandler), so the loop survives its Goexit, the
// stream is reset and the Goexit logged once (celeris#889). The client does not read: /fill
// (a 5 MiB response, window 65,535) leaves the connection over the budget;
// then /boom is sent on the same connection. If the budget had not been
// reached, /boom would run on the loop and the loop-exit check below fails.
func TestH2OverBudgetStreamGoexitKeepsTheLoop848(t *testing.T) {
	for _, a := range arms848()[:2] {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH2848(t, a.eng)
			c := dialH2Client893(t, r.addr)
			c.get(t, 1, "/fill", fillBody848)
			if !c.waitFor(10*time.Second, func() bool { return c.data[1] > 0 }) {
				t.Fatalf("PREMISE: no DATA of /fill within 10 s")
			}
			time.Sleep(500 * time.Millisecond) // the rest is buffered by now
			c.get(t, 3, "/boom", nil)
			for dl := time.Now().Add(5 * time.Second); r.hits.Load() == 0 && time.Now().Before(dl); time.Sleep(10 * time.Millisecond) {
			}
			time.Sleep(300 * time.Millisecond)
			exitsEarly := len(r.logs.exits())
			// The connection is served by a live loop: grant the windows, and
			// /fill arrives whole, then a new stream is answered.
			c.replenish.Store(true)
			c.credit(t, 0, 8<<20)
			c.credit(t, 1, 8<<20)
			fillDone := c.waitFor(30*time.Second, func() bool { return c.ended[1] && c.data[1] == fill848 })
			c.get(t, 5, "/ok", nil)
			okDone := c.waitFor(5*time.Second, func() bool { return c.ended[5] })
			c.mu.Lock()
			boomEnded := c.ended[3]
			boomRst, boomReset := c.resets[3]
			c.mu.Unlock()
			answered, tried := h2Probe848(r.addr, 20, time.Second, false)
			errs := errorRecords848(r.logs)
			stopped := r.stopWithin(loopExitStop848)
			exits := len(r.logs.exits())
			t.Logf("celeris848 RESULT H2-OVER-BUDGET arm=%s hits=%d loop_exit_records_before_credit=%d fill_complete=%v ok_same_conn=%v boom_ended=%v boom_reset=%v fresh_answered=%d/%d error_records=%d loop_exit_records=%d stopped=%v",
				a.name, r.hits.Load(), exitsEarly, fillDone, okDone, boomEnded, boomReset, answered, tried, errs, exits, stopped)
			if r.hits.Load() != 1 {
				t.Fatalf("INJECTION: /boom ran %d times, want 1", r.hits.Load())
			}
			if exitsEarly != 0 || exits != 0 {
				t.Fatalf("celeris#848: /boom produced loop-exit records (%d, then %d): it ran on the loop, so the connection was not over its outbound budget", exitsEarly, exits)
			}
			if boomEnded || !boomReset || boomRst != http2.ErrCodeInternal {
				t.Errorf("stream 3: ended=%v reset=%v (%v), want no response and a RST_STREAM(INTERNAL_ERROR) (celeris#889)", boomEnded, boomReset, boomRst)
			}
			if errs != 1 {
				t.Errorf("%d error records for one Goexit, want 1", errs)
			}
			if !fillDone {
				t.Errorf("the buffered /fill response did not arrive whole after the windows were granted: the connection is not served")
			}
			if !okDone {
				t.Errorf("a new stream on the same connection was not answered")
			}
			if answered != tried {
				t.Errorf("%d of %d fresh conns were answered: a loop died", answered, tried)
			}
			if !stopped {
				t.Errorf("the server did not stop within %v of cancel", loopExitStop848)
			}
		})
	}
}
