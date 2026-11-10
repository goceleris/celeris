//go:build linux

package epoll

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/diag/resid"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// Tests of the celeris#757 residence stamps (resid_linux.go): the bookkeeping
// of a sample, the flushWrites hook, the gate, and an h2c integration run
// through a real epoll engine.

// residLoop is a residState sampling every recv, on a fresh registry.
func residLoop(t *testing.T) *residState {
	t.Helper()
	resid.Reset()
	t.Cleanup(resid.Reset)
	return &residState{Loop: resid.NewLoopShift(0, 0)}
}

func TestResidFinishAccounting(t *testing.T) {
	cs := &connState{}
	arm := func(r *residState) { r.arm(cs, 100); r.rx, r.proc = 110, 150; r.k, r.pool = 1, 0 }
	type want struct{ n, unst, part, nowr, early uint64 }
	for name, tc := range map[string]struct {
		setup func(r *residState)
		want  want
	}{
		"timed":     {func(r *residState) { r.wr = 190 }, want{n: 1}},
		"pool":      {func(r *residState) { r.pool, r.wr = 1, 190 }, want{unst: 1}},
		"no req":    {func(r *residState) { r.k, r.wr = 0, 190 }, want{}},
		"partial":   {func(r *residState) { r.partial = true }, want{part: 1}},
		"no write":  {func(r *residState) {}, want{nowr: 1}},
		"write<prc": {func(r *residState) { r.wr = 140 }, want{early: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			r := residLoop(t)
			arm(r)
			tc.setup(r)
			r.finish()
			got := want{r.N.Load(), r.Unst.Load(), r.Part.Load(), r.NoWr.Load(), r.Early.Load()}
			if got != tc.want {
				t.Errorf("counters %+v, want %+v", got, tc.want)
			}
			if r.cs != nil {
				t.Error("finish left the sample armed")
			}
			r.finish() // a second finish with nothing armed is a no-op
			if got2 := (want{r.N.Load(), r.Unst.Load(), r.Part.Load(), r.NoWr.Load(), r.Early.Load()}); got2 != tc.want {
				t.Errorf("a finish with nothing armed counted: %+v", got2)
			}
		})
	}
}

func TestResidFinishSegments(t *testing.T) {
	r := residLoop(t)
	r.arm(&connState{}, 100)
	r.rx, r.proc, r.wr, r.k = 110, 150, 190, 3
	r.krx, r.rxWall = 1_000, 1_900
	var got residSample
	residSampleHook = func(s residSample) { got = s }
	defer func() { residSampleHook = nil }()
	r.finish()
	s := resid.Snapshot()
	// weight 3 each: R = 80, S1 = 10, S2 = 40, S3 = 40, G = 900
	for _, c := range []struct {
		h   int
		sum uint64
	}{{resid.HistR, 3 * 80}, {resid.HistS1, 3 * 10}, {resid.HistS2, 3 * 40}, {resid.HistS3, 3 * 40}, {resid.HistG, 3 * 900}} {
		if s.H[c.h].N != 3 || s.H[c.h].SumNS != c.sum {
			t.Errorf("%s: n=%d sum=%d, want 3 and %d", resid.HistNames[c.h], s.H[c.h].N, s.H[c.h].SumNS, c.sum)
		}
	}
	if got.Ew != 100 || got.Rx != 110 || got.Proc != 150 || got.Wr != 190 || got.K != 3 || !got.HasG || got.G != 900 {
		t.Errorf("hook sample %+v", got)
	}
	// No kernel stamp: no G sample, the rest still recorded.
	r.arm(&connState{}, 100)
	r.rx, r.proc, r.wr, r.k, r.krx = 110, 150, 190, 1, 0
	r.finish()
	if s := resid.Snapshot(); s.H[resid.HistG].N != 3 || s.H[resid.HistR].N != 4 {
		t.Errorf("without a kernel stamp: G_n=%d R_n=%d, want 3 and 4", s.H[resid.HistG].N, s.H[resid.HistR].N)
	}
}

