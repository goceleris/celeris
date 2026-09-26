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
// This test fixes (1) with validation.SetZCWindowHold, which holds the
// worker right after it has recorded a first completion and released
// cs.detachMu, before it releases anything else, for zcWinDelay (the NIC
// DMA latency loopback lacks), while the handler streams 64 KiB frames at a
// steady pace, faster than the client reads them, so its goroutine keeps
// calling `guarded` through every hold. (A request/echo shape does not: the
// hold also stops the worker delivering inbound frames, so an echo handler
// is parked in ReadMessage exactly while the window is open -- CI run 3 of
// that shape let the mutant survive.) With this paced stream the window
// also opens on its own -- the slow reader keeps notifications pending --
// and the natural arm (hold 0) catches the mutant too on the hosts
// measured; the hold is what makes that independent of the host's timing.
// The test asserts the window was
// entered (the guard declined the fast path with a notification
// outstanding) and that every streamed byte arrived intact and in order.
// Run under -race it is also the detector control for (2): the mandatory
// mutant deletes the detachMu acquire in handleSend's CQE_F_MORE branch,
// and this test must then FAIL with a DATA RACE report on that write
// against the guard's read. The mutant is applied by a script, never
// committed (see the PR and the ci.yml zc-window job).
//
// Arms, all read from the environment so one binary serves every run:
//
//	CELERIS587_ZC_WINDOW_HOLD  window hold (default 2ms). "0" is the
//	                           natural-window arm: the counts are logged,
//	                           the guard is not required to fire.
//	CELERIS_IOURING_SEND_ZC=off  branch control: no ZC send may be armed,
//	                           the guard witness must stay 0, and the same
//	                           stream must still be byte-intact.
//
// Needs -tags=validation (the witnesses and the hold are validation-only)
// and an io_uring engine; CELERIS_REQUIRE_IOURING_WORKERS=1 turns the
// io_uring-unavailable skip into a failure.

import (
	"encoding/binary"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/validation"
)

const (
	// zcWinFrames 64 KiB frames: 16 MiB, far under the 64 MiB detached
	// send cap, far over any loopback socket buffer once the client's
	// receive window is clamped.
	zcWinFrames = 256
	// zcWinWritePace is slept after every frame the handler writes: the
	// handler keeps calling `guarded` for the whole run instead of queueing
	// all 16 MiB in the first milliseconds.
	zcWinWritePace = 500 * time.Microsecond
	// zcWinReadPace is slept after every frame the client reads: at most a
	// quarter of the write rate even where a 500 us sleep takes a full
	// millisecond, so the backlog outgrows any autotuned socket send buffer
	// (a 1 ms reader let a linuxkit VM carry all 16 MiB inline) and ring
	// sends carry the stream, while the backlog stays under ~12 MiB, a
	// fifth of the detached cap.
	zcWinReadPace = 4 * time.Millisecond
	// zcWinAttempts bounds the re-runs of an unexposed stream.
	zcWinAttempts = 3
	// zcWinDefaultDelay is the default window hold. It is two orders of
	// magnitude above the natural (copy-fallback) window and small enough
	// that a few hundred notifications cost well under a second.
	zcWinDefaultDelay = 2 * time.Millisecond
)

