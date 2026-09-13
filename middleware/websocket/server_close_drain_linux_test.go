//go:build linux && celeris_closeprobe

package websocket

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/sockopts"
	"github.com/goceleris/celeris/probe"
	"golang.org/x/sys/unix"
)

// TestServerInitiatedCloseDrainFINvsRST is Tier 1 of celeris#583: does the
// pre-close recv drain (sockopts.DrainRecvBuffer, run between SHUT_WR and
// close(2) on every truly-detached WS/SSE close) change what the peer sees,
// measured on the only path where the drain has something to drain — a
// SERVER-initiated detached close on a peer that is still sending and not
// reading. The two checked-in backpressure oracles close only after the
// client's Close frame has been parsed, i.e. with an empty receive queue,
// and cannot discriminate (the adversarial-review corrections on #583).
//
// Per connection the handler reads and echoes echoFrames frames, then stops
// reading. The client keeps flooding until the chanReader crosses its
// high-water mark and requests the engine pause (verified through the
// chanReader's own pausedState, the existing pause bookkeeping); from that
// point every byte the client sends stays in the kernel receive queue.
// Two inbound cells:
//
//   - small: after the pause the client sends <= 16 KiB more (below the
//     32 KiB drain cap) and stops;
//   - flood: the client keeps writing until its write blocks (the server's
//     autotuned receive buffer is full: far above the cap).
//
// The client never reads during the flood and has an 8 KiB SO_RCVBUF, so
// the server's echo backlog and its Close frame are staged in the kernel
// send buffer at close (outq > 1) — the "full by construction" send buffer
// the closure's claim (3) is about. Then the client tells the handler to
// return; the middleware writes Close(1000) and asks the engine to drop the
// fd; the engine runs SHUT_WR -> CloseDrain -> close(2). CloseDrain (built
// with -tags celeris_closeprobe, CELERIS_DEBUG_CLOSE_PROBE=1) reports
// SIOCINQ before/after the drain, the bytes drained and SIOCOUTQ just
// before close(2), keyed by the peer address; the client waits for that
// record (server raddr == client laddr), then reads to its terminal result
// and samples SO_ERROR. The drain-off arm is the same binary started with
// CELERIS_DEBUG_SKIP_CLOSE_DRAIN=1, which makes CloseDrain return without
// reading; nothing else differs.
//
// Validity gate (binding): a cell whose P(inq_before>0) over its joined
// closes is below 20% is UNINFORMATIVE and is labelled so in its
// TIER1-CELL line; it must not be cited for the closure's links.
//
// Handler goroutines never touch t; every anomaly is a counter printed and
// asserted after the engine has been shut down.
func TestServerInitiatedCloseDrainFINvsRST(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the docker harness with the celeris_closeprobe tag")
	}
	p := t1Params{
		conns:        envInt("WS583_CONNS", 32),
		echoFrames:   envInt("WS583_ECHO_FRAMES", 256),
		bpBuf:        envInt("WS583_BP", 64),
		postPause:    envInt("WS583_POSTPAUSE_BYTES", 16<<10),
		clientRcvBuf: envInt("WS583_CLIENT_RCVBUF", 8<<10),
		probeOn:      sockopts.CloseProbeEnabled(),
		arm:          "drain_on",
	}
	if sockopts.CloseDrainSkipped() {
		p.arm = "drain_off"
	}
	cells := []string{"small", "flood"}
	if v := os.Getenv("WS583_CELLS"); v != "" {
		cells = strings.Split(v, ",")
	}
	t.Logf("TIER1-ENV kernel=%s arm=%s probe=%t conns=%d echoFrames=%d bp=%d postPause=%d clientRcvBuf=%d cells=%v",
		kernelReleaseWS(), p.arm, p.probeOn, p.conns, p.echoFrames, p.bpBuf, p.postPause, p.clientRcvBuf, cells)

	for _, kind := range engineKinds(t) {
		kind := kind
		variants := []struct{ name, mshotEnv string }{{kind.String(), ""}}
		if kind == celeris.IOUring {
			// No slash in the name: -run splits patterns on '/', so a
			// "io_uring/multishot_recv" subtest would also match '^io_uring$'.
			variants = append(variants, struct{ name, mshotEnv string }{"io_uring_multishot", "1"})
		}
		for _, v := range variants {
			v := v
			t.Run(v.name, func(t *testing.T) {
				if v.mshotEnv != "" {
					t.Setenv("CELERIS_IOURING_MULTISHOT_RECV", v.mshotEnv)
					pr := probe.Probe()
					if !pr.ProvidedBuffers || pr.IOUringTier < celerisengine.High {
						t.Skip("kernel lacks provided buffers / High tier for multishot recv")
					}
				}
				for _, cell := range cells {
					cell := cell
					t.Run(cell, func(t *testing.T) {
						runT1Cell(t, kind, v.name, cell, p)
					})
				}
			})
		}
	}
}