func TestResidSampleReadGate(t *testing.T) {
	r := residLoop(t)
	cs := &connState{detected: true, protocol: engine.H2C}
	if !r.sampleRead(cs) {
		t.Error("a detected h2c conn is sampled at shift 0")
	}
	for name, c := range map[string]*connState{
		"undetected": {protocol: engine.H2C},
		"promoted":   {detected: true, protocol: engine.H2C, asyncPromoted: true},
		"http1":      {detected: true, protocol: engine.HTTP1},
	} {
		if r.sampleRead(c) {
			t.Errorf("%s: sampled", name)
		}
	}
}

func TestResidTurnStamps(t *testing.T) {
	r := residLoop(t)
	t0 := r.turnBegin()
	if t0 == 0 {
		t.Fatal("shift 0 samples every turn")
	}
	r.turnWoke(t0, t0+500)
	r.turnEnd()
	s := resid.Snapshot()
	if s.H[resid.HistWt].N != 1 || s.H[resid.HistWt].SumNS != 500 || s.H[resid.HistB].N != 1 || s.H[resid.HistB].SumNS == 0 {
		t.Errorf("Wt n=%d sum=%d, B n=%d sum=%d", s.H[resid.HistWt].N, s.H[resid.HistWt].SumNS, s.H[resid.HistB].N, s.H[resid.HistB].SumNS)
	}
	// A turn that is not sampled records nothing, and turnEnd does not reuse a
	// stale start.
	r.turnWoke(0, 99)
	r.turnEnd()
	r.bStart = 0
	if s := resid.Snapshot(); s.H[resid.HistWt].N != 1 || s.H[resid.HistB].N != 1 {
		t.Errorf("an unsampled turn recorded: Wt %d B %d", s.H[resid.HistWt].N, s.H[resid.HistB].N)
	}
}

// TestResidFlushHook: flushWrites stamps T_wr for the sampled conn on the
// loop's thread only (a dispatch goroutine's flush, onLoopThread false, must
// not touch the loop-local state), for a conn that is not the sampled one not
// at all, and tells a partial write from a complete one.
func TestResidFlushHook(t *testing.T) {
	r := residLoop(t)
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fds[0]); _ = unix.Close(fds[1]) }()
	l := &Loop{resid: r, bytesWritten: &atomic.Uint64{}}
	cs := &connState{fd: fds[0], writeBuf: []byte("hello")}
	other := &connState{fd: fds[0], writeBuf: []byte("x")}
	r.arm(cs, 1)

	if err := l.flushWrites(cs, false); err != nil || r.wr != 0 || r.partial {
		t.Fatalf("a flush off the loop thread touched the sample: err=%v wr=%d partial=%v", err, r.wr, r.partial)
	}
	cs.writeBuf, cs.writePos = []byte("hello"), 0
	if err := l.flushWrites(other, true); err != nil || r.wr != 0 {
		t.Fatalf("another conn's flush stamped the sample: err=%v wr=%d", err, r.wr)
	}
	if err := l.flushWrites(cs, true); err != nil || r.wr == 0 || r.partial {
		t.Fatalf("a complete flush: err=%v wr=%d partial=%v", err, r.wr, r.partial)
	}
	// writev path, complete.
	r.arm(cs, 1)
	cs.writeBuf, cs.writePos, cs.bodyBuf = []byte("head"), 0, []byte("body")
	if err := l.flushWrites(cs, true); err != nil || r.wr == 0 {
		t.Fatalf("a complete writev: err=%v wr=%d", err, r.wr)
	}
	// A partial write: fill the socket so that one write takes part of the queue.
	r.arm(cs, 1)
	cs.bodyBuf = nil
	cs.writeBuf, cs.writePos = bytes.Repeat([]byte("z"), 8<<20), 0
	if err := l.flushWrites(cs, true); err != nil {
		t.Fatal(err)
	}
	if r.wr != 0 || !r.partial || cs.writePos == 0 {
		t.Fatalf("a partial write: wr=%d partial=%v writePos=%d", r.wr, r.partial, cs.writePos)
	}
	// A write inside ProcessH2 is counted (the guarded-writeFn question): drain
	// the socket first so that the write is not an EAGAIN.
	for buf := make([]byte, 1<<20); ; {
		if n, err := unix.Read(fds[1], buf); n <= 0 || err != nil {
			break
		}
	}
	cs.writeBuf, cs.writePos = nil, 0
	r.arm(cs, 1)
	r.procBegin(conn.NewH2State(nil, conn.H2Config{}, func([]byte) {}, nil))
	cs.writeBuf, cs.writePos = nil, 0
	_ = l.flushWrites(cs, true) // nothing to write: no write, no count
	if r.InProc.Load() != 0 {
		t.Errorf("a flush with nothing queued counted as a write in ProcessH2")
	}
	cs.writeBuf = []byte("a")
	_ = l.flushWrites(cs, true)
	if r.InProc.Load() != 1 {
		t.Errorf("a write inside ProcessH2: InProc = %d, want 1", r.InProc.Load())
	}
}

