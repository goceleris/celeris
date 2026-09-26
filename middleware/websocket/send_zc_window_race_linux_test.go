//go:build linux && validation

package websocket

// celeris#587: judge the io_uring SEND_ZC send state with the race
// detector, on the interleaving the v1.5.7 send-state audit was about.
//
// The audit's claim is that the SEND_ZC first-completion / notification
// writes to cs.sending, cs.zcNotifPending and cs.zcSentBytes (worker
// thread, engine/iouring/worker.go handleSend) and the detached
// inline-egress guard that reads them (dispatch goroutine, the `guarded`
// closure in initProtocol) are synchronised by cs.detachMu, so the raw
// unix.Write fast path can never interleave with a ring SEND_ZC on the
// wire. Two things kept that claim unmeasured:
//
//  1. On loopback the kernel copies (IORING_NOTIF_USAGE_ZC_COPIED), so the
//     notification lands in the same completion batch as the first
//     completion. The window in which the guard must refuse the fast path
//     is a few hundred nanoseconds wide and celeris#601 measured the
//     guard's witness (IouringInlineGuardBlockedZC) at 0 on an unmodified
//     build. No test ever put a dispatch-goroutine write inside it.
//  2. Nothing showed the race detector is live on this path: a -race run
//     that reports nothing is only evidence if the same run reports a race
//     when the synchronisation is removed.
//
// This test fixes (1) with validation.SetZCNotifDelay, which holds the
// window open on the worker thread, lock-free, for zcWinDelay (the NIC
// DMA latency loopback lacks), while a full-duplex 64 KiB echo keeps the
// handler goroutine writing through `guarded` the whole time. It asserts
// the window was entered (the guard declined the fast path with a
// notification outstanding) and that every echoed byte came back intact
// and in order. Run under -race it is also the detector control for (2):
// the mandatory mutant deletes the detachMu acquire in handleSend's
// CQE_F_MORE branch, and this test must then FAIL with a DATA RACE report
// on that write against the guard's read. The mutant is applied by a
// script, never committed (see the PR and the ci.yml zc-window job).
//
// Arms, all read from the environment so one binary serves every run:
//
//	CELERIS587_ZC_NOTIF_DELAY  window hold (default 2ms). "0" is the
//	                           natural-window arm: the counts are logged,
//	                           the guard is not required to fire.
//	CELERIS_IOURING_SEND_ZC=off  branch control: no ZC send may be armed,
//	                           the guard witness must stay 0, and the same
//	                           echo must still be byte-intact.
//
// Needs -tags=validation (the witnesses and the hold are validation-only)
// and an io_uring engine; CELERIS_REQUIRE_IOURING_WORKERS=1 turns the
// io_uring-unavailable skip into a failure.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/validation"
)

const (
	// zcWinFrames 64 KiB frames each way: 16 MiB, far under the 64 MiB
	// detached send cap, far over any loopback socket buffer once the
	// client's receive window is clamped.
	zcWinFrames = 256
	// zcWinReadPace is slept after every frame the client reads, so the
	// server's socket stays congested and ring sends (not the inline fast
	// path) carry the echo.
	zcWinReadPace = 300 * time.Microsecond
	// zcWinDefaultDelay is the default window hold. It is two orders of
	// magnitude above the natural (copy-fallback) window and small enough
	// that a few hundred notifications cost well under a second.
	zcWinDefaultDelay = 2 * time.Millisecond
)

func zcWinDelay(t *testing.T) time.Duration {
	t.Helper()
	v := os.Getenv("CELERIS587_ZC_NOTIF_DELAY")
	if v == "" {
		return zcWinDefaultDelay
	}
	if v == "0" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		t.Fatalf("CELERIS587_ZC_NOTIF_DELAY=%q: want a Go duration >= 0", v)
	}
	return d
}

func zcWinPayload(seq int) []byte {
	p := make([]byte, zcBigFrame)
	binary.BigEndian.PutUint64(p[:8], uint64(seq))
	for j := 8; j < len(p); j++ {
		p[j] = byte(seq) + byte(j)
	}
	return p
}

// zcWinCheck returns a non-nil error naming the first way payload differs
// from the frame the client sent as seq.
func zcWinCheck(seq int, payload []byte) error {
	if len(payload) != zcBigFrame {
		return fmt.Errorf("frame %d: len=%d, want %d", seq, len(payload), zcBigFrame)
	}
	if got := binary.BigEndian.Uint64(payload[:8]); got != uint64(seq) {
		return fmt.Errorf("frame %d: carries seq %d (reordered, duplicated or interleaved)", seq, got)
	}
	for j := 8; j < len(payload); j++ {
		if payload[j] != byte(seq)+byte(j) {
			return fmt.Errorf("frame %d: corrupt at byte %d", seq, j)
		}
	}
	return nil
}

