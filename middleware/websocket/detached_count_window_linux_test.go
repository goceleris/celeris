//go:build linux

package websocket_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/websocket"
)

// TestDetachedCountUnderRSTDuringUpgrade measures the celeris#549 accounting
// (closed by #551) on a live io_uring engine over real TCP, the way
// celeris#584 asks for it: it drives the engine into the window where a
// connection has been Detach()ed by the WebSocket upgrade but the worker's
// deferred detachedCount increment has not run yet, closes the connection
// inside that window, and then checks the counter against the number of
// detached connections that are provably alive.
//
// Reaching the window (both adversarial reviews on #584 are binding, and the
// first cut of this test refined them further with a per-CQE trace):
//
//   - A client that reads the 101 and then resets can never enter it: its RST
//     is causally after OnDetach's enqueue, so it lands in a later completion
//     batch, after the end-of-pass drainDetachQueue has counted the conn.
//   - Nor can a fresh (unpromoted) conn, whatever it does: the adaptive /ws
//     upgrade runs INLINE on the worker thread, and with the default
//     single-shot recv model the completion that reports the RST (the
//     re-armed recv's -ECONNRESET, or the 101 SEND's -EPIPE) is only
//     submitted at the top of the NEXT pass — after the drain at the end of
//     the pass that detached. The inline burst below is kept as a measured
//     negative (expected 0) so a future change to that ordering is noticed.
//   - The reachable window is the PROMOTED path: a conn that first hit an
//     explicit .Async() route runs its upgrade on its dispatch goroutine,
//     under detachMu. closeConn (worker thread, on the RST completion) sets
//     asyncClosed and then blocks on detachMu; if the goroutine is already
//     past its asyncClosed re-check and inside the upgrade, it detaches
//     (asyncDetachPending=true, Detached=true) and unlocks, and closeConn
//     proceeds with the conn detached-but-not-counted. To make that
//     deterministic instead of a ~10 us race, the trigger conns carry a
//     header that makes CheckOrigin — which runs BEFORE Detach — sleep for
//     dcwOriginStall while the client writes the upgrade and resets without
//     reading. The RST completion is reaped well inside that sleep.
//
// Observables (exported by #584's engine counters):
//
//   - O2 DetachWindowCloses: closes that landed inside the window. MANDATORY
//     > 0 for a run to count — a run with O2 == 0 never entered the window and
//     discriminates nothing (reported as INCONCLUSIVE, never as a pass).
//   - O1 DetachedConnections after the burst == live anchors, where the
//     anchors are K >= 8 x Workers long-lived WS conns opened BEFORE the burst
//     and fed a frame every 200 ms. They are load-bearing: the pre-#551 code
//     only decremented when the worker's count was > 0, so without a counted
//     conn on every worker the clamp erases the drift and the negative
//     control cannot fail.
//   - O3 idle-close latency: feeding stops and each anchor times the server's
//     FIN under websocket.Config{IdleTimeout: 1s}. The count gates the idle
//     sweep cadence (0x1F x 50 ms when > 0, 0x3FF x 100 ms when 0), so a
//     drifted-to-zero worker reaps ~100 s late; the fixed tree must reap
//     within IdleTimeout + 1.6 s. Only observable with ReadHeaderTimeout
//     truly disabled: the default 10 s slowloris timeout pins the gate to
//     0x1F and the idle wait to <= 25 ms whatever the count says, and today
//     -1 is re-defaulted to 10 s by the engine (see the Config below), so
//     O3 is a sanity bound, not a discriminator, on both builds.
//
// io_uring only (the count is an io_uring worker field); skipped when the
// engine is unavailable. Runs for ~10 s; skipped under -short.
func TestDetachedCountUnderRSTDuringUpgrade(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement test; skipped under -short")
	}
	t.Run("iouring", func(t *testing.T) {
		detachedCountWindow(t, celeris.IOUring)
	})
}

var detachedCountRun atomic.Int64

const (
	dcwWorkers        = 2
	dcwAnchorsPerW    = 8
	dcwAttempts       = 32 // per trigger path
	dcwOriginStall    = 20 * time.Millisecond
	dcwFeedEvery      = 200 * time.Millisecond
	dcwIdleTimeout    = 1 * time.Second
	dcwIdleCloseBound = 5 * time.Second  // fixed tree: 1 s idle + 0x1F x 50 ms sweep + slack
	dcwIdleCloseCap   = 20 * time.Second // control: 0x3FF x 100 ms ~ 100 s; stop looking here
	dcwStallHeader    = "x-584-origin-stall"
)