// ---- h2c integration ----

type residHandler struct {
	spin time.Duration
	n    atomic.Int64
}

func (h *residHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	h.n.Add(1)
	if h.spin > 0 {
		for t := time.Now(); time.Since(t) < h.spin; {
		}
	}
	if s.ResponseWriter == nil {
		return nil
	}
	body := `{"message":"Hello, World!"}`
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "application/json"}, {"content-length", strconv.Itoa(len(body))}}, []byte(body))
}

// startResid starts an h2c epoll engine with the gate on (the loops read it
// when they are created) and 2 workers.
func startResid(t *testing.T, async bool, h stream.Handler) *Engine {
	t.Helper()
	resid.Reset()
	restore := resid.SetEnabled(true)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, err := New(resource.Config{
		Addr: addr, Protocol: engine.H2C, AsyncHandlers: async,
		Resources: resource.Resources{Workers: 2},
	}, h)
	if err != nil {
		restore()
		t.Fatalf("epoll.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done: //nolint:errcheck // Listen's result is not under test here
		case <-time.After(5 * time.Second):
		}
		restore()
		resid.Reset()
	})
	for dl := time.Now().Add(20 * time.Second); time.Now().Before(dl); {
		if e.Addr() != nil && e.NumWorkers() > 0 {
			return e
		}
		select {
		case lerr := <-done:
			t.Fatalf("epoll engine did not start: %v", lerr)
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("epoll engine did not start within 20s")
	return nil
}

// h2cClient runs nreq sequential GETs on one prior-knowledge h2c conn, one
// stream at a time (a loadgen's closed-loop connection).
func h2cClient(t *testing.T, addr string, nreq int) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Errorf("dial: %v", err)
		return
	}
	defer func() { _ = c.Close() }()
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/json"}, {":authority", "x"}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	block := hb.Bytes()
	fr := http2.NewFramer(c, bufio.NewReader(c))
	if _, err := c.Write([]byte(http2.ClientPreface)); err != nil {
		t.Errorf("preface: %v", err)
		return
	}
	if err := fr.WriteSettings(); err != nil {
		t.Errorf("settings: %v", err)
		return
	}
	// The connection's receive window is 65535 bytes: without this the server
	// stops answering after about 2400 responses.
	if err := fr.WriteWindowUpdate(0, 1<<30); err != nil {
		t.Errorf("window update: %v", err)
		return
	}
	// The first request goes with the first reads: that read carries the
	// preface and is not sampled; the rest are one request per recv.
	for i := 0; i < nreq; i++ {
		id := uint32(2*i + 1)
		if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: block, EndStream: true, EndHeaders: true}); err != nil {
			t.Errorf("write request %d: %v", i, err)
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		for done := false; !done; {
			f, err := fr.ReadFrame()
			if err != nil {
				t.Errorf("read request %d: %v", i, err)
				return
			}
			switch f := f.(type) {
			case *http2.SettingsFrame:
				if !f.IsAck() {
					_ = fr.WriteSettingsAck()
				}
			case *http2.DataFrame:
				done = f.StreamID == id && f.StreamEnded()
			case *http2.HeadersFrame:
				done = f.StreamID == id && f.StreamEnded()
			}
		}
	}
}