type t1Params struct {
	conns, echoFrames, bpBuf, postPause, clientRcvBuf int
	probeOn                                           bool
	arm                                               string
}

const (
	t1Plen        = 120
	t1ClientFrame = 6 + t1Plen // masked client frame: 2 hdr + 4 mask + payload
	t1ServerFrame = 2 + t1Plen // unmasked echo: 2 hdr + payload
	t1CloseFrame  = 4          // Close(1000): 2 hdr + 2 status
	// t1Phase2Cap bounds the bytes a client sends while waiting for the
	// pause request; a pause that never comes is reported, not hung on.
	t1Phase2Cap = 16 << 20
	// t1FloodCap bounds the flood cell (the write is expected to block long
	// before this).
	t1FloodCap = 64 << 20
)

type t1Slot struct {
	ws          atomic.Pointer[Conn]
	stopped     chan struct{} // handler stopped reading (closed on every handler exit)
	stoppedOK   atomic.Bool   // ... after echoFrames clean reads
	closeNow    chan struct{} // client -> handler: return now
	probe       chan sockopts.CloseProbe
	echoed      atomic.Int64
	handlerDone atomic.Bool
}

type t1Term struct {
	idx          int
	laddr        string
	kind         string // EOF / ECONNRESET / timeout / <other>
	soErr        string
	bytesRx      int
	echoRx       int
	closeRx      bool
	truncTail    bool
	sentBytes    int
	pauseSeen    bool
	writeBlocked bool
	writeErr     string
	probeSeen    bool
	probeWait    time.Duration
	spilled      uint64
	chDepth      int  // chanReader channel depth at close request
	spillLen     int  // chanReader spill depth at close request
	pausedState  bool // chanReader pausedState at close request
	highWater    int
	phase2Sent   int // bytes written after the handler stopped, before the pause was seen
	rec          sockopts.CloseProbe
	fail         string // harness-level failure for this conn
}

