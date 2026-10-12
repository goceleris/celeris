package stream

import (
	"bytes"
	"context"
	"testing"

	"golang.org/x/net/http2"
)

// celeris#889, review round 1 of PR #1005: the decided numbers and their
// boundaries. The behaviour tests of refusal_889_test.go are written against
// the constants (so a change of the number passes them); these pin the number
// and the off-by-one of each comparison.

// The numbers are the decisions listed in the pull request (#1005, "Decisions
// for the maintainers"): a change of one is a change of the decision, made
// here and there together.
func TestDecidedBounds889(t *testing.T) {
	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"headerListRefusalBurstMax", headerListRefusalBurstMax, 100},
		{"streamRefusalBurstMax", streamRefusalBurstMax, 2000},
		{"maxContinuationFrames", maxContinuationFrames, 32},
		{"maxPriorityPlaceholders", maxPriorityPlaceholders, 16},
		{"serverResetWindow", serverResetWindow, 1024},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, the decision is %d", c.name, c.got, c.want)
		}
	}
}

// A header block of exactly maxContinuationFrames CONTINUATION frames after
// its HEADERS is whole and served; one more ends the connection.
func TestContinuationFramesBoundary889(t *testing.T) {
	run := func(t *testing.T, continuations int) (*proc944, error) {
		p := newProc944()
		block := newEnc981().req()
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
		if err := w.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: block[:1], EndStream: true}); err != nil {
			t.Fatal(err)
		}
		if err := next(); err != nil {
			t.Fatal(err)
		}
		// The empty fragments first, the rest of the block in the last frame.
		for i := 1; i <= continuations; i++ {
			frag := []byte(nil)
			if i == continuations {
				frag = block[1:]
			}
			if err := w.WriteContinuation(1, i == continuations, frag); err != nil {
				t.Fatal(err)
			}
			if err := next(); err != nil {
				return p, err
			}
		}
		return p, nil
	}
	t.Run("at-the-limit", func(t *testing.T) {
		p, err := run(t, maxContinuationFrames)
		if err != nil {
			t.Fatalf("a block of HEADERS and %d CONTINUATION frames: %v", maxContinuationFrames, err)
		}
		p.FlushInlineCleanup()
		if p.runs.Load() != 1 {
			t.Errorf("handler runs = %d, want 1", p.runs.Load())
		}
	})
	t.Run("one-over", func(t *testing.T) {
		p, err := run(t, maxContinuationFrames+1)
		if err == nil {
			t.Fatalf("a block of HEADERS and %d CONTINUATION frames was accepted", maxContinuationFrames+1)
		}
		if goaways, _ := sinks889(p.Processor); len(goaways) != 1 || goaways[0].code != http2.ErrCodeEnhanceYourCalm {
			t.Fatalf("GOAWAYs = %+v, want one ENHANCE_YOUR_CALM", goaways)
		}
	})
}

// PRIORITY on an even identifier (the server's, which a client never opens)
// that the manager does not hold creates nothing.
func TestPriorityOnAnEvenIdleStreamIsIgnored889(t *testing.T) {
	p := newProc944()
	err := processFrames981(t, p.Processor, func(w *http2.Framer) {
		if werr := w.WritePriority(2, http2.PriorityParam{StreamDep: 0, Weight: 99}); werr != nil {
			t.Fatal(werr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := p.manager.StreamCount(); n != 0 {
		t.Errorf("PRIORITY on even stream 2 left %d streams in the manager", n)
	}
	if _, ok := p.manager.priorityTree.GetPriority(2); ok {
		t.Error("PRIORITY on even stream 2 left an entry in the priority tree")
	}
}

// DATA on an identifier above the last client stream is a connection error
// even if the reset set holds it: the check in handleData is defensive (every
// reset of such an identifier returns an error, so the connection is gone
// anyway), and this keeps it so.
func TestDataOnAResetIDAboveTheLastClientStreamEndsTheConnection889(t *testing.T) {
	p := newProc944()
	if err := p.ProcessRawHeaders(1, true, newEnc981().req()); err != nil {
		t.Fatal(err)
	}
	p.FlushInlineCleanup()
	p.resetIDs.mark(7)
	f := &http2.DataFrame{FrameHeader: http2.FrameHeader{Type: http2.FrameData, StreamID: 7, Length: 4}}
	if err := p.handleData(context.Background(), f); err == nil {
		t.Fatal("DATA on a never-opened stream was ignored because the reset set holds its identifier")
	}
	if goaways, _ := sinks889(p.Processor); len(goaways) != 1 || goaways[0].code != http2.ErrCodeProtocol {
		t.Fatalf("GOAWAYs = %+v, want one PROTOCOL_ERROR", goaways)
	}
}
