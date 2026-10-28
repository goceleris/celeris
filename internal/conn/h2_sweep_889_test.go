package conn

import (
	"bytes"
	"context"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// H2State.Sweep (celeris#889): the engines' per-connection look at the request
// deadline, the header-block deadline and the idle clock of an h2 conn.

type sweepRig889 struct {
	t     *testing.T
	st    *H2State
	wire  bytes.Buffer // what the server wrote
	write func([]byte)
}

func newSweepRig889(t *testing.T, cfg H2Config) *sweepRig889 {
	t.Helper()
	r := &sweepRig889{t: t}
	r.write = func(b []byte) { r.wire.Write(b) }
	r.st = NewH2State(nil, cfg, r.write, nil)
	// The client preface and an empty SETTINGS.
	var in bytes.Buffer
	in.WriteString(http2.ClientPreface)
	if err := http2.NewFramer(&in, nil).WriteSettings(); err != nil {
		t.Fatal(err)
	}
	r.feed(in.Bytes())
	return r
}

func (r *sweepRig889) feed(b []byte) {
	r.t.Helper()
	if err := ProcessH2(context.Background(), b, r.st, nil, r.write, r.st.cfg); err != nil {
		r.t.Fatalf("ProcessH2: %v", err)
	}
}

func (r *sweepRig889) frames(f func(fr *http2.Framer)) {
	r.t.Helper()
	var in bytes.Buffer
	f(http2.NewFramer(&in, nil))
	r.feed(in.Bytes())
}

func (r *sweepRig889) block(method, path string) []byte {
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", method}, {":scheme", "http"}, {":authority", "x"}, {":path", path}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	return hb.Bytes()
}

// wframe889 is a frame the server wrote, copied out of the framer (whose
// frames are valid only until the next read).
type wframe889 struct {
	typ    http2.FrameType
	stream uint32
	ended  bool
	code   http2.ErrCode
	block  []byte
}

// written parses the frames the server wrote since the last call.
func (r *sweepRig889) written() []wframe889 {
	r.t.Helper()
	data := r.wire.Bytes()
	r.wire.Reset()
	fr := http2.NewFramer(nil, bytes.NewReader(data))
	var out []wframe889
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			return out
		}
		w := wframe889{typ: f.Header().Type, stream: f.Header().StreamID}
		switch f := f.(type) {
		case *http2.HeadersFrame:
			w.ended = f.StreamEnded()
			w.block = append([]byte(nil), f.HeaderBlockFragment()...)
		case *http2.RSTStreamFrame:
			w.code = f.ErrCode
		case *http2.GoAwayFrame:
			w.code = f.ErrCode
		}
		out = append(out, w)
	}
}

func TestSweepExpiresARequestAnswers408AndResets889(t *testing.T) {
	r := newSweepRig889(t, H2Config{ReadTimeout: time.Second})
	r.written() // the server's preface
	r.frames(func(fr *http2.Framer) {
		_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: r.block("POST", "/u"), EndHeaders: true})
	})
	now := time.Now().UnixNano()
	if res := r.st.Sweep(now, r.write); res.Wrote || res.Expired != 0 {
		t.Fatalf("a request inside its deadline was touched: %+v", res)
	}
	if len(r.written()) != 0 {
		t.Fatal("something was written inside the deadline")
	}
	res := r.st.Sweep(now+int64(2*time.Second), r.write)
	if res.Expired != 1 || !res.Wrote {
		t.Fatalf("Sweep past the deadline: %+v, want one expiry and a write", res)
	}
	var sawHeaders, sawRST bool
	for _, f := range r.written() {
		switch f.typ {
		case http2.FrameHeaders:
			if f.stream == 1 && f.ended {
				sawHeaders = true
				fields, _ := hpack.NewDecoder(4096, nil).DecodeFull(f.block)
				ok := false
				for _, h := range fields {
					if h.Name == ":status" && h.Value == "408" {
						ok = true
					}
				}
				if !ok {
					t.Fatalf("the response is not a 408: %v", fields)
				}
			}
		case http2.FrameRSTStream:
			if f.stream == 1 && f.code == http2.ErrCodeNo {
				if !sawHeaders {
					t.Fatal("the RST_STREAM came before the 408")
				}
				sawRST = true
			}
		}
	}
	if !sawHeaders || !sawRST {
		t.Fatalf("want a 408 (HEADERS, END_STREAM) then RST_STREAM(NO_ERROR) on stream 1: headers %v, rst %v", sawHeaders, sawRST)
	}
	// Late DATA on it is credited and ignored, the conn goes on.
	r.frames(func(fr *http2.Framer) { _ = fr.WriteData(1, false, []byte("late")) })
	if res := r.st.Sweep(now+int64(3*time.Second), r.write); res.Expired != 0 {
		t.Fatalf("a second expiry: %+v", res)
	}
}

