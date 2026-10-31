//go:build linux

package iouring

import (
	"bytes"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris/internal/mono"
)

// celeris#1173, the cause #1175 left open. A SEND the loop has staged is
// submitted to the kernel at the top of the NEXT iteration, and the sweep runs
// later in the SAME one. flushSend stamps the conn's write clock (lastActivity)
// with the clock the iteration read when it started (w.cachedNow), and
// checkTimeouts counts a staged SEND as in flight: a worker that stalls for
// longer than WriteTimeout in the iteration that stages the response, between
// that read and its timeout sweep (a handler of another conn running inline in
// the batch, a GC assist, -race, a CFS quota throttling a CPU-limited
// container), closes a conn whose SEND the kernel has not even been given, and
// the peer, who could not have read a byte, is charged for the time the engine
// held it. Only io_uring has such a state: epoll writes inline.
//
// The time here is mono.Manual: it moves when the test moves it, so the stall is
// a number and no test depends on the speed of the runner. The ring is real, so
// "the kernel has the SEND" is a real Submit.

// clock1173 installs a manual clock for the test.
func clock1173(t *testing.T) *mono.Manual {
	t.Helper()
	m := mono.NewManual()
	t.Cleanup(mono.SetManual(m))
	return m
}

// cut1173 reports whether the sweep closed (or began to close) rig's conn.
func cut1173(rig *stallRig704) bool {
	return rig.cs.closing || rig.w.conns[rig.local] != rig.cs
}

// unconsumed1173 is the number of SQEs in the SQ ring the kernel has not
// consumed.
func unconsumed1173(w *Worker) int { return int(w.ring.sqPlaced() - w.ring.sqConsumed()) }

// sendSites1173 are the places a SEND SQE is armed for a conn: each stages one
// on rig's conn and leaves it in the SQ ring.
var sendSites1173 = []struct {
	name  string
	stage func(t *testing.T, rig *stallRig704)
}{
	{"flushSend", func(t *testing.T, rig *stallRig704) {
		rig.cs.writeBuf = append(rig.cs.writeBuf[:0], "a response"...)
		if rig.w.flushSend(rig.cs) {
			t.Fatal("apparatus: flushSend found no SQE")
		}
	}},
	{"flushSend_zerocopy", func(t *testing.T, rig *stallRig704) {
		rig.w.sendZC = true
		rig.cs.writeBuf = append(rig.cs.writeBuf[:0], make([]byte, 2*sendZCMinBytes)...)
		if rig.w.flushSend(rig.cs) {
			t.Fatal("apparatus: flushSend found no SQE")
		}
		if !rig.cs.sendIsZC {
			t.Fatal("apparatus: the SEND was not armed as SEND_ZC")
		}
	}},
	{"flushSendLink_linked", func(t *testing.T, rig *stallRig704) {
		rig.cs.writeBuf = append(rig.cs.writeBuf[:0], "a response"...)
		if rig.w.flushSendLink(rig.cs) {
			t.Fatal("apparatus: flushSendLink found no SQE")
		}
		if !rig.cs.recvLinked {
			t.Fatal("apparatus: the SEND was not linked to a RECV")
		}
	}},
	{"flushSendLink_one_free_slot", func(t *testing.T, rig *stallRig704) {
		// All but one slot of the SQ ring taken: the SEND goes alone.
		for unconsumed1173(rig.w) < int(rig.w.ring.params.sqEntries)-1 {
			if rig.w.ring.GetSQE() == nil {
				t.Fatal("apparatus: the SQ ring filled early")
			}
		}
		rig.cs.writeBuf = append(rig.cs.writeBuf[:0], "a response"...)
		if rig.w.flushSendLink(rig.cs) {
			t.Fatal("apparatus: flushSendLink found no SQE")
		}
		if rig.cs.recvLinked {
			t.Fatal("apparatus: the SEND was linked")
		}
	}},
	{"remainder_resend", func(t *testing.T, rig *stallRig704) {
		// The first SEND took 8 of 16 bytes; the rest is armed by handleSend.
		inFlight750(rig, "0123456789abcdef", false)
		rig.w.handleSend(sendCQE750(rig, 8, 0), rig.local, mono.Now())
		if !rig.cs.sending || string(rig.cs.sendBuf) != "89abcdef" {
			t.Fatalf("apparatus: sending=%v sendBuf=%q, want the remainder in flight", rig.cs.sending, rig.cs.sendBuf)
		}
	}},
}

