package stream

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"golang.org/x/net/http2"
)

// celeris#889: the processor's hooks and counters for a handler that is over
// without having finished its response. What reaches the wire is pinned in
// internal/conn (h2_handler_return_889_test.go), where a real write queue is.

type goexitHandler889 struct{ async bool }

func (h goexitHandler889) HandleStream(context.Context, *Stream) error {
	runtime.Goexit()
	return nil
}
func (h goexitHandler889) RouteAsync(_, _ string) bool { return h.async }
func (h goexitHandler889) HasAsyncRoutes() bool        { return h.async }

type goexitLog889 struct {
	mu     sync.Mutex
	ids    []uint32
	stacks []string
}

func (l *goexitLog889) hook(id uint32, stack []byte) {
	l.mu.Lock()
	l.ids = append(l.ids, id)
	l.stacks = append(l.stacks, string(stack))
	l.mu.Unlock()
}

// A Goexit reaches OnHandlerGoexit with the stream and a stack through the
// handler, is counted (without a hook too), and the stream is reset: through
// the connection writer on the event-loop path, and on the worker pool only
// cancelled when the writer cannot queue the reset (Resetter).
func TestHandlerGoexitIsReportedCountedAndReset889(t *testing.T) {
	for _, mode := range []string{"inline", "pool"} {
		t.Run(mode, func(t *testing.T) {
			var log goexitLog889
			conn := &rstConn944{testResponseWriter: newTestResponseWriter()}
			p := NewProcessor(goexitHandler889{async: mode == "pool"}, newTestFrameWriter(), conn)
			p.OnHandlerGoexit = log.hook
			e := newEnc981()
			for _, id := range []uint32{1, 3} {
				if mode == "pool" {
					if err := p.ProcessRawHeaders(id, true, e.req()); err != nil {
						t.Fatal(err)
					}
					waitPool889(t, p)
					continue
				}
				// A Goexit in an inline handler ends the goroutine that runs
				// the processor: give it one of its own.
				done := make(chan struct{})
				go func() {
					defer close(done)
					_ = p.ProcessRawHeaders(id, true, e.req())
				}()
				<-done
			}
			if got := p.HandlerGoexits(); got != 2 {
				t.Errorf("HandlerGoexits = %d, want 2", got)
			}
			if got := p.HandlerPanics(); got != 0 {
				t.Errorf("HandlerPanics = %d, a Goexit is not a panic", got)
			}
			log.mu.Lock()
			ids, stacks := append([]uint32(nil), log.ids...), append([]string(nil), log.stacks...)
			log.mu.Unlock()
			if len(ids) != 2 || ids[0] != 1 || ids[1] != 3 {
				t.Fatalf("hook told of streams %v, want [1 3]", ids)
			}
			for i, st := range stacks {
				if !strings.Contains(st, "goexitHandler889") || !strings.Contains(st, "Goexit") {
					t.Errorf("record %d: the stack does not show the Goexit and the handler:\n%s", i, st)
				}
			}
			rst := conn.resets()
			if mode == "pool" {
				// The connection's writer belongs to the event loop: a pool
				// goroutine does not write to it. (The real writers are
				// Resetters, which queue the reset; this recorder is not.)
				if len(rst) != 0 {
					t.Errorf("a pool goroutine wrote %v to the connection's writer", rst)
				}
			} else if len(rst) != 2 || rst[0].streamID != 1 || rst[1].streamID != 3 || rst[0].code != http2.ErrCodeInternal {
				t.Errorf("RST_STREAMs %v, want INTERNAL_ERROR for 1 and 3", rst)
			}
			if n := p.manager.CountActiveStreams(); n != 0 {
				t.Errorf("%d streams still count against the limit after the Goexits", n)
			}
		})
	}
}