func TestSweepHeaderBlockDeadlineEndsTheConn889(t *testing.T) {
	r := newSweepRig889(t, H2Config{ReadHeaderTimeout: time.Second})
	r.written()
	blk := r.block("GET", "/")
	r.frames(func(fr *http2.Framer) {
		_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: blk[:2], EndStream: true, EndHeaders: false})
	})
	now := time.Now().UnixNano()
	if res := r.st.Sweep(now, r.write); res.CloseAfterFlush || res.Wrote {
		t.Fatalf("inside the deadline: %+v", res)
	}
	res := r.st.Sweep(now+int64(2*time.Second), r.write)
	if !res.CloseAfterFlush || !res.Wrote {
		t.Fatalf("past the deadline: %+v, want CloseAfterFlush", res)
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
	// Completing the block disarms it.
	r2 := newSweepRig889(t, H2Config{ReadHeaderTimeout: time.Second})
	r2.written()
	r2.frames(func(fr *http2.Framer) {
		_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: blk[:2], EndStream: true, EndHeaders: false})
		_ = fr.WriteContinuation(1, true, blk[2:])
	})
	if res := r2.st.Sweep(now+int64(5*time.Second), r2.write); res.CloseAfterFlush {
		t.Fatalf("a completed block expired: %+v", res)
	}
}

func TestSweepIdleClockIgnoresPings889(t *testing.T) {
	r := newSweepRig889(t, H2Config{IdleTimeout: time.Second})
	r.written()
	t0 := time.Now().UnixNano()
	if res := r.st.Sweep(t0, r.write); res.CloseNow {
		t.Fatalf("the first sweep closed the conn: %+v", res)
	}
	ping := func() {
		r.frames(func(fr *http2.Framer) { _ = fr.WritePing(false, [8]byte{1}) })
	}
	for i := 1; i <= 4; i++ {
		ping()
		res := r.st.Sweep(t0+int64(i)*int64(300*time.Millisecond), r.write)
		if want := i*300 > 1000; res.CloseNow != want {
			t.Fatalf("after %d ms of pings: CloseNow %v, want %v", i*300, res.CloseNow, want)
		}
	}
	// A stream moves the clock.
	r.frames(func(fr *http2.Framer) {
		_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: r.block("POST", "/u"), EndHeaders: true})
	})
	if res := r.st.Sweep(t0+int64(1500*time.Millisecond), r.write); res.CloseNow {
		t.Fatalf("a conn with a stream open was closed as idle: %+v", res)
	}
	// With IdleTimeout off nothing closes it.
	q := newSweepRig889(t, H2Config{})
	q.written()
	q.st.Sweep(t0, q.write)
	if res := q.st.Sweep(t0+int64(time.Hour), q.write); res.CloseNow {
		t.Fatalf("IdleTimeout off, closed: %+v", res)
	}
}