// A SEND that sits in the engine's own SQ ring when the sweep comes must not be
// cut for the time it sat there, however long that was; a SEND the kernel has,
// whose peer does not take it, is cut WriteTimeout after the kernel was given
// it (the SQ ring is not a way to switch the timeout off).
func TestSendHeldInTheSQRingIsNotCutByWriteTimeout1173(t *testing.T) {
	const wt = 100 * time.Millisecond
	for _, site := range sendSites1173 {
		t.Run(site.name, func(t *testing.T) {
			clk := clock1173(t)
			rig := newStallRig704(t)
			w := rig.w
			w.cfg.WriteTimeout = wt
			w.cfg.IdleTimeout = time.Hour
			w.cfg.ReadTimeout = time.Hour

			site.stage(t, rig)
			if unconsumed1173(w) == 0 {
				t.Fatal("apparatus: the SEND is not in the SQ ring")
			}

			// The worker stalls for twice WriteTimeout between staging the SEND
			// and the sweep. The peer has been handed nothing.
			clk.Advance(2 * wt)
			w.checkTimeouts()
			if cut1173(rig) {
				t.Fatalf("celeris#1173: the sweep cut a conn whose SEND had been in our own SQ ring for %v (WriteTimeout %v): "+
					"the time the engine held the SEND was charged to the peer", 2*wt, wt)
			}

			// The kernel is given the SEND (a real submit) and the peer does not
			// read: the completion never comes. Half of WriteTimeout later the
			// conn is still not cut...
			if _, err := w.ring.Submit(); err != nil {
				t.Fatalf("apparatus: submit: %v", err)
			}
			if n := unconsumed1173(w); n != 0 {
				t.Fatalf("apparatus: %d SQEs still unconsumed after the submit", n)
			}
			clk.Advance(wt / 2)
			w.checkTimeouts()
			if cut1173(rig) {
				t.Fatalf("the conn was cut %v after the sweep that found its SEND in our SQ ring (WriteTimeout %v): "+
					"the sweep's restamp is missing (the clock ran from the SEND's staging)", wt/2, wt)
			}
			// ...and WriteTimeout after it, it is.
			clk.Advance(wt)
			w.checkTimeouts()
			if !cut1173(rig) {
				t.Fatalf("a SEND the kernel holds, to a peer that has taken nothing for %v (WriteTimeout %v), was not cut", wt+wt/2, wt)
			}
		})
	}
}

// The positive control by itself: nothing is held in our ring. A SEND the kernel
// holds, whose peer does not take it, is cut at WriteTimeout, not before.
func TestStalledPeerIsStillCutByWriteTimeout1173(t *testing.T) {
	const wt = 100 * time.Millisecond
	for _, site := range sendSites1173 {
		t.Run(site.name, func(t *testing.T) {
			clk := clock1173(t)
			rig := newStallRig704(t)
			w := rig.w
			w.cfg.WriteTimeout = wt
			w.cfg.IdleTimeout = time.Hour
			w.cfg.ReadTimeout = time.Hour

			site.stage(t, rig)
			if _, err := w.ring.Submit(); err != nil {
				t.Fatalf("apparatus: submit: %v", err)
			}
			if n := unconsumed1173(w); n != 0 {
				t.Fatalf("apparatus: %d SQEs still unconsumed after the submit", n)
			}
			clk.Advance(wt / 2)
			w.checkTimeouts()
			if cut1173(rig) {
				t.Fatalf("a SEND %v old (WriteTimeout %v) was cut", wt/2, wt)
			}
			clk.Advance(wt)
			w.checkTimeouts()
			if !cut1173(rig) {
				t.Fatalf("a SEND the peer has not taken for %v (WriteTimeout %v) was not cut", wt+wt/2, wt)
			}
		})
	}
}

// The same, end to end, on a real engine: the worker stalls for a second in the
// iteration that stages an HTTP/2 response, before that iteration's sweep. The
// stall is injected by the write-drain seam, which runs before the drain stages
// the frames; flushSend then stamps the clock the iteration read before the
// stall, and the sweep of the same iteration reads the clock after it. The
// client reads normally, so the conn must survive; with the bug the sweep closes
// it (after the response has gone out, the close being deferred behind the
// SEND), which the PING that follows finds.
func TestStalledWorkerDoesNotCutAConnWithAStagedSend1173(t *testing.T) {
	clk := clock1173(t)
	g := &gate1173{started: make(chan struct{}, 1), gate: make(chan struct{})}
	run, addr := startGate1173(t, g)
	defer func() { _, _ = run.stop() }()

	var armed atomic.Bool
	var stalls atomic.Int64
	seam := func(w *Worker, _ int) {
		if armed.CompareAndSwap(true, false) {
			stalls.Add(1)
			// The stall (before the drain stages the response, so that
			// flushSend stamps the clock read before it), and a sweep that is
			// due in this very iteration (the last one a whole ceiling ago:
			// sweepDue is given the batch's clock, so it is the worker's own
			// record that is moved); checkTimeouts then reads the clock after
			// the stall.
			clk.Advance(stallDuration1173)
			w.lastSweep = w.cachedNow - sweepCeiling
		}
	}
	h2WriteDrainSeam.Store(&seam)
	t.Cleanup(func() { h2WriteDrainSeam.Store(nil) })

	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := c.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(c, c)
	if err := fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/gate"}, {":authority", "x"}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-g.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler never ran")
	}
	armed.Store(true)
	g.gate <- struct{}{} // the handler writes its response: the worker drains it and stalls

	gotResponse, gotAck := false, false
	pinged := false
	for !gotAck {
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatalf("celeris#1173: the conn ended (%v) after a worker stall of %v in the iteration that staged the response, the peer reading normally "+
				"(response seen: %v, PING sent: %v)", err, stallDuration1173, gotResponse, pinged)
		}
		switch f := f.(type) {
		case *http2.DataFrame:
			if f.StreamEnded() && !pinged {
				gotResponse = true
				// The conn must still be alive: a PING goes round.
				if err := fr.WritePing(false, [8]byte{1, 2, 3, 4, 5, 6, 7, 8}); err != nil {
					t.Fatalf("PING: %v", err)
				}
				pinged = true
			}
		case *http2.PingFrame:
			if f.IsAck() {
				gotAck = true
			}
		}
	}
	if stalls.Load() != 1 {
		t.Fatalf("apparatus: the stall was injected %d times, want 1", stalls.Load())
	}
}

// stallDuration1173 is the injected stall: twelve times WriteTimeout.
const stallDuration1173 = 1200 * time.Millisecond