func TestHandlerGoexitCountsWithoutAHookAndSurvivesABadHook889(t *testing.T) {
	p := NewProcessor(goexitHandler889{async: true}, newTestFrameWriter(), &rstConn944{testResponseWriter: newTestResponseWriter()})
	e := newEnc981()
	if err := p.ProcessRawHeaders(1, true, e.req()); err != nil {
		t.Fatal(err)
	}
	waitPool889(t, p)
	if got := p.HandlerGoexits(); got != 1 {
		t.Errorf("HandlerGoexits without a hook = %d, want 1", got)
	}
	p.OnHandlerGoexit = func(uint32, []byte) { panic("the hook panics too") }
	if err := p.ProcessRawHeaders(3, true, e.req()); err != nil {
		t.Fatal(err)
	}
	waitPool889(t, p)
	if got := p.HandlerGoexits(); got != 2 {
		t.Errorf("HandlerGoexits = %d, want 2", got)
	}
}

// An error return with nothing sent reaches OnHandlerUnfinished as a reset
// with the error, and is counted.
func TestHandlerErrorReturnIsReportedAsUnfinished889(t *testing.T) {
	boom := errors.New("boom889")
	type rec struct {
		id                uint32
		reset             bool
		written, declared int64
		err               error
	}
	var mu sync.Mutex
	var recs []rec
	var runs atomic.Int32
	conn := &rstConn944{testResponseWriter: newTestResponseWriter()}
	p := NewProcessor(HandlerFunc(func(context.Context, *Stream) error { runs.Add(1); return boom }), newTestFrameWriter(), conn)
	p.OnHandlerUnfinished = func(id uint32, reset bool, written, declared int64, err error) {
		mu.Lock()
		recs = append(recs, rec{id, reset, written, declared, err})
		mu.Unlock()
	}
	e := newEnc981()
	if err := p.ProcessRawHeaders(1, true, e.req()); err != nil {
		t.Fatal(err)
	}
	p.FlushInlineCleanup()
	mu.Lock()
	defer mu.Unlock()
	if len(recs) != 1 || recs[0].id != 1 || !recs[0].reset || recs[0].declared != -1 || !errors.Is(recs[0].err, boom) {
		t.Fatalf("OnHandlerUnfinished records %+v, want one reset of stream 1 with the error", recs)
	}
	if got := p.UnfinishedResponses(); got != 1 {
		t.Errorf("UnfinishedResponses = %d, want 1", got)
	}
	if r := conn.resets(); len(r) != 1 || r[0].code != http2.ErrCodeInternal {
		t.Errorf("RST_STREAMs %v, want one INTERNAL_ERROR", r)
	}
}

// A panic or a Goexit is told to its own hook only: it is not also counted as
// an unfinished response, so one fault is one record.
func TestPanicAndGoexitAreNotCountedAsUnfinished889(t *testing.T) {
	for _, h := range []Handler{panicHandler889{async: true}, goexitHandler889{async: true}} {
		p := NewProcessor(h, newTestFrameWriter(), &rstConn944{testResponseWriter: newTestResponseWriter()})
		var told atomic.Int32
		p.OnHandlerUnfinished = func(uint32, bool, int64, int64, error) { told.Add(1) }
		if err := p.ProcessRawHeaders(1, true, newEnc981().req()); err != nil {
			t.Fatal(err)
		}
		waitPool889(t, p)
		if told.Load() != 0 || p.UnfinishedResponses() != 0 {
			t.Errorf("%T: OnHandlerUnfinished told %d, UnfinishedResponses %d, want 0", h, told.Load(), p.UnfinishedResponses())
		}
	}
}

// celeris#889 added two fields to Stream. A Stream was exactly 1024 bytes, the
// edge of a malloc size class; one byte more is 1152, 12% more memory for every
// stream of every connection. The one-byte flags were regrouped to make room.
func TestStreamStaysInItsSizeClass889(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the size class is a 64-bit one")
	}
	if got := unsafe.Sizeof(Stream{}); got > 1024 {
		t.Errorf("sizeof(Stream) = %d, want at most 1024 (the malloc size class it was in)", got)
	}
}