// Nothing due takes no lock, and a lock that is held is left to the next sweep.
func TestSweepTakesNoLockWhenNothingIsDue889(t *testing.T) {
	r := newSweepRig889(t, H2Config{ReadTimeout: time.Second, ReadHeaderTimeout: time.Second})
	r.written()
	r.frames(func(fr *http2.Framer) {
		_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: r.block("POST", "/u"), EndHeaders: true})
	})
	now := time.Now().UnixNano()
	r.st.mu.Lock()
	done := make(chan SweepResult)
	go func() { done <- r.st.Sweep(now, r.write) }()
	select {
	case res := <-done:
		if res.Wrote || res.Expired != 0 {
			t.Fatalf("not due: %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Sweep blocked on H2State.mu with nothing due")
	}
	go func() { done <- r.st.Sweep(now+int64(5*time.Second), r.write) }()
	select {
	case res := <-done:
		if res.Wrote || res.Expired != 0 {
			t.Fatalf("due, lock held: %+v, want it left to the next sweep", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Sweep blocked on a held H2State.mu")
	}
	r.st.mu.Unlock()
	if res := r.st.Sweep(now+int64(5*time.Second), r.write); res.Expired != 1 {
		t.Fatalf("after the lock was released: %+v, want the expiry", res)
	}
}

// An engine that finds the conn still sending when Sweep proposes the idle close
// calls ExtendIdle: the idle period runs again from there. GoAwayNoWait writes a
// GOAWAY(NO_ERROR) for the close that does go ahead, and leaves a held lock to
// the next sweep.
func TestSweepExtendIdleAndGoAwayNoWait889(t *testing.T) {
	r := newSweepRig889(t, H2Config{IdleTimeout: time.Second})
	r.written()
	t0 := time.Now().UnixNano()
	r.st.Sweep(t0, r.write)
	at := t0 + int64(1500*time.Millisecond)
	if res := r.st.Sweep(at, r.write); !res.CloseNow {
		t.Fatalf("after 1.5 s idle: %+v, want CloseNow", res)
	}
	if !r.st.ExtendIdle(at, 1000, true) { // the engine found output pending
		t.Fatal("the first ExtendIdle refused")
	}
	if res := r.st.Sweep(at+int64(500*time.Millisecond), r.write); res.CloseNow {
		t.Fatal("CloseNow 0.5 s after ExtendIdle")
	}
	if res := r.st.Sweep(at+int64(1200*time.Millisecond), r.write); !res.CloseNow {
		t.Fatal("no CloseNow IdleTimeout after ExtendIdle")
	}
	r.st.mu.Lock()
	if r.st.GoAwayNoWait(r.write) {
		t.Fatal("GoAwayNoWait wrote with H2State.mu held")
	}
	r.st.mu.Unlock()
	if !r.st.GoAwayNoWait(r.write) {
		t.Fatal("GoAwayNoWait did not write")
	}
	var ga *wframe889
	for _, f := range r.written() {
		if f.typ == http2.FrameGoAway {
			f := f
			ga = &f
		}
	}
	if ga == nil || ga.code != http2.ErrCodeNo {
		t.Fatalf("want GOAWAY(NO_ERROR), got %+v", ga)
	}
}

// A burst of expiries is answered in one Sweep: all of them, in batches of 32
// until a batch comes back short. The time budget that bounds the loop is taken
// out of the count (a variable a test sets), so that the count does not depend on
// the machine: with it off, 100 expiries take one Sweep (a mutant that answers
// one batch of 32, or 8, per sweep needs 4 or 13) and with a budget that is spent
// at once the first batch is all one Sweep answers and the rest is the next's,
// never lost.
func TestSweepAnswersABurstOfExpiriesInOneSweep889(t *testing.T) {
	const n = 100
	burst := func(t *testing.T, budget time.Duration) (sweeps, first int) {
		t.Helper()
		old := expireBudget
		expireBudget = budget
		t.Cleanup(func() { expireBudget = old })
		r := newSweepRig889(t, H2Config{ReadTimeout: time.Second})
		r.written()
		r.frames(func(fr *http2.Framer) {
			for i := range n {
				_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: uint32(1 + 2*i), BlockFragment: r.block("POST", "/u"), EndHeaders: true})
			}
		})
		now := time.Now().UnixNano() + int64(2*time.Second)
		total := 0
		for total < n && sweeps < 20 {
			e := r.st.Sweep(now+int64(sweeps), r.write).Expired
			if sweeps == 0 {
				first = e
			}
			total += e
			sweeps++
		}
		if total != n {
			t.Fatalf("%d of %d expired requests answered after %d sweeps", total, n, sweeps)
		}
		return sweeps, first
	}
	t.Run("no-budget", func(t *testing.T) {
		if sweeps, first := burst(t, time.Hour); sweeps != 1 || first != n {
			t.Fatalf("%d sweeps, the first answered %d of %d: want one sweep for all", sweeps, first, n)
		}
	})
	t.Run("budget-spent", func(t *testing.T) {
		if sweeps, first := burst(t, -1); first != 32 || sweeps != 4 {
			t.Fatalf("%d sweeps, the first answered %d: want a batch of 32 per sweep (4 sweeps) when the budget is spent at once", sweeps, first)
		}
	})
}

