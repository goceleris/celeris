package stream

import (
	"bytes"
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
	// AllocsPerRun integer-divides the amortised growth of an append, so a
	// refusal that built its fields (the decoder's emit left on) reads 0 above
	// while the slice grows for the life of the connection (celeris#889). The
	// decode target is what is checked: nothing was appended to it, and no
	// backing array was ever allocated.
	if got, c := len(p.refusedFields), cap(p.refusedFields); got != 0 || c != 0 {
		t.Errorf("refusedFields has len %d, cap %d after the refused blocks, want 0 and 0: a refused block is decoded with emit off", got, c)
	}
}

// celeris#889 item 28: the GOAWAY of a header block that does not decode
// names the last stream the server opened, as the comments of refuseStream
// and completeHeaderBlock say. An idle PRIORITY placeholder is no stream the
// server opened, so after stream 1, PRIORITY on 2001 and an undecodable block
// on 3, the GOAWAY does not name 2001. There are two ways a block can fail:
// "admitted" opens stream 3 and fails in completeHeaderBlock (the GOAWAY
// names 3, the last stream the server opened), "refused" has no room under
// SETTINGS_MAX_CONCURRENT_STREAMS and fails in refuseStream (stream 3 was
// never opened: the GOAWAY names 1). TestPlaceholderIsNotTheLastStream889
// pins the refused block on the raw path only.
func TestDecodeFailureGoAwayIgnoresPriorityPlaceholder889(t *testing.T) {
	undecodable := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f, 0x80}
	for _, mode := range []string{"admitted", "refused"} {
		for _, path := range []string{"raw", "frame", "continuation"} {
			t.Run(mode+"/"+path, func(t *testing.T) {
				p := newProc944()
				p.manager.SetLocalMaxFrameSize(1 << 20)
				e := newEnc981()
				if mode == "refused" {
					p.manager.SetMaxConcurrentStreams(1)
				}
				// Stream 1 is open (refused mode: it holds the only slot) or
				// served (admitted mode).
				if err := feed981(t, p.Processor, path, hdr981(1, mode == "admitted", e.req())); err != nil {
					t.Fatal(err)
				}
				prio := &http2.PriorityParam{StreamDep: 0, Weight: 15}
				if err := feed981(t, p.Processor, path, step981{priority: true, id: 2001, prio: prio}); err != nil {
					t.Fatal(err)
				}
				if _, ok := p.manager.GetStream(2001); !ok {
					t.Fatal("self-check: PRIORITY on 2001 left no placeholder")
				}
				if got := p.manager.GetLastStreamID(); got != 1 {
					t.Fatalf("self-check: the last stream the server opened is %d, want 1", got)
				}
				err := feed981(t, p.Processor, path, hdr981(3, true, undecodable))
				if err == nil {
					t.Fatal("a header block that does not decode was accepted")
				}
				goaways, _ := sinks889(p.Processor)
				if len(goaways) != 1 || goaways[0].code != http2.ErrCodeCompression {
					t.Fatalf("GOAWAYs = %+v, want one COMPRESSION_ERROR", goaways)
				}
				// An admitted block opened its stream (3) before it failed to
				// decode, so that one is named; a refused one never opened.
				// Either way the placeholder on 2001 is not.
				want := uint32(1)
				if mode == "admitted" {
					want = 3
				}
				if goaways[0].lastStreamID != want {
					t.Errorf("GOAWAY names last stream %d, want %d: the PRIORITY placeholder on 2001 is no stream the server opened", goaways[0].lastStreamID, want)
				}
			})
		}
	}
}

// celeris#889 item 29: a split HEADERS frame that carries the PRIORITY flag
// costs no allocation more than one without it. ContinuationState used to
// hold the field as a pointer, which escaped to the heap for every block that
// had one. The comparison is on the allocations of the first fragment (the
// state that is built and the copy of the fragment are the same for both);
// the field's way into the priority tree is TestSplitHeadersPriorityFieldReachesTree981.
func TestSplitHeadersPriorityFlagAddsNoAllocation889(t *testing.T) {
	block := newEnc981().req()
	allocs := func(withPriority bool) float64 {
		var wire bytes.Buffer
		param := http2.HeadersFrameParam{StreamID: 1, BlockFragment: block[:len(block)/2], EndStream: true}
		if withPriority {
			param.Priority = http2.PriorityParam{StreamDep: 0, Weight: 15}
		}
		if err := http2.NewFramer(&wire, nil).WriteHeaders(param); err != nil {
			t.Fatal(err)
		}
		raw := wire.Bytes()
		if flags := raw[4]&0x20 != 0; flags != withPriority {
			t.Fatalf("self-check: the PRIORITY flag is %v, want %v", flags, withPriority)
		}
		p := NewProcessor(HandlerFunc(func(context.Context, *Stream) error { return nil }),
			newTestFrameWriter(), &rstConn944{testResponseWriter: newTestResponseWriter()})
		rd := bytes.NewReader(raw)
		fr := http2.NewFramer(nil, rd)
		fr.SetReuseFrames()
		fr.SetMaxReadFrameSize(1 << 20)
		fr.AllowIllegalReads = true // the same HEADERS again, with no CONTINUATION between
		step := func() {
			rd.Reset(raw)
			f, err := fr.ReadFrame()
			if err != nil {
				t.Fatal(err)
			}
			if err := p.ProcessFrame(context.Background(), f); err != nil {
				t.Fatal(err)
			}
			// The block is never finished: drop it, as the next frame would.
			p.continuationStateMu.Lock()
			if p.continuationState == nil {
				t.Error("no pending header block after the first fragment")
			}
			p.continuationState = nil
			p.continuationActive.Store(false)
			p.continuationStateMu.Unlock()
		}
		step() // warm up
		return testing.AllocsPerRun(200, step)
	}
	without, with := allocs(false), allocs(true)
	t.Logf("allocations per first fragment: %v without the PRIORITY flag, %v with it", without, with)
	if with != without {
		t.Errorf("a split HEADERS frame with the PRIORITY flag allocates %v times, one without it %v", with, without)
	}
}
