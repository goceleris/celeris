package conn

import (
	"bytes"
	"context"
	"testing"
	"time"

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
