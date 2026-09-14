//go:build linux

package websocket

// End-to-end verification rig for the SEND_ZC exposure counters
// (celeris#591). The unit tests in engine/iouring/send_zc_gate_test.go pin
// the gate at the SQE, this one proves the counters move on a real ring
// with a real detached WebSocket connection — which is the only thing that
// lets the #585 fabric A/B and the #587 race tier distinguish "SEND_ZC was
// clean" from "SEND_ZC never ran".
//
// Two phases against ONE server, so the deltas come from one engine on one
// set of workers:
//
//	SMALL  strict ping-pong of zcSmallFrame (256B) payloads. The client
//	       drains every frame before asking for the next, so cs.writeBuf
//	       never reaches sendZCMinBytes and no send can be promoted to ZC.
//	       Expect ZCSendsSubmitted delta == 0.
//	BIG    a burst of zcBigFrames x zcBigFrame (64 KiB) frames written from
//	       the detached handler goroutine while the client is not reading.
//	       The client's SO_RCVBUF is clamped, so the server's socket send
//	       buffer fills within the first few frames: the inline-egress raw
//	       unix.Write goes short, the remainder is handed to the worker, and
//	       flushSend swaps a >= 4096-byte buffer into cs.sendBuf — the ZC
//	       arm. Expect ZCSendsSubmitted > 0 and ZCNotifs > 0.
//
// The phase order matters: SMALL runs first so it cannot be contaminated by
// a ZC send still in flight from the burst.
//
// Negative control: CELERIS_IOURING_SEND_ZC=off. resolveSendZCPolicy then
// clears profile.SendZC, w.sendZC is false, useSendZC never returns true and
// the BIG phase must report 0 submits and 0 notifs while shipping the same
// bytes (RingBytes still moves). The test reads the env var itself so the
// same binary serves as both arms.

import (
	"bufio"
	"encoding/binary"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/validation"
)

const (
	// zcMinBytes mirrors engine/iouring.sendZCMinBytes, which is unexported;
	// engine/iouring/send_zc_gate_test.go pins the real constant, this copy
	// only labels the two sides of the threshold in failure messages.
	zcMinBytes = 4096
	// zcBigFrame is comfortably above zcMinBytes so every ring send of a
	// whole frame takes the ZC arm.
	zcBigFrame = 64 << 10
	// zcSmallFrame is below zcMinBytes: the plain-SEND path.
	zcSmallFrame = 256
	// zcBigFrames x zcBigFrame = 4 MiB, past any loopback send buffer once
	// the receiver window is clamped by zcClientRcvBuf.
	zcBigFrames = 64
	// zcBigWaves pipelined triggers keep the handler writing while the
	// client drains, so guarded writes overlap in-flight ring sends instead
	// of all landing before the first SQE is armed.
	zcBigWaves = 8
	// zcSmallRounds keeps the small phase long enough that a per-send
	// mis-attribution would show up, while each round is fully drained.
	zcSmallRounds = 200
	// zcClientRcvBuf clamps the client's receive window so the server's
	// send buffer fills (and the inline write goes short) deterministically
	// rather than depending on the host's wmem autotuning.
	zcClientRcvBuf = 8 << 10
)

// zcDialClamped dials addr with a small SO_RCVBUF so the peer's send buffer
// fills quickly, and returns the same raw client shape the other tests use.
func zcDialClamped(tb testing.TB, addr string) *testWSClient {
	tb.Helper()
	d := net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(_, _ string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET,
					syscall.SO_RCVBUF, zcClientRcvBuf)
			}); err != nil {
				return err
			}
			return serr
		},
	}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		tb.Fatal(err)
	}
	return &testWSClient{
		conn: conn,
		br:   bufio.NewReaderSize(conn, 4096),
		bw:   bufio.NewWriter(conn),
	}
}

// zcPolicyOff reports whether the SEND_ZC policy knob is forced off for
// this process — the negative-control arm.
func zcPolicyOff() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CELERIS_IOURING_SEND_ZC"))) {
	case "0", "off", "false":
		return true
	}
	return false
}

// zcHasIOUring reports whether the io_uring engine is usable here.
func zcHasIOUring(t *testing.T) bool {
	t.Helper()
	for _, k := range engineKinds(t) {
		if k == celeris.IOUring {
			return true
		}
	}
	return false
}