func runResidLoad(t *testing.T, e *Engine, conns, nreq int) {
	t.Helper()
	var wg sync.WaitGroup
	for range conns {
		wg.Add(1)
		go func() { defer wg.Done(); h2cClient(t, e.Addr().String(), nreq) }()
	}
	wg.Wait()
}

// TestResidH2cIntegration drives requests through the epoll engine with the
// gate on, sync and AsyncHandlers (the async loop holds cs.detachMu around
// ProcessH2 and the inline flush: the config of the d757 bench), and checks the
// counts and the ordering T_ew <= T_rx <= T_proc <= T_wr of every sample.
func TestResidH2cIntegration(t *testing.T) {
	for _, async := range []bool{false, true} {
		name := "sync"
		if async {
			name = "async"
		}
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var samples []residSample
			residSampleHook = func(s residSample) { mu.Lock(); samples = append(samples, s); mu.Unlock() }
			t.Cleanup(func() { residSampleHook = nil })
			h := &residHandler{}
			e := startResid(t, async, h)
			const conns, nreq = 4, 3000
			runResidLoad(t, e, conns, nreq)
			if got := h.n.Load(); got != conns*nreq {
				t.Fatalf("handler ran %d times, want %d", got, conns*nreq)
			}
			s := resid.Snapshot()
			line := s.Line(time.Now().Unix())
			t.Logf("%s", line)
			if _, back, err := resid.Parse(line); err != nil || back.N != s.N {
				t.Fatalf("the exported line does not parse back: %v", err)
			}
			if s.N == 0 || s.H[resid.HistR].N == 0 {
				t.Fatalf("no sample timed: n=%d R_n=%d unst=%d part=%d nowr=%d early=%d", s.N, s.H[resid.HistR].N, s.Unst, s.Part, s.NoWr, s.Early)
			}
			// 1 in 64 of 12000 recvs is about 187: half of it is a safe floor.
			if s.N < conns*nreq/64/2 {
				t.Errorf("only %d samples of ~%d expected", s.N, conns*nreq/64)
			}
			for _, h := range []int{resid.HistS1, resid.HistS2, resid.HistS3} {
				if s.H[h].N != s.H[resid.HistR].N {
					t.Errorf("%s_n=%d, R_n=%d: every timed sample feeds all four segments", resid.HistNames[h], s.H[h].N, s.H[resid.HistR].N)
				}
			}
			if s.H[resid.HistG].N == 0 {
				t.Errorf("no kernel receive stamp (SO_TIMESTAMPNS) was read: G_n=0")
			}
			if s.H[resid.HistWt].N == 0 || s.H[resid.HistB].N == 0 {
				t.Errorf("no sampled loop turn: Wt_n=%d B_n=%d", s.H[resid.HistWt].N, s.H[resid.HistB].N)
			}
			if s.Unst != 0 {
				t.Errorf("unst=%d on an all-inline handler", s.Unst)
			}
			// The open questions of DESIGN-757: no write inside ProcessH2.
			if s.InProc != 0 || s.Early != 0 {
				t.Errorf("a write returned inside ProcessH2: inproc=%d early=%d", s.InProc, s.Early)
			}
			active := 0
			for _, n := range s.Loops {
				if n > 0 {
					active++
				}
			}
			if active == 0 {
				t.Errorf("loops=%v", s.Loops)
			}
			mu.Lock()
			defer mu.Unlock()
			if uint64(len(samples)) != s.N {
				t.Errorf("hook saw %d samples, snapshot n=%d", len(samples), s.N)
			}
			for i, x := range samples {
				if x.Ew > x.Rx || x.Rx > x.Proc || x.Proc > x.Wr || x.K < 1 {
					t.Fatalf("sample %d out of order: T_ew=%d T_rx=%d T_proc=%d T_wr=%d k=%d", i, x.Ew, x.Rx, x.Proc, x.Wr, x.K)
				}
				if x.HasG && x.G < 0 {
					t.Fatalf("sample %d: kernel receive after the read: G=%d", i, x.G)
				}
			}
		})
	}
}

