package stream

import (
	"context"
	"sync"
	"testing"

	"golang.org/x/net/http2"
)

// celeris#981, review round 1.

// A request whose header block ends in CONTINUATION is served after
// continuationStateMu is released: canRunInline picks the event loop for it
// (continuationActive is already false), so a handler runs on the goroutine
// that called ProcessFrame, and it must not run inside the lock's critical
// section.
func TestSplitHeadersHandlerRunsOutsideContinuationLock981(t *testing.T) {
	for _, path := range []string{"frame", "continuation"} {
		t.Run(path, func(t *testing.T) {
			var (
				p        *Processor
				mu       sync.Mutex
				runs     int
				lockFree bool
			)
			h := HandlerFunc(func(_ context.Context, _ *Stream) error {
				mu.Lock()
				defer mu.Unlock()
				runs++
				if p.continuationStateMu.TryLock() {
					p.continuationStateMu.Unlock()
					lockFree = true
				}
				return nil
			})
			p = NewProcessor(h, newTestFrameWriter(), &rstConn944{testResponseWriter: newTestResponseWriter()})
			p.manager.SetLocalMaxFrameSize(1 << 20)
			e := newEnc981()
			if err := feed981(t, p, path, hdr981(1, true, e.req())); err != nil {
				t.Fatal(err)
			}
			p.FlushInlineCleanup()
			mu.Lock()
			defer mu.Unlock()
			if runs != 1 {
				t.Fatalf("handler ran %d times, want 1", runs)
			}
			if !lockFree {
				t.Error("the handler ran with continuationStateMu held")
			}
		})
	}
}

// The PRIORITY field of the HEADERS frame that starts a split block reaches
// the priority tree, as the field of a one-frame HEADERS does.
func TestSplitHeadersPriorityFieldReachesTree981(t *testing.T) {
	for _, path := range []string{"frame", "continuation"} {
		t.Run(path, func(t *testing.T) {
			p := NewProcessor(HandlerFunc(func(context.Context, *Stream) error { return nil }),
				newTestFrameWriter(), &rstConn944{testResponseWriter: newTestResponseWriter()})
			p.manager.SetLocalMaxFrameSize(1 << 20)
			e := newEnc981()
			// The blocks are built in the order they are sent (one HPACK encoder).
			first := hdr981(1, false, e.req())
			st := hdr981(3, false, e.req())
			st.prio = &http2.PriorityParam{StreamDep: 1, Weight: 200, Exclusive: true}
			if err := feed981(t, p, path, first); err != nil {
				t.Fatal(err)
			}
			if err := feed981(t, p, path, st); err != nil {
				t.Fatal(err)
			}
			pr, ok := p.manager.priorityTree.GetPriority(3)
			if !ok {
				t.Fatal("stream 3 has no entry in the priority tree")
			}
			if pr.StreamDependency != 1 || pr.Weight != 200 || !pr.Exclusive {
				t.Errorf("priority of stream 3 = dependency %d weight %d exclusive %v, want 1, 200, true",
					pr.StreamDependency, pr.Weight, pr.Exclusive)
			}
		})
	}
}

// A refused stream does not allocate for its decode target: refuseStream
// decodes into a field of the processor, not into a slice that escapes (a
// refused stream is the peer's to repeat at wire speed). The block is three
// static-table fields, so x/net builds no dynamic-table string.
func TestRefusedBlockAllocs981(t *testing.T) {
	fw := newTestFrameWriter()
	p := NewProcessor(HandlerFunc(func(context.Context, *Stream) error { return nil }), fw,
		&rstConn944{testResponseWriter: newTestResponseWriter()})
	p.manager.SetMaxConcurrentStreams(1)
	block := []byte{0x82, 0x86, 0x84} // :method GET, :scheme http, :path /
	if err := p.ProcessRawHeaders(1, false, block); err != nil {
		t.Fatal(err)
	}
	id := uint32(3)
	refuse := func() {
		if err := p.ProcessRawHeaders(id, true, block); err != nil {
			t.Fatal(err)
		}
		id += 2
	}
	refuse() // warm up: the decoder and the writer's first allocations
	fw.mu.Lock()
	fw.rstStreamsSent = nil
	fw.mu.Unlock()
	n := testing.AllocsPerRun(100, refuse)
	t.Logf("allocations per refused stream: %v", n)
	if n > 0 {
		t.Errorf("%v allocations per refused stream, want 0", n)
	}
}
