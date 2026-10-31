package conn

import (
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris/internal/mono"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// celeris#1190 (follow-up of #1178). The HTTP/2 deadlines the engines' stepped-clock
// tests do not reach: the connection's idle clock D0 and its ack cap, the write
// window D2 (ExpiredWrites), the header-block deadline and the tombstone deadline.
// The clock is mono.Manual: it moves only when the test says, so there is no sleep
// and no margin for a slow runner. For each deadline, in both directions of a step
// of the wall clock (75 s): the step alone must not fire it (a deadline that fired
// early), and the time that then really passes must fire it (a deadline held past its
// bound, which a backward step does to a clock on the wall).

var steps1190 = []struct {
	name string
	step time.Duration
}{{"forward", 75 * time.Second}, {"backward", -75 * time.Second}}

func clock1190(t *testing.T) *mono.Manual {
	t.Helper()
	m := mono.NewManual()
	t.Cleanup(mono.SetManual(m))
	return m
}

// D0: IdleTimeout (1 s) for a connection with no stream open runs from the last
// stream activity the sweep saw; a step of the wall clock neither closes a fresh conn
// nor keeps an idle one. ExtendIdle gives the period again to a conn whose output the
// peer is acknowledging, and refuses it when the peer has acknowledged nothing since
// the last extension: those periods are on the same clock.
func TestIdleClockAndAckCapIgnoreAStepOfTheWallClock1190(t *testing.T) {
	for _, tc := range steps1190 {
		t.Run(tc.name, func(t *testing.T) {
			clk := clock1190(t)
			r := newSweepRig889(t, H2Config{IdleTimeout: time.Second})
			r.written()
			if res := r.st.Sweep(mono.Now(), r.write); res.CloseNow {
				t.Fatalf("the first sweep closed the conn: %+v", res)
			}
			clk.Step(tc.step)
			if tc.step > 0 { // a backward step is judged by the time that passes
				if res := r.st.Sweep(mono.Now(), r.write); res.CloseNow {
					t.Fatalf("a step of %v closed an idle clock that had run for nothing: %+v", tc.step, res)
				}
			}
			clk.Advance(1200 * time.Millisecond)
			if res := r.st.Sweep(mono.Now(), r.write); !res.CloseNow {
				t.Fatalf("after IdleTimeout (1.2 s of time, step %v) the idle conn was not closed: %+v", tc.step, res)
			}
			// The engine holds output: the first extension is given (the baseline is
			// taken), the period runs again from now.
			if !r.st.ExtendIdle(mono.Now(), 1000, true) {
				t.Fatal("the first extension was refused")
			}
			clk.Step(tc.step)
			clk.Advance(500 * time.Millisecond)
			if res := r.st.Sweep(mono.Now(), r.write); res.CloseNow {
				t.Fatalf("0.5 s into the extended period (step %v) the conn was closed: %+v", tc.step, res)
			}
			clk.Advance(700 * time.Millisecond)
			if res := r.st.Sweep(mono.Now(), r.write); !res.CloseNow {
				t.Fatalf("the extended period (1.2 s of time, step %v) did not run out: %+v", tc.step, res)
			}
			// The peer acknowledged nothing since: the cap closes it. With progress it
			// is extended again.
			if r.st.ExtendIdle(mono.Now(), 1000, true) {
				t.Fatal("no progress acknowledged, and the extension was given")
			}
			if !r.st.ExtendIdle(mono.Now(), 5000, true) {
				t.Fatal("4000 bytes acknowledged, and the extension was refused")
			}
			clk.Step(tc.step)
			clk.Advance(500 * time.Millisecond)
			if res := r.st.Sweep(mono.Now(), r.write); res.CloseNow {
				t.Fatalf("0.5 s into a period given for acknowledged output (step %v) the conn was closed: %+v", tc.step, res)
			}
		})
	}
}

// D2 and ExpiredWrites: a stream that waits for the peer's window is reset at the
// end of its write window (WriteTimeout, 1 s) when it released less than its floor.
func TestWriteWindowExpiryIgnoresAStepOfTheWallClock1190(t *testing.T) {
	for _, tc := range steps1190 {
		t.Run(tc.name, func(t *testing.T) {
			clk := clock1190(t)
			r := newSweepRig889(t, H2Config{WriteTimeout: time.Second})
			r.written()
			m := r.st.processor.GetManager()
			m.UpdateConnectionWindow(1 << 20)
			r.frames(func(fr *http2.Framer) {
				_ = fr.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 0})
			})
			s := m.CreateStream(1)
			s.SetState(stream.StateHalfClosedRemote)
			s.SetWindowSize(0)
			s.BufferOutbound(make([]byte, 1<<20), true) // the window begins now
			clk.Step(tc.step)
			if tc.step > 0 {
				if res := r.st.Sweep(mono.Now(), r.write); res.Expired != 0 {
					t.Fatalf("a step of %v ended a write window that had run for nothing: %+v", tc.step, res)
				}
				if got := m.ExpiredWrites(mono.Now(), make([]uint32, 0, 8)); len(got) != 0 {
					t.Fatalf("ExpiredWrites after a step of %v returned %v", tc.step, got)
				}
			}
			clk.Advance(1200 * time.Millisecond)
			if got := m.ExpiredWrites(mono.Now(), make([]uint32, 0, 8)); len(got) != 1 || got[0] != 1 {
				t.Fatalf("ExpiredWrites after the window (1.2 s of time, step %v) returned %v, want stream 1", tc.step, got)
			}
			res := r.st.Sweep(mono.Now(), r.write)
			if res.Expired != 1 || !res.Wrote {
				t.Fatalf("the sweep after the window (step %v): %+v, want the stream reset", tc.step, res)
			}
			if _, ok := m.GetStream(1); ok {
				t.Fatal("stream 1 is still on the connection")
			}
		})
	}
}