// TestSendZCWindowGuardUnderRace is the celeris#587 measurement. See the
// file comment for the arms.
func TestSendZCWindowGuardUnderRace(t *testing.T) {
	if !zcHasIOUring(t) {
		if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
			t.Fatal("io_uring is not usable here and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids the skip")
		}
		t.Skip("io_uring not usable at High+ tier on this kernel")
	}
	policyOff := zcPolicyOff()
	delay := zcWinDelay(t)
	validation.SetZCNotifDelay(delay)
	defer validation.SetZCNotifDelay(0)

	cfg := Config{Handler: func(c *Conn) {
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	}}
	addr, stop, srv := startNativeServerWithHandle(t, celeris.IOUring, cfg)
	defer stop()
	metrics := func() celeris.EngineMetrics {
		info := srv.EngineInfo()
		if info == nil {
			t.Fatal("EngineInfo() == nil after start")
		}
		return info.Metrics
	}

	client := zcDialClamped(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")

	base := metrics()
	vBase := validation.Snapshot()

	// Sender: every frame back to back. It never reads, so the handler's
	// echoes pile up behind the clamped receive window while it keeps
	// calling `guarded` for each new frame it reads.
	var wg sync.WaitGroup
	sendErr := make(chan error, 1)
	wg.Go(func() {
		for i := 0; i < zcWinFrames; i++ {
			if err := client.writeClientFrame(true, OpBinary, zcWinPayload(i)); err != nil {
				sendErr <- fmt.Errorf("send frame %d: %w", i, err)
				return
			}
			if err := client.bw.Flush(); err != nil {
				sendErr <- fmt.Errorf("flush frame %d: %w", i, err)
				return
			}
		}
	})

	// Reader: paced, and every echo verified in order.
	var readErr error
	got := 0
	deadline := time.Now().Add(90 * time.Second)
	for got < zcWinFrames {
		if err := client.conn.SetReadDeadline(deadline); err != nil {
			readErr = err
			break
		}
		fin, op, payload, err := readServerFrameNonFatal(client.br)
		if err != nil {
			readErr = fmt.Errorf("read echo %d/%d: %w", got, zcWinFrames, err)
			break
		}
		if !fin || op != OpBinary {
			readErr = fmt.Errorf("echo %d: fin=%v op=%d, want a final binary frame", got, fin, op)
			break
		}
		if err := zcWinCheck(got, payload); err != nil {
			readErr = err
			break
		}
		got++
		time.Sleep(zcWinReadPace)
	}
	wg.Wait()
	select {
	case err := <-sendErr:
		readErr = errors.Join(readErr, err)
	default:
	}

	// Ring bytes and notifications are published on the worker's
	// per-iteration cadence; give it a pass (plus the hold) to settle.
	time.Sleep(300*time.Millisecond + 4*delay)
	m := metrics()
	v := validation.Snapshot()
	submits := v.IouringSendZCSubmits - vBase.IouringSendZCSubmits
	detached := v.IouringSendZCSubmitsDetached - vBase.IouringSendZCSubmitsDetached
	notifs := v.IouringSendZCNotifs - vBase.IouringSendZCNotifs
	blocked := v.IouringInlineGuardBlockedZC - vBase.IouringInlineGuardBlockedZC
	pendWrite := v.IouringZCCompletionWithPendingWrite - vBase.IouringZCCompletionWithPendingWrite
	inline := m.InlineBytes - base.InlineBytes
	ring := m.RingBytes - base.RingBytes
	// One line, stable keys: the evidence scripts tally it.
	t.Logf("ZCWIN policy_off=%v delay=%s frames=%d/%d submits=%d detached=%d notifs=%d guard_blocked_zc=%d zc_completion_with_pending_write=%d inline_bytes=%d ring_bytes=%d",
		policyOff, delay, got, zcWinFrames, submits, detached, notifs, blocked, pendWrite, inline, ring)

	if readErr != nil {
		t.Fatalf("echo oracle: %v", readErr)
	}
	if ring == 0 {
		t.Fatalf("RingBytes did not move: the echo never left the inline fast path, so no ring send (and no SEND_ZC) was exercised")
	}
	if policyOff {
		// Branch control: the same echo with the ZC arm disabled. The
		// witnesses must track SEND_ZC, not traffic.
		if submits != 0 || detached != 0 || notifs != 0 || blocked != 0 || pendWrite != 0 {
			t.Fatalf("CELERIS_IOURING_SEND_ZC=off: ZC witnesses moved (submits=%d detached=%d notifs=%d guard_blocked_zc=%d pending_write=%d), want all 0",
				submits, detached, notifs, blocked, pendWrite)
		}
		return
	}
	if detached == 0 {
		t.Fatalf("no SEND_ZC was armed on the detached connection (submits=%d): the echo did not reach the zero-copy arm", submits)
	}
	if detached != submits {
		t.Errorf("detached ZC submits %d != total %d: every ZC send here is on the one detached connection", detached, submits)
	}
	if notifs > submits {
		t.Errorf("ZC notifications %d > submits %d: a notification without a submit", notifs, submits)
	}
	if notifs != submits {
		t.Errorf("ZC notifications %d != submits %d after settle: a SEND_ZC never completed", notifs, submits)
	}
	if delay > 0 && blocked == 0 {
		t.Fatalf("the inline-egress guard never declined with a SEND_ZC notification outstanding although the window was held open %s per notification over %d notifications: the test did not exercise the window it exists for", delay, notifs)
	}
}