func runT1Cell(t *testing.T, kind celeris.EngineType, engineName, cell string, p t1Params) {
	netBefore := readTCPExtWS(t)

	slots := make([]t1Slot, p.conns)
	for i := range slots {
		slots[i].stopped = make(chan struct{})
		slots[i].closeNow = make(chan struct{})
		slots[i].probe = make(chan sockopts.CloseProbe, 1)
	}
	var addrMu sync.Mutex
	addrToIdx := map[string]int{}
	var unjoinedProbes, dupProbes, ignoredProbes atomic.Int64
	var probeMu sync.Mutex
	var unjoined []sockopts.CloseProbe
	hook := func(rec sockopts.CloseProbe) {
		addrMu.Lock()
		idx, ok := addrToIdx[rec.RAddr]
		addrMu.Unlock()
		if ok && idx < 0 {
			ignoredProbes.Add(1)
			return
		}
		if !ok {
			unjoinedProbes.Add(1)
			probeMu.Lock()
			if len(unjoined) < 8 {
				unjoined = append(unjoined, rec)
			}
			probeMu.Unlock()
			return
		}
		select {
		case slots[idx].probe <- rec:
		default:
			dupProbes.Add(1)
		}
	}
	sockopts.CloseProbeHook.Store(&hook)
	defer sockopts.CloseProbeHook.Store(nil)

	var detailMu sync.Mutex
	var details []string
	note := func(format string, args ...any) {
		detailMu.Lock()
		if len(details) < 24 {
			details = append(details, fmt.Sprintf(format, args...))
		}
		detailMu.Unlock()
	}

	var handlerReadErr, handlerBadFrame, handlerEchoErr, handlerStopped, closeNowTimeout atomic.Int64
	var handlerWG sync.WaitGroup
	expectedPad := strings.Repeat("x", t1Plen-16)

	s := celeris.New(celeris.Config{Engine: kind})
	s.GET("/ws", New(Config{
		CheckOrigin:           func(*celeris.Context) bool { return true },
		ReadLimit:             256 * 1024,
		MaxBackpressureBuffer: p.bpBuf,
		Handler: func(c *Conn) {
			handlerWG.Add(1)
			defer handlerWG.Done()
			idx := -1
			var once sync.Once
			stop := func() {
				if idx >= 0 {
					once.Do(func() { close(slots[idx].stopped) })
				}
			}
			defer func() {
				if idx >= 0 {
					slots[idx].handlerDone.Store(true)
				}
				stop()
			}()
			for i := 0; i < p.echoFrames; i++ {
				mt, msg, err := c.ReadMessage()
				if err != nil {
					handlerReadErr.Add(1)
					note("conn %d: handler read error after %d frames: %v", idx, i, err)
					return
				}
				if len(msg) != t1Plen || string(msg[16:]) != expectedPad {
					handlerBadFrame.Add(1)
					note("conn %d: bad frame len=%d", idx, len(msg))
					return
				}
				cIdx := int(binary.BigEndian.Uint64(msg[8:16]))
				if cIdx < 0 || cIdx >= p.conns {
					handlerBadFrame.Add(1)
					return
				}
				if idx < 0 {
					idx = cIdx
					slots[idx].ws.Store(c)
				} else if idx != cIdx {
					handlerBadFrame.Add(1)
					note("conn %d: frame claimed conn %d", idx, cIdx)
					return
				}
				if err := c.WriteMessage(mt, msg); err != nil {
					handlerEchoErr.Add(1)
					note("conn %d: echo failed after %d frames: %v", idx, i, err)
					return
				}
				slots[idx].echoed.Add(1)
			}
			// Stop reading. Everything the client sends from here on queues
			// in the chanReader until it requests the pause, then in the
			// kernel receive buffer.
			handlerStopped.Add(1)
			slots[idx].stoppedOK.Store(true)
			stop()
			select {
			case <-slots[idx].closeNow:
			case <-time.After(60 * time.Second):
				closeNowTimeout.Add(1)
			}
			// Returning is the server-initiated close: the middleware's
			// deferred block writes Close(1000) and calls ws.Close(), which
			// asks the engine to drop the fd on its next sweep.
		},
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, serverCancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.StartWithListenerAndContext(serverCtx, ln) }()
	// Readiness: dial until the listener answers. Each probe dial is a
	// plain TCP conn the engine also closes through the drain path (no
	// protocol was ever seen on it), so its address is registered as
	// ignored rather than counted as an unjoined record.
	addr := ""
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if a := s.Addr(); a != nil {
			c, err := net.DialTimeout("tcp", a.String(), 100*time.Millisecond)
			if err == nil {
				addrMu.Lock()
				addrToIdx[c.LocalAddr().String()] = -1
				addrMu.Unlock()
				_ = c.Close()
				addr = a.String()
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("server not ready within 30s")
	}
	workers := -1
	if info := s.EngineInfo(); info != nil {
		workers = info.Metrics.Workers
	}
	t.Logf("TIER1-SERVER engine=%s cell=%s arm=%s addr=%s workers=%d gomaxprocs=%d", engineName, cell, p.arm, addr, workers, runtime.GOMAXPROCS(0))

	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			serverCancel()
			select {
			case <-serverDone:
			case <-time.After(20 * time.Second):
				t.Errorf("engine shutdown did not complete within 20s")
			}
		})
	}
	defer shutdown()

	hostPort := addr
	dialer := net.Dialer{Timeout: 3 * time.Second, Control: func(_, _ string, rc syscall.RawConn) error {
		var serr error
		_ = rc.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, p.clientRcvBuf)
		})
		return serr
	}}

	terms := make([]t1Term, p.conns)
	var wg sync.WaitGroup
	for i := range p.conns {
		id := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			tm := &terms[id]
			tm.idx = id
			c, err := dialer.Dial("tcp", hostPort)
			if err != nil {
				tm.fail = "dial: " + err.Error()
				return
			}
			defer func() { _ = c.Close() }()
			tm.laddr = c.LocalAddr().String()
			addrMu.Lock()
			addrToIdx[tm.laddr] = id
			addrMu.Unlock()
			if err := wsHandshake(c, hostPort); err != nil {
				tm.fail = "handshake: " + err.Error()
				return
			}
			// Frames are whole; sentBytes counts what the kernel accepted.
			var seq uint64
			nextFrames := func(n int) []byte {
				buf := make([]byte, 0, n*t1ClientFrame)
				for range n {
					buf = append(buf, t1EncodeFrame(seq, uint64(id))...)
					seq++
				}
				return buf
			}
			// Phase 1: the frames the handler reads and echoes.
			if !writeAll(c, nextFrames(p.echoFrames), 15*time.Second) {
				tm.fail = "phase1 write"
				return
			}
			tm.sentBytes += p.echoFrames * t1ClientFrame
			select {
			case <-slots[id].stopped:
			case <-time.After(30 * time.Second):
				tm.fail = "handler never stopped"
				return
			}
			if !slots[id].stoppedOK.Load() {
				tm.fail = "handler exited early"
				// The conn is closing on its own; still collect the close.
			} else {
				ws := slots[id].ws.Load()
				// Phase 2: flood in small paced pieces until the chanReader
				// asks the engine to pause recv. Each piece is allowed to
				// drain out of the client's send buffer (SIOCOUTQ==0) before
				// the next, so nothing is queued on the client side when the
				// pause lands and the post-pause bytes are the only bytes
				// that reach the server's queue afterwards.
				p2start := tm.sentBytes
				for tm.sentBytes-p2start < t1Phase2Cap {
					chunk := nextFrames(8)
					_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
					n, err := c.Write(chunk)
					tm.sentBytes += n
					if err != nil {
						if errors.Is(err, os.ErrDeadlineExceeded) {
							tm.writeBlocked = true
						} else {
							tm.writeErr = err.Error()
						}
						break
					}
					if readerPaused(ws) {
						tm.pauseSeen = true
						break
					}
					if !waitClientOutq(c, 200*time.Millisecond) {
						// The window closed without a pause request: the
						// engine stopped reading on its own. Recorded, not
						// hidden — see chDepth/pausedState in the record.
						tm.writeBlocked = true
						break
					}
				}
				tm.phase2Sent = tm.sentBytes - p2start
				if tm.pauseSeen && tm.writeErr == "" {
					// Everything in flight lands (in the chanReader or the
					// kernel queue), then the worker applies the pause on its
					// next loop pass, before the measured bytes go out.
					_ = waitClientOutq(c, 500*time.Millisecond)
					time.Sleep(100 * time.Millisecond)
					switch cell {
					case "small":
						frames := p.postPause / t1ClientFrame
						chunk := nextFrames(frames)
						_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
						n, err := c.Write(chunk)
						tm.sentBytes += n
						if err != nil {
							if errors.Is(err, os.ErrDeadlineExceeded) {
								tm.writeBlocked = true
							} else {
								tm.writeErr = err.Error()
							}
						}
						_ = waitClientOutq(c, 500*time.Millisecond)
					case "flood":
						big := nextFrames(512) // 64512 B per write
						for tm.sentBytes < t1FloodCap {
							_ = c.SetWriteDeadline(time.Now().Add(1 * time.Second))
							n, err := c.Write(big)
							tm.sentBytes += n
							if err != nil {
								if errors.Is(err, os.ErrDeadlineExceeded) {
									tm.writeBlocked = true
								} else {
									tm.writeErr = err.Error()
								}
								break
							}
						}
					}
					// Give the last bytes a moment to land in the server's
					// queue so inq_before reflects what was sent.
					time.Sleep(50 * time.Millisecond)
				}
				if ws != nil && ws.engineReader != nil {
					r := ws.engineReader
					tm.spilled = r.Spilled()
					tm.chDepth = len(r.ch)
					tm.spillLen = int(r.spillLen.Load())
					tm.pausedState = readerPaused(ws)
					tm.highWater = r.highWater
				}
			}
			// Server-initiated close.
			t0 := time.Now()
			close(slots[id].closeNow)
			if p.probeOn {
				select {
				case rec := <-slots[id].probe:
					tm.probeSeen = true
					tm.rec = rec
					tm.probeWait = time.Since(t0)
					// The record is delivered immediately BEFORE close(2).
					// Reading right away can drain the server's send buffer
					// before close(2) runs and turn a reset into a clean
					// close on the wire; let the close land first.
					time.Sleep(30 * time.Millisecond)
				case <-time.After(20 * time.Second):
					tm.probeWait = time.Since(t0)
				}
			} else {
				// Probe-off perturbation run: no join key; wait past the
				// engine sweep (100 ms) and the deferred-close bounds.
				time.Sleep(1500 * time.Millisecond)
			}
			// Read to the terminal condition and classify like the oracles.
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			var rx []byte
			buf := make([]byte, 64<<10)
			for {
				n, err := c.Read(buf)
				if n > 0 && len(rx) < 8<<20 {
					rx = append(rx, buf[:n]...)
				}
				tm.bytesRx += n
				if err != nil {
					switch {
					case errors.Is(err, syscall.ECONNRESET):
						tm.kind = "ECONNRESET"
					case errors.Is(err, io.EOF):
						tm.kind = "EOF"
					case errors.Is(err, os.ErrDeadlineExceeded):
						tm.kind = "timeout"
					default:
						tm.kind = err.Error()
					}
					break
				}
			}
			tm.soErr = soErrorConn(c)
			tm.echoRx, tm.closeRx, tm.truncTail = t1ParseServerFrames(rx)
		}()
	}
	wg.Wait()

	// Settle: the handlers return on closeNow; wait for them, snapshot the
	// kernel counters BEFORE shutdown so its own closes do not pollute them,
	// then stop the engine.
	handlersDrained := make(chan struct{})
	go func() { handlerWG.Wait(); close(handlersDrained) }()
	select {
	case <-handlersDrained:
	case <-time.After(70 * time.Second):
		t.Errorf("handlers still running 70s after the clients finished")
	}
	// Every close the probe reports arrives before close(2); allow the
	// deferred-close bound for stragglers before sampling the counters.
	time.Sleep(200 * time.Millisecond)
	netAfter := readTCPExtWS(t)
	shutdown()

	abortOnClose := netAfter["TCPAbortOnClose"] - netBefore["TCPAbortOnClose"]
	abortOnData := netAfter["TCPAbortOnData"] - netBefore["TCPAbortOnData"]

	// Tally from the settled per-connection records.
	type strata struct{ n, eof, rst, timeout, closeRx, fullRx int }
	strat := map[string]*strata{}
	var joined, dialFail, hsFail, harnessFail, probeTimeouts, pauseSeen, blockedNoPause, writeBlocked, writeErrs, stoppedOK int
	var inqBeforePos, inqAfterPos, outqDataPos, eof, rst, timeout, other, closeRx, fullRx, truncTail int
	var soEPIPE, soRST, so0 int
	var inqBeforeMin, inqBeforeMax, drainedMin, drainedMax, inqAfterMin, inqAfterMax, outqMin, outqMax int
	var drainedSum, inqBeforeSum int64
	var spilledSum uint64
	var probeWaitMax time.Duration
	sites := map[string]int{}
	for i := range terms {
		tm := &terms[i]
		if tm.fail != "" {
			switch {
			case strings.HasPrefix(tm.fail, "dial"):
				dialFail++
			case strings.HasPrefix(tm.fail, "handshake"):
				hsFail++
			default:
				harnessFail++
				note("conn %d: %s", i, tm.fail)
			}
		}
		if tm.laddr == "" {
			continue
		}
		if slots[i].stoppedOK.Load() {
			stoppedOK++
		}
		if tm.pauseSeen {
			pauseSeen++
		}
		if tm.writeBlocked {
			writeBlocked++
		}
		if tm.writeBlocked && !tm.pauseSeen {
			blockedNoPause++
			note("conn %d: window closed with no pause request: chDepth=%d/%d spillLen=%d pausedState=%t phase2Sent=%d", i, tm.chDepth, tm.highWater, tm.spillLen, tm.pausedState, tm.phase2Sent)
		}
		if tm.writeErr != "" {
			writeErrs++
			note("conn %d: write error during flood: %s", i, tm.writeErr)
		}
		spilledSum += tm.spilled
		if p.probeOn && !tm.probeSeen {
			probeTimeouts++
		}
		switch tm.kind {
		case "EOF":
			eof++
		case "ECONNRESET":
			rst++
		case "timeout":
			timeout++
		default:
			other++
		}
		switch tm.soErr {
		case "EPIPE":
			soEPIPE++
		case "ECONNRESET":
			soRST++
		case "0":
			so0++
		}
		expectedRx := int(slots[i].echoed.Load())*t1ServerFrame + t1CloseFrame
		full := tm.bytesRx == expectedRx
		if tm.closeRx {
			closeRx++
		}
		if full {
			fullRx++
		}
		if tm.truncTail {
			truncTail++
		}
		if tm.probeSeen {
			r := tm.rec
			first := joined == 0
			joined++
			sites[r.Site]++
			if r.InqBefore > 0 {
				inqBeforePos++
			}
			if r.InqAfter > 0 {
				inqAfterPos++
			}
			if r.Outq > 1 {
				outqDataPos++
			}
			drainedSum += int64(r.Drained)
			inqBeforeSum += int64(r.InqBefore)
			t1MinMax(first, r.InqBefore, &inqBeforeMin, &inqBeforeMax)
			t1MinMax(first, r.Drained, &drainedMin, &drainedMax)
			t1MinMax(first, r.InqAfter, &inqAfterMin, &inqAfterMax)
			t1MinMax(first, r.Outq, &outqMin, &outqMax)
			if tm.probeWait > probeWaitMax {
				probeWaitMax = tm.probeWait
			}
			key := fmt.Sprintf("inqB=%s/inqA=%s/outq=%s", t1Sign(r.InqBefore > 0), t1Sign(r.InqAfter > 0), t1Sign(r.Outq > 1))
			st := strat[key]
			if st == nil {
				st = &strata{}
				strat[key] = st
			}
			st.n++
			switch tm.kind {
			case "EOF":
				st.eof++
			case "ECONNRESET":
				st.rst++
			default:
				st.timeout++
			}
			if tm.closeRx {
				st.closeRx++
			}
			if full {
				st.fullRx++
			}
		}
		t.Logf("TIER1-CONN engine=%s cell=%s arm=%s conn=%d laddr=%s stoppedOK=%t pauseSeen=%t pausedState=%t chDepth=%d/%d spillLen=%d writeBlocked=%t sent=%d phase2Sent=%d spilled=%d probe=%t probeWait=%s site=%s inq_before=%d drained=%d inq_after=%d outq=%d peer=%s soErr=%s bytesRx=%d expectedRx=%d echoRx=%d echoed=%d closeRx=%t truncTail=%t fail=%q",
			engineName, cell, p.arm, i, tm.laddr, slots[i].stoppedOK.Load(), tm.pauseSeen, tm.pausedState, tm.chDepth, tm.highWater, tm.spillLen, tm.writeBlocked, tm.sentBytes, tm.phase2Sent, tm.spilled,
			tm.probeSeen, tm.probeWait.Round(time.Millisecond), tm.rec.Site, tm.rec.InqBefore, tm.rec.Drained, tm.rec.InqAfter, tm.rec.Outq,
			tm.kind, tm.soErr, tm.bytesRx, expectedRx, tm.echoRx, slots[i].echoed.Load(), tm.closeRx, tm.truncTail, tm.fail)
	}
	pInqBefore := 0.0
	if joined > 0 {
		pInqBefore = float64(inqBeforePos) / float64(joined)
	}
	informative := joined > 0 && pInqBefore >= 0.20
	siteStr := ""
	for k, v := range sites {
		siteStr += fmt.Sprintf("%s:%d,", k, v)
	}
	t.Logf("TIER1-CELL engine=%s cell=%s arm=%s probe=%t workers=%d conns=%d joined=%d sites=%s unjoinedProbes=%d ignoredProbes=%d dupProbes=%d probeTimeouts=%d probeWaitMax=%s stoppedOK=%d pauseSeen=%d blockedNoPause=%d writeBlocked=%d writeErrs=%d spilledSum=%d inqBeforePos=%d pInqBefore=%.3f inqBeforeMin=%d inqBeforeMax=%d inqBeforeSum=%d drainedMin=%d drainedMax=%d drainedSum=%d inqAfterPos=%d inqAfterMin=%d inqAfterMax=%d outqDataPos=%d outqMin=%d outqMax=%d EOF=%d RST=%d timeout=%d other=%d soErrEPIPE=%d soErrECONNRESET=%d soErr0=%d closeFrameRx=%d fullRx=%d truncTail=%d handlerReadErr=%d handlerBadFrame=%d handlerEchoErr=%d handlerStopped=%d closeNowTimeout=%d dialFail=%d hsFail=%d harnessFail=%d abortOnClose=%d abortOnData=%d informative=%t",
		engineName, cell, p.arm, p.probeOn, workers, p.conns, joined, siteStr, unjoinedProbes.Load(), ignoredProbes.Load(), dupProbes.Load(), probeTimeouts, probeWaitMax.Round(time.Millisecond),
		stoppedOK, pauseSeen, blockedNoPause, writeBlocked, writeErrs, spilledSum,
		inqBeforePos, pInqBefore, inqBeforeMin, inqBeforeMax, inqBeforeSum, drainedMin, drainedMax, drainedSum,
		inqAfterPos, inqAfterMin, inqAfterMax, outqDataPos, outqMin, outqMax,
		eof, rst, timeout, other, soEPIPE, soRST, so0, closeRx, fullRx, truncTail,
		handlerReadErr.Load(), handlerBadFrame.Load(), handlerEchoErr.Load(), handlerStopped.Load(), closeNowTimeout.Load(),
		dialFail, hsFail, harnessFail, abortOnClose, abortOnData, informative)
	for k, st := range strat {
		t.Logf("TIER1-STRATA engine=%s cell=%s arm=%s %s n=%d EOF=%d RST=%d timeout=%d closeFrameRx=%d fullRx=%d",
			engineName, cell, p.arm, k, st.n, st.eof, st.rst, st.timeout, st.closeRx, st.fullRx)
	}
	probeMu.Lock()
	for _, r := range unjoined {
		t.Logf("TIER1-UNJOINED engine=%s cell=%s site=%s fd=%d raddr=%s inq_before=%d", engineName, cell, r.Site, r.FD, r.RAddr, r.InqBefore)
	}
	probeMu.Unlock()
	detailMu.Lock()
	for _, d := range details {
		t.Logf("%s/%s: %s", engineName, cell, d)
	}
	detailMu.Unlock()

	// Harness integrity only; the FIN/RST outcome is the measurement.
	if dialFail+hsFail > 0 {
		t.Fatalf("environment: %d dial / %d handshake failures", dialFail, hsFail)
	}
	if harnessFail > 0 || handlerReadErr.Load() > 0 || handlerBadFrame.Load() > 0 || handlerEchoErr.Load() > 0 {
		t.Errorf("harness: %d client failures, handler readErr=%d badFrame=%d echoErr=%d", harnessFail, handlerReadErr.Load(), handlerBadFrame.Load(), handlerEchoErr.Load())
	}
	if closeNowTimeout.Load() > 0 {
		t.Errorf("harness: %d handlers never received closeNow", closeNowTimeout.Load())
	}
	if p.probeOn {
		if probeTimeouts > 0 || unjoinedProbes.Load() > 0 {
			t.Errorf("join: %d clients saw no CLOSE-PROBE record within 20s, %d records had no client (the raddr/laddr join is broken or a close took a non-drain path)", probeTimeouts, unjoinedProbes.Load())
		}
		if joined != p.conns {
			t.Errorf("join: %d of %d connections joined", joined, p.conns)
		}
	}
	if pauseSeen != p.conns {
		// A workload property, reported rather than asserted: a conn whose
		// window closed without a pause request still closes with a full
		// kernel queue (its record says so) but is not the designed cell.
		t.Logf("TIER1-NOTE engine=%s cell=%s arm=%s pause request observed on %d of %d connections (%d blocked without one)", engineName, cell, p.arm, pauseSeen, p.conns, blockedNoPause)
	}
	if cell == "flood" && writeBlocked != p.conns {
		t.Errorf("flood: only %d of %d clients saw their write block (window closed = pause applied)", writeBlocked, p.conns)
	}
	if !informative {
		t.Logf("TIER1-VERDICT engine=%s cell=%s arm=%s UNINFORMATIVE: P(inq_before>0)=%.3f < 0.20 over %d joined closes", engineName, cell, p.arm, pInqBefore, joined)
	}
}