// D2, the other way: a stream that releases its floor in every window is not reset,
// and a step between windows is not a missed window.
func TestWriteWindowRenewalIgnoresAStepOfTheWallClock1190(t *testing.T) {
	for _, tc := range steps1190 {
		t.Run(tc.name, func(t *testing.T) {
			clk := clock1190(t)
			r := newSweepRig889(t, H2Config{WriteTimeout: time.Second})
			r.written()
			m := r.st.processor.GetManager()
			m.UpdateConnectionWindow(1 << 20)
			r.frames(func(fr *http2.Framer) {
				_ = fr.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 0})
			})
			s := m.CreateStream(1)
			s.SetState(stream.StateHalfClosedRemote)
			s.SetWindowSize(0)
			s.BufferOutbound(make([]byte, 1<<20), true)
			for w := 1; w <= 3; w++ {
				s.SetWindowSize(65536) // the client reads: the window is returned
				m.FlushOutbound(s, func(uint32, bool, []byte) {})
				m.UpdateConnectionWindow(65536)
				clk.Step(tc.step)
				clk.Advance(1100 * time.Millisecond)
				if res := r.st.Sweep(mono.Now(), r.write); res.Expired != 0 {
					t.Fatalf("window %d (step %v): a stream that released its floor was reset: %+v", w, tc.step, res)
				}
			}
			if _, ok := m.GetStream(1); !ok {
				t.Fatal("the stream that kept releasing its floor was taken")
			}
			// It stops: the next windows' ends take it, however the wall moved.
			var res SweepResult
			for w := 1; w <= 4 && res.Expired == 0; w++ {
				clk.Step(tc.step)
				clk.Advance(1100 * time.Millisecond)
				res = r.st.Sweep(mono.Now(), r.write)
			}
			if res.Expired != 1 {
				t.Fatalf("a stream that stopped releasing was not reset within four windows (step %v): %+v", tc.step, res)
			}
		})
	}
}

// The header-block deadline: ReadHeaderTimeout (1 s) after the first HEADERS of a
// block that does not end with it, the connection ends with GOAWAY.
func TestHeaderBlockDeadlineIgnoresAStepOfTheWallClock1190(t *testing.T) {
	for _, tc := range steps1190 {
		t.Run(tc.name, func(t *testing.T) {
			clk := clock1190(t)
			r := newSweepRig889(t, H2Config{ReadHeaderTimeout: time.Second})
			r.written()
			blk := r.block("GET", "/")
			r.frames(func(fr *http2.Framer) {
				_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: blk[:2], EndStream: true, EndHeaders: false})
			})
			clk.Step(tc.step)
			if tc.step > 0 {
				if res := r.st.Sweep(mono.Now(), r.write); res.CloseAfterFlush || res.Wrote {
					t.Fatalf("a step of %v ended a header block that had run for nothing: %+v", tc.step, res)
				}
			}
			clk.Advance(1200 * time.Millisecond)
			res := r.st.Sweep(mono.Now(), r.write)
			if !res.CloseAfterFlush || !res.Wrote {
				t.Fatalf("the header block (1.2 s of time, step %v) was not ended: %+v", tc.step, res)
			}
			var ga *wframe889
			for _, f := range r.written() {
				if f.typ == http2.FrameGoAway {
					f := f
					ga = &f
				}
			}
			if ga == nil || ga.code != http2.ErrCodeEnhanceYourCalm {
				t.Fatalf("want GOAWAY(ENHANCE_YOUR_CALM), got %+v", ga)
			}
		})
	}
}

// The tombstone deadline: a stream whose request deadline expired is remembered for
// two request timeouts (so that its DATA in flight is ignored, not an error), then
// forgotten.
func TestTombstoneDeadlineIgnoresAStepOfTheWallClock1190(t *testing.T) {
	for _, tc := range steps1190 {
		t.Run(tc.name, func(t *testing.T) {
			clk := clock1190(t)
			r := newSweepRig889(t, H2Config{ReadTimeout: time.Second})
			r.written()
			r.frames(func(fr *http2.Framer) {
				_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: r.block("POST", "/u"), EndHeaders: true})
			})
			clk.Advance(1200 * time.Millisecond)
			if res := r.st.Sweep(mono.Now(), r.write); res.Expired != 1 {
				t.Fatalf("the request was not expired: %+v", res)
			}
			p := r.st.processor
			left := time.Duration(p.NextTombstoneDeadline() - mono.Now())
			if left < 1900*time.Millisecond || left > 2100*time.Millisecond {
				t.Fatalf("the tombstone has %v to live, want two request timeouts (2 s)", left)
			}
			clk.Step(tc.step)
			if tc.step > 0 {
				r.st.Sweep(mono.Now(), r.write)
				if p.NextTombstoneDeadline() == 0 {
					t.Fatalf("a step of %v dropped a tombstone that had lived for nothing", tc.step)
				}
			}
			clk.Advance(1900 * time.Millisecond)
			r.st.Sweep(mono.Now(), r.write)
			if p.NextTombstoneDeadline() == 0 {
				t.Fatalf("the tombstone was dropped 1.9 s into its 2 s (step %v)", tc.step)
			}
			clk.Advance(300 * time.Millisecond)
			r.st.Sweep(mono.Now(), r.write)
			if p.NextTombstoneDeadline() != 0 {
				t.Fatalf("the tombstone outlived its 2 s (2.2 s of time, step %v)", tc.step)
			}
		})
	}
}