// TestSendZCCountersOnDetachedWrite is the celeris#591 measurement: a
// detached WebSocket write at or above sendZCMinBytes must move
// ZCSendsSubmitted and ZCNotifs, and a 256-byte write must not.
func TestSendZCCountersOnDetachedWrite(t *testing.T) {
	if !zcHasIOUring(t) {
		t.Skip("io_uring not usable at High+ tier on this kernel")
	}
	policyOff := zcPolicyOff()

	big := make([]byte, zcBigFrame)
	for i := range big {
		big[i] = byte(i)
	}
	small := make([]byte, zcSmallFrame)
	for i := range small {
		small[i] = byte(i)
	}

	cfg := Config{Handler: func(c *Conn) {
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			switch string(msg) {
			case "small":
				if err := c.WriteMessage(OpBinary, small); err != nil {
					return
				}
			case "big":
				for i := 0; i < zcBigFrames; i++ {
					payload := make([]byte, zcBigFrame)
					copy(payload, big)
					binary.BigEndian.PutUint64(payload[:8], uint64(i))
					if err := c.WriteMessage(OpBinary, payload); err != nil {
						return
					}
				}
			default:
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

	// --- SMALL phase: strictly drained 256-byte writes ----------------------
	base := metrics()
	vBase := validation.Snapshot()
	for i := 0; i < zcSmallRounds; i++ {
		if err := client.writeClientFrame(true, OpText, []byte("small")); err != nil {
			t.Fatalf("small %d: write: %v", i, err)
		}
		if err := client.conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatalf("small %d: deadline: %v", i, err)
		}
		_, op, payload, err := readServerFrameNonFatal(client.br)
		if err != nil {
			t.Fatalf("small %d: read: %v", i, err)
		}
		if op != OpBinary || len(payload) != zcSmallFrame {
			t.Fatalf("small %d: op=%d len=%d, want OpBinary/%d", i, op, len(payload), zcSmallFrame)
		}
	}
	afterSmall := metrics()
	vAfterSmall := validation.Snapshot()
	smallSubmits := afterSmall.ZCSendsSubmitted - base.ZCSendsSubmitted
	smallNotifs := afterSmall.ZCNotifs - base.ZCNotifs
	t.Logf("SMALL  (%d x %dB, drained): ZCSendsSubmitted+=%d ZCNotifs+=%d InlineBytes+=%d RingBytes+=%d",
		zcSmallRounds, zcSmallFrame, smallSubmits, smallNotifs,
		afterSmall.InlineBytes-base.InlineBytes, afterSmall.RingBytes-base.RingBytes)
	t.Logf("SMALL  validation (build=%v): submits+=%d detached+=%d notifs+=%d guard_blocked+=%d notif_pending_write+=%d",
		wsZCValidationBuild,
		vAfterSmall.IouringSendZCSubmits-vBase.IouringSendZCSubmits,
		vAfterSmall.IouringSendZCSubmitsDetached-vBase.IouringSendZCSubmitsDetached,
		vAfterSmall.IouringSendZCNotifs-vBase.IouringSendZCNotifs,
		vAfterSmall.IouringInlineGuardBlockedZC-vBase.IouringInlineGuardBlockedZC,
		vAfterSmall.IouringZCCompletionWithPendingWrite-vBase.IouringZCCompletionWithPendingWrite)
	if smallSubmits != 0 {
		t.Errorf("SMALL: ZCSendsSubmitted moved by %d — a sub-%dB send took the ZC arm",
			smallSubmits, zcMinBytes)
	}
	if smallNotifs != 0 {
		t.Errorf("SMALL: ZCNotifs moved by %d with no submit", smallNotifs)
	}
	if n := vAfterSmall.IouringSendZCSubmits - vBase.IouringSendZCSubmits; n != 0 {
		t.Errorf("SMALL: validation IouringSendZCSubmits moved by %d, want 0", n)
	}

	// --- BIG phase: zcBigWaves waves of 64 KiB frames -----------------------
	// The triggers are pipelined so the handler starts the next wave while
	// the client is still draining the previous one: after the first wave the
	// connection is permanently congested, every guarded write lands with a
	// ring send in flight, and flushSend keeps swapping >= 4096-byte buffers
	// into cs.sendBuf. The first wave is left undrained on purpose so the
	// socket send buffer fills before any reading starts.
	for w := 0; w < zcBigWaves; w++ {
		if err := client.writeClientFrame(true, OpText, []byte("big")); err != nil {
			t.Fatalf("big: trigger %d write: %v", w, err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	var got int
	for got < zcBigFrames*zcBigWaves {
		if err := client.conn.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
			t.Fatalf("big %d: deadline: %v", got, err)
		}
		fin, op, payload, err := readServerFrameNonFatal(client.br)
		if err != nil {
			t.Fatalf("big: read frame %d/%d: %v", got, zcBigFrames*zcBigWaves, err)
		}
		if !fin || op != OpBinary || len(payload) != zcBigFrame {
			t.Fatalf("big frame %d: fin=%v op=%d len=%d", got, fin, op, len(payload))
		}
		if seq := binary.BigEndian.Uint64(payload[:8]); seq != uint64(got%zcBigFrames) {
			t.Fatalf("big frame %d: seq=%d — frames reordered or corrupted", got, seq)
		}
		for j := 8; j < len(payload); j++ {
			if payload[j] != byte(j) {
				t.Fatalf("big frame %d: corrupt at byte %d", got, j)
			}
		}
		got++
	}
	// The ring-byte counter is published on the worker's per-iteration
	// cadence (see Worker.ringBytesBatch), so give the loop a pass after the
	// last completion before reading it.
	time.Sleep(300 * time.Millisecond)
	afterBig := metrics()
	vAfterBig := validation.Snapshot()
	bigSubmits := afterBig.ZCSendsSubmitted - afterSmall.ZCSendsSubmitted
	bigNotifs := afterBig.ZCNotifs - afterSmall.ZCNotifs
	bigInline := afterBig.InlineBytes - afterSmall.InlineBytes
	bigRing := afterBig.RingBytes - afterSmall.RingBytes
	vSubmits := vAfterBig.IouringSendZCSubmits - vAfterSmall.IouringSendZCSubmits
	vDetached := vAfterBig.IouringSendZCSubmitsDetached - vAfterSmall.IouringSendZCSubmitsDetached
	vNotifs := vAfterBig.IouringSendZCNotifs - vAfterSmall.IouringSendZCNotifs
	vBlocked := vAfterBig.IouringInlineGuardBlockedZC - vAfterSmall.IouringInlineGuardBlockedZC
	vPendWrite := vAfterBig.IouringZCCompletionWithPendingWrite - vAfterSmall.IouringZCCompletionWithPendingWrite
	t.Logf("BIG    (%d x %dB, undrained): ZCSendsSubmitted+=%d ZCNotifs+=%d InlineBytes+=%d RingBytes+=%d (policy_off=%v)",
		zcBigFrames, zcBigFrame, bigSubmits, bigNotifs, bigInline, bigRing, policyOff)
	t.Logf("BIG    validation (build=%v): submits+=%d detached+=%d notifs+=%d guard_blocked+=%d notif_pending_write+=%d",
		wsZCValidationBuild, vSubmits, vDetached, vNotifs, vBlocked, vPendWrite)

	if wsZCValidationBuild {
		// Under -tags=validation the counter package holds live atomics, so
		// the validation witnesses must agree with the always-on metrics.
		// Every ZC send here is on a detached (upgraded WebSocket) conn, so
		// the detached split must equal the total.
		if vSubmits != bigSubmits {
			t.Errorf("validation IouringSendZCSubmits+=%d but EngineMetrics ZCSendsSubmitted+=%d",
				vSubmits, bigSubmits)
		}
		if vNotifs != bigNotifs {
			t.Errorf("validation IouringSendZCNotifs+=%d but EngineMetrics ZCNotifs+=%d",
				vNotifs, bigNotifs)
		}
		if vDetached != vSubmits {
			t.Errorf("validation IouringSendZCSubmitsDetached+=%d, want %d (every ZC send here is on a detached conn)",
				vDetached, vSubmits)
		}
	} else if vSubmits != 0 || vNotifs != 0 || vDetached != 0 || vBlocked != 0 || vPendWrite != 0 {
		t.Errorf("production build: validation deltas must all be 0, got submits=%d detached=%d notifs=%d blocked=%d pending_write=%d",
			vSubmits, vDetached, vNotifs, vBlocked, vPendWrite)
	}

	if bigRing == 0 {
		t.Fatalf("BIG: RingBytes did not move — the burst never reached the ring, so the ZC arm could not be tested")
	}
	if policyOff {
		// Negative control: same burst, same ring sends, ZC policy off.
		if bigSubmits != 0 || bigNotifs != 0 {
			t.Errorf("CELERIS_IOURING_SEND_ZC=off: ZCSendsSubmitted+=%d ZCNotifs+=%d, want 0/0",
				bigSubmits, bigNotifs)
		}
		if vSubmits != 0 || vNotifs != 0 || vDetached != 0 {
			t.Errorf("CELERIS_IOURING_SEND_ZC=off: validation submits+=%d detached+=%d notifs+=%d, want 0/0/0",
				vSubmits, vDetached, vNotifs)
		}
		return
	}
	if bigSubmits == 0 {
		t.Errorf("BIG: ZCSendsSubmitted did not move over %d x %dB of detached egress (RingBytes+=%d)",
			zcBigFrames, zcBigFrame, bigRing)
	}
	if bigNotifs == 0 {
		t.Errorf("BIG: ZCNotifs did not move though %d ZC sends were submitted", bigSubmits)
	}
	if bigNotifs > bigSubmits {
		t.Errorf("BIG: ZCNotifs(%d) > ZCSendsSubmitted(%d) — a notification without a submit",
			bigNotifs, bigSubmits)
	}
}