// TestResidHandlerDelay: a 200 us handler is S2 and so R, not G, S1 or S3.
func TestResidHandlerDelay(t *testing.T) {
	h := &residHandler{spin: 200 * time.Microsecond}
	e := startResid(t, true, h)
	runResidLoad(t, e, 2, 2500)
	s := resid.Snapshot()
	if s.N < 20 {
		t.Fatalf("only %d samples", s.N)
	}
	mean := func(i int) float64 { return float64(s.H[i].SumNS) / float64(s.H[i].N) }
	if m := mean(resid.HistS2); m < 200_000 {
		t.Errorf("mean S2 = %.0f ns with a 200 us handler", m)
	}
	if mean(resid.HistR) < mean(resid.HistS2) {
		t.Errorf("mean R %.0f < mean S2 %.0f", mean(resid.HistR), mean(resid.HistS2))
	}
	// R's median bucket is at or above the handler's delay.
	var cum uint64
	for i, c := range s.H[resid.HistR].B {
		if cum += c; cum*2 >= s.H[resid.HistR].N {
			if resid.BucketLo(i) < 128_000 {
				t.Errorf("R median bucket starts at %d ns with a 200 us handler", resid.BucketLo(i))
			}
			break
		}
	}
	if mean(resid.HistS3) > 150_000 {
		t.Errorf("mean S3 = %.0f ns: the write took longer than the handler's delay would explain", mean(resid.HistS3))
	}
}

// TestResidGateOff: with the gate off a loop has no residence state, nothing is
// registered or recorded, and the engine serves as before.
func TestResidGateOff(t *testing.T) {
	resid.Reset()
	t.Cleanup(resid.Reset)
	defer resid.SetEnabled(false)()
	if rs := newResidState(0); rs != nil {
		t.Fatal("newResidState returned a state with the gate off")
	}
	l := newLoop(0, -1, nil, resource.ResolvedResources{}, resource.Config{}, &atomic.Uint64{}, &atomic.Int64{}, nil, &atomic.Uint64{}, &atomic.Bool{}, &atomic.Uint64{}, &atomic.Uint64{}, &atomic.Uint64{}, &atomic.Uint64{})
	if l.resid != nil {
		t.Error("a loop built with the gate off holds residence state")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	h := &residHandler{}
	e, err := New(resource.Config{Addr: addr, Protocol: engine.H2C, Resources: resource.Resources{Workers: 2}}, h)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	defer func() { cancel(); <-done }() //nolint:errcheck // Listen's result is not under test here
	for dl := time.Now().Add(20 * time.Second); (e.Addr() == nil || e.NumWorkers() == 0) && time.Now().Before(dl); {
		time.Sleep(5 * time.Millisecond)
	}
	runResidLoad(t, e, 2, 1000)
	if h.n.Load() != 2000 {
		t.Fatalf("handler ran %d times", h.n.Load())
	}
	s := resid.Snapshot()
	if s.N != 0 || len(s.Loops) != 0 || s.H[resid.HistR].N != 0 || s.H[resid.HistWt].N != 0 {
		t.Errorf("a gate-off engine recorded: %s", s.Line(0))
	}
}
