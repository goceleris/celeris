package stream

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// celeris#889 item 31: a panic that escapes a stream's handler to the
// last-resort recover of executeHandlerInline or executeHandler was dropped
// without a trace (`_ = r`). The stream is still given up and the connection
// goes on, as before; the processor now says so to OnHandlerPanic and counts it.

// panicHandler889 panics with a marker value. async makes it a pool handler.
type panicHandler889 struct{ async bool }

func (h panicHandler889) HandleStream(context.Context, *Stream) error { panic("boom889") }
func (h panicHandler889) RouteAsync(_, _ string) bool                 { return h.async }
func (h panicHandler889) HasAsyncRoutes() bool                        { return h.async }

type panicRecord889 struct {
	id    uint32
	value any
	stack string
}

type panicLog889 struct {
	mu   sync.Mutex
	recs []panicRecord889
}

func (l *panicLog889) hook(id uint32, r any, stack []byte) {
	l.mu.Lock()
	l.recs = append(l.recs, panicRecord889{id, r, string(stack)})
	l.mu.Unlock()
}

func (l *panicLog889) records() []panicRecord889 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]panicRecord889(nil), l.recs...)
}

func waitPool889(t *testing.T, p *Processor) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); p.PoolHandlersRunning() && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if p.PoolHandlersRunning() {
		t.Fatal("a pool handler is still running")
	}
}

// A panic that escapes the handler reaches the hook with the stream, the
// value and a stack that shows the panic's origin, on the event-loop path
// (inline) and on the worker pool; the stream is retired as before and the
// next request is served.
func TestEscapedHandlerPanicIsReported889(t *testing.T) {
	for _, mode := range []string{"inline", "pool"} {
		t.Run(mode, func(t *testing.T) {
			var log panicLog889
			p := NewProcessor(panicHandler889{async: mode == "pool"}, newTestFrameWriter(),
				&rstConn944{testResponseWriter: newTestResponseWriter()})
			p.OnHandlerPanic = log.hook
			e := newEnc981()
			for i, id := range []uint32{1, 3} {
				if err := p.ProcessRawHeaders(id, true, e.req()); err != nil {
					t.Fatalf("request %d: %v", i+1, err)
				}
				waitPool889(t, p)
				p.FlushInlineCleanup()
			}
			recs := log.records()
			if len(recs) != 2 {
				t.Fatalf("hook told of %d panics, want 2", len(recs))
			}
			for i, want := range []uint32{1, 3} {
				r := recs[i]
				if r.id != want || r.value != "boom889" {
					t.Errorf("record %d = stream %d value %v, want stream %d value boom889", i, r.id, r.value, want)
				}
				if !strings.Contains(r.stack, "panicHandler889") {
					t.Errorf("record %d: the stack does not show the handler that panicked:\n%s", i, r.stack)
				}
			}
			if got := p.HandlerPanics(); got != 2 {
				t.Errorf("HandlerPanics = %d, want 2", got)
			}
			if n := p.manager.CountActiveStreams(); n != 0 {
				t.Errorf("%d streams still count against the limit after the panics", n)
			}
		})
	}
}

// The count does not need a hook, and a hook that panics itself does not end
// the loop: the recover that runs it has nothing above it.
func TestHandlerPanicCountsWithoutAHookAndSurvivesABadHook889(t *testing.T) {
	p := NewProcessor(panicHandler889{}, newTestFrameWriter(), &rstConn944{testResponseWriter: newTestResponseWriter()})
	e := newEnc981()
	if err := p.ProcessRawHeaders(1, true, e.req()); err != nil {
		t.Fatal(err)
	}
	p.FlushInlineCleanup()
	if got := p.HandlerPanics(); got != 1 {
		t.Errorf("HandlerPanics without a hook = %d, want 1", got)
	}
	p.OnHandlerPanic = func(uint32, any, []byte) { panic("the hook panics too") }
	if err := p.ProcessRawHeaders(3, true, e.req()); err != nil {
		t.Fatalf("a panicking hook reached the caller: %v", err)
	}
	p.FlushInlineCleanup()
	if got := p.HandlerPanics(); got != 2 {
		t.Errorf("HandlerPanics = %d, want 2", got)
	}
}