func zcWinDelay(t *testing.T) time.Duration {
	t.Helper()
	v := os.Getenv("CELERIS587_ZC_WINDOW_HOLD")
	if v == "" {
		return zcWinDefaultDelay
	}
	if v == "0" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		t.Fatalf("CELERIS587_ZC_WINDOW_HOLD=%q: want a Go duration >= 0", v)
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
// from the frame the server sent as seq.
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

// zcWinAttempt is one stream's worth of witness deltas.
type zcWinAttempt struct {
	submits, detached, notifs, blocked, pendWrite, inline, ring uint64
}

func (a *zcWinAttempt) add(b zcWinAttempt) {
	a.submits += b.submits
	a.detached += b.detached
	a.notifs += b.notifs
	a.blocked += b.blocked
	a.pendWrite += b.pendWrite
	a.inline += b.inline
	a.ring += b.ring
}

// zcWinStream runs one stream on a fresh connection: "go", then zcWinFrames
// frames read at zcWinReadPace and verified in order. It returns the
// witness deltas the stream produced, the frames read and the first oracle
// failure.
func zcWinStream(t *testing.T, addr string, metrics func() celeris.EngineMetrics, hold time.Duration) (zcWinAttempt, int, error) {
	t.Helper()
	client := zcDialClamped(t, addr)
	defer client.close()
	client.upgrade(t, "/ws")
	base := metrics()
	vBase := validation.Snapshot()
	if err := client.writeClientFrame(true, OpText, []byte("go")); err != nil {
		return zcWinAttempt{}, 0, fmt.Errorf("send go: %w", err)
	}
	if err := client.bw.Flush(); err != nil {
		return zcWinAttempt{}, 0, fmt.Errorf("flush go: %w", err)
	}
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
			readErr = fmt.Errorf("read frame %d/%d: %w", got, zcWinFrames, err)
			break
		}
		if !fin || op != OpBinary {
			readErr = fmt.Errorf("frame %d: fin=%v op=%d, want a final binary frame", got, fin, op)
			break
		}
		if err := zcWinCheck(got, payload); err != nil {
			readErr = err
			break
		}
		got++
		time.Sleep(zcWinReadPace)
	}
	// Ring bytes and notifications are published on the worker's
	// per-iteration cadence; give it a pass (plus the hold) to settle.
	time.Sleep(300*time.Millisecond + 4*hold)
	m := metrics()
	v := validation.Snapshot()
	return zcWinAttempt{
		submits:   v.IouringSendZCSubmits - vBase.IouringSendZCSubmits,
		detached:  v.IouringSendZCSubmitsDetached - vBase.IouringSendZCSubmitsDetached,
		notifs:    v.IouringSendZCNotifs - vBase.IouringSendZCNotifs,
		blocked:   v.IouringInlineGuardBlockedZC - vBase.IouringInlineGuardBlockedZC,
		pendWrite: v.IouringZCCompletionWithPendingWrite - vBase.IouringZCCompletionWithPendingWrite,
		inline:    m.InlineBytes - base.InlineBytes,
		ring:      m.RingBytes - base.RingBytes,
	}, got, readErr
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
	hold := zcWinDelay(t)
	validation.SetZCWindowHold(hold)
	defer validation.SetZCWindowHold(0)

	cfg := Config{Handler: func(c *Conn) {
		if _, _, err := c.ReadMessage(); err != nil { // the client's "go"
			return
		}
		for i := 0; i < zcWinFrames; i++ {
			if err := c.WriteMessage(OpBinary, zcWinPayload(i)); err != nil {
				return
			}
			time.Sleep(zcWinWritePace)
		}
		for { // hold the conn open (and detached) until the client closes it
			if _, _, err := c.ReadMessage(); err != nil {
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

	// A stream the host's socket buffers absorbed whole never reaches the
	// ring, and one whose ZC cycles all fell between the handler's writes
	// never shows the guard declining: both are the host's timing, not the
	// code, so a stream that was not exposed is run again on a fresh
	// connection, up to zcWinAttempts. Every attempt is logged, and the race
	// detector watches all of them.
	var total zcWinAttempt
	for attempt := 1; attempt <= zcWinAttempts; attempt++ {
		a, got, readErr := zcWinStream(t, addr, metrics, hold)
		// One line per attempt, stable keys: the evidence scripts tally it.
		t.Logf("ZCWIN attempt=%d policy_off=%v delay=%s frames=%d/%d submits=%d detached=%d notifs=%d guard_blocked_zc=%d zc_completion_with_pending_write=%d inline_bytes=%d ring_bytes=%d",
			attempt, policyOff, hold, got, zcWinFrames, a.submits, a.detached, a.notifs, a.blocked, a.pendWrite, a.inline, a.ring)
		if readErr != nil {
			t.Fatalf("stream oracle, attempt %d: %v", attempt, readErr)
		}
		total.add(a)
		if a.ring > 0 && (policyOff || (a.detached > 0 && (hold == 0 || a.blocked > 0))) {
			break
		}
	}

	if total.ring == 0 {
		t.Fatalf("RingBytes did not move in %d attempts: the stream never left the inline fast path, so no ring send (and no SEND_ZC) was exercised", zcWinAttempts)
	}
	if policyOff {
		// Branch control: the same stream with the ZC arm disabled. The
		// witnesses must track SEND_ZC, not traffic.
		if total.submits != 0 || total.detached != 0 || total.notifs != 0 || total.blocked != 0 || total.pendWrite != 0 {
			t.Fatalf("CELERIS_IOURING_SEND_ZC=off: ZC witnesses moved (submits=%d detached=%d notifs=%d guard_blocked_zc=%d pending_write=%d), want all 0",
				total.submits, total.detached, total.notifs, total.blocked, total.pendWrite)
		}
		return
	}
	if total.detached == 0 {
		t.Fatalf("no SEND_ZC was armed on the detached connection in %d attempts (submits=%d): the stream did not reach the zero-copy arm", zcWinAttempts, total.submits)
	}
	if total.detached != total.submits {
		t.Errorf("detached ZC submits %d != total %d: every ZC send here is on a detached connection", total.detached, total.submits)
	}
	if total.notifs != total.submits {
		t.Errorf("ZC notifications %d != submits %d after settle: a SEND_ZC never completed, or a notification had no submit", total.notifs, total.submits)
	}
	if hold > 0 && total.blocked == 0 {
		t.Fatalf("the inline-egress guard never declined with a SEND_ZC notification outstanding in %d attempts although the window was held open %s after each of %d first completions: the test did not exercise the window it exists for", zcWinAttempts, hold, total.notifs)
	}
}