func t1EncodeFrame(seq, connID uint64) []byte {
	m := [4]byte{0x11, 0x22, 0x33, 0x44}
	f := make([]byte, t1ClientFrame)
	f[0], f[1] = 0x82, 0x80|byte(t1Plen)
	copy(f[2:6], m[:])
	var payload [t1Plen]byte
	binary.BigEndian.PutUint64(payload[:8], seq)
	binary.BigEndian.PutUint64(payload[8:16], connID)
	for j := 16; j < t1Plen; j++ {
		payload[j] = 'x'
	}
	for j := 0; j < t1Plen; j++ {
		f[6+j] = payload[j] ^ m[j%4]
	}
	return f
}

// t1ParseServerFrames walks the unmasked server->client byte stream and
// counts echo frames and whether a Close frame was seen; truncTail reports
// a partial frame at the end (bytes cut off by a reset).
func t1ParseServerFrames(b []byte) (echo int, closeSeen bool, truncTail bool) {
	for len(b) > 0 {
		if len(b) < 2 {
			return echo, closeSeen, true
		}
		op := b[0] & 0x0f
		l := int(b[1] & 0x7f)
		if b[1]&0x80 != 0 {
			// The server never masks; treat as corruption / truncation.
			return echo, closeSeen, true
		}
		hdr := 2
		switch l {
		case 126:
			if len(b) < 4 {
				return echo, closeSeen, true
			}
			l = int(binary.BigEndian.Uint16(b[2:4]))
			hdr = 4
		case 127:
			if len(b) < 10 {
				return echo, closeSeen, true
			}
			l = int(binary.BigEndian.Uint64(b[2:10]))
			hdr = 10
		}
		if len(b) < hdr+l {
			return echo, closeSeen, true
		}
		switch op {
		case 0x2:
			echo++
		case 0x8:
			closeSeen = true
		}
		b = b[hdr+l:]
	}
	return echo, closeSeen, false
}