func detachedCountWindow(t *testing.T, engine celeris.EngineType) {
	run := detachedCountRun.Add(1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	srv := celeris.New(celeris.Config{
		Engine:        engine,
		AsyncHandlers: true,
		Workers:       dcwWorkers,
		Logger:        slog.New(slog.NewTextHandler(os.Stderr, nil)),
		// -1 is documented to disable the slowloris header timeout (0 means
		// the 10 s default). With the timeout enabled the sweep gate is 0x1F
		// and the idle wait <= 25 ms REGARDLESS of detachedCount (worker.go
		// gate and adaptiveTimeout), so O3 cannot tell a drifted count from
		// a correct one — measured: the control build reaped every anchor
		// within 1.3 s. MEASURED TOO: -1 does not survive to the worker
		// today — Server.doPrepare applies WithDefaults (-1 → 0) and
		// iouring.New applies it AGAIN (0 → 10 s), so the worker still sees
		// 10 s (traced w.cfg.ReadHeaderTimeout=10s). Kept so O3 starts to
		// discriminate once that double normalisation is fixed; until then
		// O3 is a non-discriminating sanity bound on both builds.
		ReadHeaderTimeout: -1,
	})
	// Adaptive route (inherits AsyncHandlers, no explicit .Async()), like the
	// refapp's /ws: runs inline on an unpromoted conn, on the dispatch
	// goroutine on a promoted one. CheckOrigin runs before Detach; the
	// trigger conns ask it to stall so the RST is reaped mid-upgrade.
	srv.GET("/ws", websocket.New(websocket.Config{
		CheckOrigin: func(c *celeris.Context) bool {
			if c.Header(dcwStallHeader) != "" {
				time.Sleep(dcwOriginStall)
			}
			return true
		},
		IdleTimeout: dcwIdleTimeout,
		Handler: func(c *websocket.Conn) {
			for {
				mt, msg, err := c.ReadMessage()
				if err != nil {
					return
				}
				if err := c.WriteMessage(mt, msg); err != nil {
					return
				}
			}
		},
	}))
	// Explicit sync: warms an inline (unpromoted) trigger conn.
	srv.GET("/sync", func(c *celeris.Context) error {
		return c.String(200, "ok")
	}).Sync()
	// Explicit async: the first request on a conn that hits it promotes the
	// conn to its dispatch goroutine for every later request.
	srv.GET("/promote", func(c *celeris.Context) error {
		return c.String(200, "ok")
	}).Async()

	ctx, cancel := context.WithCancel(context.Background())
	var startErr atomic.Pointer[error]
	done := make(chan struct{})
	go func() {
		defer close(done)
		if e := srv.StartWithListenerAndContext(ctx, ln); e != nil {
			startErr.Store(&e)
		}
	}()
	time.Sleep(500 * time.Millisecond)
	if p := startErr.Load(); p != nil {
		msg := (*p).Error()
		if strings.Contains(msg, "io_uring") || strings.Contains(msg, "not available") {
			t.Skipf("engine unavailable on this runner: %v", *p)
		}
		t.Fatalf("server start: %v", *p)
	}
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}()
	metrics := func() celeris.EngineMetrics {
		info := srv.EngineInfo()
		if info == nil {
			t.Fatal("EngineInfo() == nil after start")
		}
		return info.Metrics
	}

	upgradeHdr := "GET /ws HTTP/1.1\r\nHost: " + addr + "\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	upgradeReq := []byte(upgradeHdr + "\r\n")
	upgradeStallReq := []byte(upgradeHdr + dcwStallHeader + ": 1\r\n\r\n")
	syncReq := []byte("GET /sync HTTP/1.1\r\nHost: " + addr + "\r\n\r\n")
	promoteReq := []byte("GET /promote HTTP/1.1\r\nHost: " + addr + "\r\n\r\n")

	// --- anchors -----------------------------------------------------------
	const anchors = dcwAnchorsPerW * dcwWorkers
	as := make([]*dcwAnchor, 0, anchors)
	for i := 0; i < anchors; i++ {
		a, err := dcwOpenAnchor(addr, upgradeReq, i)
		if err != nil {
			t.Fatalf("anchor %d: %v", i, err)
		}
		as = append(as, a)
	}
	var feedWG sync.WaitGroup
	stopFeed := make(chan struct{})
	for _, a := range as {
		feedWG.Add(1)
		go a.feed(&feedWG, stopFeed)
	}
	var detachedPre int64
	for i := 0; i < 40; i++ {
		detachedPre = metrics().DetachedConnections
		if detachedPre == anchors {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if detachedPre != anchors {
		t.Errorf("pre-burst: DetachedConnections = %d, want %d anchors", detachedPre, anchors)
	}
	before := metrics()

	// --- burst --------------------------------------------------------------
	attempt := func(promote bool) error {
		a, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return err
		}
		defer func() { _ = a.Close() }()
		tc := a.(*net.TCPConn)
		br := bufio.NewReader(a)
		// Warm the trigger conn with one fully-read request: /sync keeps it
		// inline (unpromoted), /promote moves it to its dispatch goroutine.
		warm := syncReq
		if promote {
			warm = promoteReq
		}
		if _, err := a.Write(warm); err != nil {
			return err
		}
		_ = a.SetReadDeadline(time.Now().Add(2 * time.Second))
		if status, _, err := dcwReadResponse(br); err != nil || !strings.Contains(status, "200") {
			return fmt.Errorf("warm-up %q: status %q err %v", warm[:12], status, err)
		}
		_ = tc.SetLinger(0) // Close() emits RST, not FIN
		// RST during the upgrade: write the request and reset without
		// reading. On the promoted path the stall header keeps the upgrade
		// inside CheckOrigin (before Detach) on the dispatch goroutine while
		// the worker reaps the RST. The inline path must NOT stall: a slow
		// inline run promotes the adaptive /ws route itself (celeris#356),
		// which would silently turn the inline attempts into promoted ones.
		req := upgradeReq
		if promote {
			req = upgradeStallReq
		}
		if _, err := a.Write(req); err != nil {
			return err
		}
		time.Sleep(500 * time.Microsecond)
		_ = a.Close()
		// Let the (stalled) upgrade finish before the next attempt so the
		// attempts do not pile up on the two workers.
		time.Sleep(dcwOriginStall + 5*time.Millisecond)
		return nil
	}
	var inlineErrs, promotedErrs int
	for i := 0; i < dcwAttempts; i++ {
		if err := attempt(false); err != nil {
			inlineErrs++
			t.Logf("inline attempt %d: %v", i, err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	afterInline := metrics()
	for i := 0; i < dcwAttempts; i++ {
		if err := attempt(true); err != nil {
			promotedErrs++
			t.Logf("promoted attempt %d: %v", i, err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	afterPromoted := metrics()

	o2Inline := afterInline.DetachWindowCloses - before.DetachWindowCloses
	o2Promoted := afterPromoted.DetachWindowCloses - afterInline.DetachWindowCloses
	inlinePromotedDelta := afterInline.AsyncPromotedConns - before.AsyncPromotedConns
	promotedDelta := afterPromoted.AsyncPromotedConns - afterInline.AsyncPromotedConns

	// --- O1 -------------------------------------------------------------------
	// Probe every anchor once more so "live" is a server-visible fact: an
	// anchor that fails its probe is closed (RST) and its server side is torn
	// down before the count is read.
	close(stopFeed)
	feedWG.Wait()
	live := 0
	for _, a := range as {
		if a.probe() {
			live++
		} else {
			_ = a.c.Close()
		}
	}
	time.Sleep(500 * time.Millisecond)
	detachedAfter := metrics().DetachedConnections
	tStop := time.Now()

	// --- O3 -------------------------------------------------------------------
	var closeWG sync.WaitGroup
	closeLat := make([]time.Duration, len(as))
	for i, a := range as {
		if !a.alive.Load() {
			closeLat[i] = -1
			continue
		}
		closeWG.Add(1)
		go func(i int, a *dcwAnchor) {
			defer closeWG.Done()
			closeLat[i] = a.waitClosed(tStop, dcwIdleCloseCap)
		}(i, a)
	}
	closeWG.Wait()
	var maxLat time.Duration
	unobserved := 0
	for _, d := range closeLat {
		switch {
		case d < 0:
			continue
		case d >= dcwIdleCloseCap:
			unobserved++
		case d > maxLat:
			maxLat = d
		}
	}

	verdict := "PASS"
	if o2Inline+o2Promoted == 0 {
		verdict = "INCONCLUSIVE"
		t.Errorf("O2: DetachWindowCloses did not advance in %d+%d attempts; the window was never entered, run is INCONCLUSIVE", dcwAttempts, dcwAttempts)
	}
	if promotedDelta < uint64(dcwAttempts-promotedErrs) {
		t.Errorf("pre-flight: AsyncPromotedConns advanced by %d, want >= %d (the promoted-path upgrades did not run on the dispatch goroutine)", promotedDelta, dcwAttempts-promotedErrs)
	}
	if detachedAfter != int64(live) {
		verdict = "FAIL"
		t.Errorf("O1: DetachedConnections = %d after the burst, want %d (live anchors); detach_window_closes=%d", detachedAfter, live, o2Inline+o2Promoted)
	}
	if unobserved > 0 || maxLat > dcwIdleCloseBound {
		verdict = "FAIL"
		t.Errorf("O3: idle close of the anchors: max %v, %d unobserved within %v (bound %v)", maxLat, unobserved, dcwIdleCloseCap, dcwIdleCloseBound)
	}
	t.Logf("RESULT584 run=%d engine=%s workers=%d multishot_recv=%q anchors=%d live_anchors=%d detached_pre=%d detached_after=%d o2_inline=%d o2_promoted=%d o2_total=%d inline_promoted_delta=%d promoted_delta=%d inline_errs=%d promoted_errs=%d idle_close_max_ms=%d idle_close_unobserved=%d verdict=%s",
		run, engine, dcwWorkers, os.Getenv("CELERIS_IOURING_MULTISHOT_RECV"), anchors, live, detachedPre, detachedAfter, o2Inline, o2Promoted, o2Inline+o2Promoted, inlinePromotedDelta, promotedDelta, inlineErrs, promotedErrs, maxLat.Milliseconds(), unobserved, verdict)
	lats := make([]string, 0, len(closeLat))
	for _, d := range closeLat {
		lats = append(lats, strconv.FormatInt(d.Milliseconds(), 10))
	}
	t.Logf("RESULT584 run=%d idle_close_ms_per_anchor=%s", run, strings.Join(lats, ","))
}

// dcwAnchor is one long-lived WebSocket conn opened before the burst.
type dcwAnchor struct {
	id    int
	c     *net.TCPConn
	br    *bufio.Reader
	alive atomic.Bool
}

func dcwOpenAnchor(addr string, upgradeReq []byte, id int) (*dcwAnchor, error) {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return nil, err
	}
	tc := c.(*net.TCPConn)
	br := bufio.NewReader(c)
	if _, err := c.Write(upgradeReq); err != nil {
		return nil, err
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	status, _, err := dcwReadResponse(br)
	if err != nil || !strings.Contains(status, "101") {
		return nil, fmt.Errorf("upgrade: status %q err %v", status, err)
	}
	a := &dcwAnchor{id: id, c: tc, br: br}
	a.alive.Store(true)
	return a, nil
}

// roundTrip sends one masked text frame and reads the echo.
func (a *dcwAnchor) roundTrip(payload string) error {
	if _, err := a.c.Write(dcwClientFrame(payload)); err != nil {
		return err
	}
	_ = a.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	op, got, err := dcwReadFrame(a.br)
	if err != nil {
		return err
	}
	if op != 0x1 || string(got) != payload {
		return fmt.Errorf("echo mismatch: op=%#x payload=%q", op, got)
	}
	return nil
}

func (a *dcwAnchor) feed(wg *sync.WaitGroup, stop <-chan struct{}) {
	defer wg.Done()
	tick := time.NewTicker(dcwFeedEvery)
	defer tick.Stop()
	n := 0
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		n++
		if err := a.roundTrip("anchor-" + strconv.Itoa(a.id) + "-" + strconv.Itoa(n)); err != nil {
			a.alive.Store(false)
			return
		}
	}
}

func (a *dcwAnchor) probe() bool {
	if !a.alive.Load() {
		return false
	}
	if err := a.roundTrip("probe-" + strconv.Itoa(a.id)); err != nil {
		a.alive.Store(false)
		return false
	}
	return true
}

// waitClosed blocks until the server closes the conn and returns the latency
// since t0, or capDur if nothing arrived by then.
func (a *dcwAnchor) waitClosed(t0 time.Time, capDur time.Duration) time.Duration {
	_ = a.c.SetReadDeadline(t0.Add(capDur))
	for {
		_, _, err := dcwReadFrame(a.br)
		if err == nil {
			continue // a close frame is not a FIN; keep reading
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return capDur
		}
		return time.Since(t0)
	}
}

// dcwReadResponse reads one HTTP/1.1 response (status line, headers, and a
// Content-Length body if any) from br.
func dcwReadResponse(br *bufio.Reader) (status string, body []byte, err error) {
	status, err = br.ReadString('\n')
	if err != nil {
		return "", nil, err
	}
	status = strings.TrimSpace(status)
	contentLength := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return status, nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			contentLength, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	if contentLength > 0 {
		body = make([]byte, contentLength)
		if _, err := io.ReadFull(br, body); err != nil {
			return status, nil, err
		}
	}
	return status, body, nil
}

// dcwClientFrame builds a masked client text frame (mask key 0 → payload as
// is), payload < 126 bytes.
func dcwClientFrame(payload string) []byte {
	f := make([]byte, 0, 6+len(payload))
	f = append(f, 0x81, 0x80|byte(len(payload)), 0, 0, 0, 0)
	return append(f, payload...)
}

// dcwReadFrame reads one unmasked server frame with a payload < 126 bytes.
func dcwReadFrame(br *bufio.Reader) (opcode byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(hdr[1] & 0x7f)
	if n >= 126 {
		return 0, nil, fmt.Errorf("unexpected frame length %d", n)
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(br, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0] & 0x0f, payload, nil
}