// TestSweepExtendIdleNeedsProgress889: the idle period is extended for output
// only while the peer acknowledged more bytes since the last extension than the
// control frames the server wrote to it in the meantime; stream activity forgets
// the baseline, and a conn whose acknowledged bytes cannot be read is extended.
func TestSweepExtendIdleNeedsProgress889(t *testing.T) {
	newConn := func() (*sweepRig889, int64) {
		r := newSweepRig889(t, H2Config{IdleTimeout: time.Second})
		r.written()
		now := time.Now().UnixNano()
		r.st.Sweep(now, r.write)
		return r, now
	}
	step := int64(1200 * time.Millisecond)
	ctl := func(r *sweepRig889, n int) { // the server writes n bytes of control frames
		r.st.outBuf.Write(make([]byte, n))
		r.st.flushOut(r.write)
	}
	t.Run("a-reader-extends", func(t *testing.T) {
		r, now := newConn()
		acked := uint64(1000)
		for i := range 6 { // the peer takes 100 KB per period, the server wrote 100 bytes of ACKs
			now += step
			if res := r.st.Sweep(now, r.write); !res.CloseNow {
				t.Fatalf("no CloseNow at period %d", i)
			}
			if !r.st.ExtendIdle(now, acked, true) {
				t.Fatalf("extension %d refused although the peer acknowledged 100 KB", i)
			}
			ctl(r, 100)
			acked += 100 << 10
		}
	})
	t.Run("a-flat-count-refuses", func(t *testing.T) {
		r, now := newConn()
		now += step
		r.st.Sweep(now, r.write)
		if !r.st.ExtendIdle(now, 5000, true) {
			t.Fatal("the first extension has no baseline and is given")
		}
		now += step
		r.st.Sweep(now, r.write)
		if r.st.ExtendIdle(now, 5000, true) {
			t.Fatal("extended with nothing acknowledged in the period (a client that stopped reading)")
		}
	})
	t.Run("acks-of-our-own-frames-are-not-progress", func(t *testing.T) {
		r, now := newConn()
		now += step
		r.st.Sweep(now, r.write)
		r.st.ExtendIdle(now, 5000, true)
		ctl(r, 85) // five PING answers
		now += step
		r.st.Sweep(now, r.write)
		if r.st.ExtendIdle(now, 5000+85, true) {
			t.Fatal("extended on the acknowledgement of the 85 bytes the server itself wrote (ACK lag)")
		}
		// One more byte than the server wrote is progress (the in-flight went down).
		r2, now2 := newConn()
		now2 += step
		r2.st.Sweep(now2, r2.write)
		r2.st.ExtendIdle(now2, 5000, true)
		ctl(r2, 85)
		if !r2.st.ExtendIdle(now2+step, 5000+86, true) {
			t.Fatal("refused with more acknowledged than the server wrote")
		}
	})
	t.Run("no-count-extends", func(t *testing.T) {
		r, now := newConn()
		for i := range 4 {
			now += step
			r.st.Sweep(now, r.write)
			if !r.st.ExtendIdle(now, 0, false) {
				t.Fatalf("a conn whose acknowledged bytes cannot be read was refused at %d", i)
			}
		}
	})
	t.Run("activity-clears-the-baseline", func(t *testing.T) {
		r, t0 := newConn()
		r.st.Sweep(t0+step, r.write)
		if !r.st.ExtendIdle(t0+step, 1000, true) || r.st.d0Acked == 0 {
			t.Fatal("setup")
		}
		r.frames(func(fr *http2.Framer) {
			_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: r.block("POST", "/u"), EndHeaders: true})
		})
		r.st.Sweep(t0+2*step, r.write)
		if r.st.d0Acked != 0 {
			t.Fatalf("stream activity left the baseline at %d", r.st.d0Acked)
		}
	})
}