// waitClientOutq polls the client's SIOCOUTQ until it is 0 (every byte the
// kernel accepted has been sent and acknowledged) or the deadline passes.
func waitClientOutq(c net.Conn, within time.Duration) bool {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return true
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return true
	}
	deadline := time.Now().Add(within)
	for {
		q := -1
		_ = rc.Control(func(fd uintptr) { q, _ = unix.IoctlGetInt(int(fd), unix.SIOCOUTQ) })
		if q == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

func readerPaused(ws *Conn) bool {
	if ws == nil || ws.engineReader == nil {
		return false
	}
	r := ws.engineReader
	r.pausedMu.Lock()
	p := r.pausedState
	r.pausedMu.Unlock()
	return p
}

func t1Sign(b bool) string {
	if b {
		return "+"
	}
	return "0"
}

func t1MinMax(first bool, v int, lo, hi *int) {
	if first || v < *lo {
		*lo = v
	}
	if first || v > *hi {
		*hi = v
	}
}

func soErrorConn(c net.Conn) string {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return "n/a"
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return "n/a"
	}
	out := "n/a"
	_ = rc.Control(func(fd uintptr) {
		v, err := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_ERROR)
		if err != nil {
			out = "getsockopt:" + err.Error()
			return
		}
		switch syscall.Errno(v) {
		case 0:
			out = "0"
		case syscall.EPIPE:
			out = "EPIPE"
		case syscall.ECONNRESET:
			out = "ECONNRESET"
		default:
			out = syscall.Errno(v).Error()
		}
	})
	return out
}

func kernelReleaseWS() string {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return "?"
	}
	var b []byte
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

// readTCPExtWS parses the TcpExt block of /proc/net/netstat (per network
// namespace, so exclusive to the container the test runs in).
func readTCPExtWS(t *testing.T) map[string]int64 {
	t.Helper()
	f, err := os.Open("/proc/net/netstat")
	if err != nil {
		t.Fatalf("open /proc/net/netstat: %v", err)
	}
	defer func() { _ = f.Close() }()
	out := map[string]int64{}
	sc := bufio.NewScanner(f)
	var names []string
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "TcpExt:") {
			continue
		}
		fields := strings.Fields(line)[1:]
		if names == nil {
			names = fields
			continue
		}
		for i, v := range fields {
			if i < len(names) {
				n, _ := strconv.ParseInt(v, 10, 64)
				out[names[i]] = n
			}
		}
		break
	}
	if _, ok := out["TCPAbortOnClose"]; !ok {
		t.Fatalf("TcpExt TCPAbortOnClose not found in /proc/net/netstat")
	}
	return out
}