// TestSweepFramesOnAResetStreamAreNotActivity889: the server resets a request
// (no :path) whose client side is still open; DATA the client goes on sending for
// that stream is ignored and is not stream activity, so the idle clock runs out
// whatever it sends. For the stream of an expired request, which has a tombstone
// that lasts at most 2 x ReadTimeout, it is activity (the slow uploader, M3).
func TestSweepFramesOnAResetStreamAreNotActivity889(t *testing.T) {
	step := int64(300 * time.Millisecond)
	t.Run("reset-for-another-cause", func(t *testing.T) {
		r := newSweepRig889(t, H2Config{IdleTimeout: time.Second})
		r.written()
		now := time.Now().UnixNano()
		r.st.Sweep(now, r.write)
		r.frames(func(fr *http2.Framer) {
			noPath := []byte{0x82, 0x86, 0x41, 0x01, 'x'}
			_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: noPath, EndHeaders: true})
		})
		closed := false
		for i := 0; i < 12 && !closed; i++ {
			now += step
			r.frames(func(fr *http2.Framer) { _ = fr.WriteData(1, false, []byte{'x'}) })
			closed = r.st.Sweep(now, r.write).CloseNow
		}
		if !closed {
			t.Fatal("DATA for a stream the server reset held the idle clock for 3.6 s with IdleTimeout 1 s")
		}
	})
	t.Run("expired-request", func(t *testing.T) {
		r := newSweepRig889(t, H2Config{IdleTimeout: time.Second, ReadTimeout: time.Second})
		r.written()
		now := time.Now().UnixNano()
		r.frames(func(fr *http2.Framer) {
			_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: r.block("POST", "/u"), EndHeaders: true})
		})
		now += int64(1100 * time.Millisecond)
		if res := r.st.Sweep(now, r.write); res.Expired != 1 {
			t.Fatalf("setup: %+v", res)
		}
		for i := 0; i < 6; i++ { // 1.8 s of the uploader's DATA, past IdleTimeout
			now += step
			r.frames(func(fr *http2.Framer) { _ = fr.WriteData(1, false, []byte{'x'}) })
			if r.st.Sweep(now, r.write).CloseNow {
				t.Fatalf("the uploader's DATA for an expired request did not hold the idle clock (at %d ms)", (i+1)*300)
			}
		}
	})
}

// TestH2StateStaysInItsSizeClass889: an H2State is one allocation of exactly
// 640 bytes, the edge of a malloc size class; one byte more is 704, 10% more memory
// for every h2 connection. The sweep's fields (celeris#889) were packed to stay
// in it; the words the sweep's look at a conn with nothing due reads are the
// first 40 bytes.
func TestH2StateStaysInItsSizeClass889(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the size class is a 64-bit one")
	}
	if got := unsafe.Sizeof(H2State{}); got > 640 {
		t.Errorf("sizeof(H2State) = %d, want at most 640 (the malloc size class it was in)", got)
	}
}
