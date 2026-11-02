//go:build linux

package iouring

import (
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
)

// celeris#1208: a worker with nothing to send, on its first look at the shutdown,
// asks the kernel for a request it has not read before it exits
// (sendDrainDone, h1UnreadInKernel), inside the 250 ms floor only. These tests pin
// the parts that the end-to-end cells (test/server, B-hold-other and
// B-accept-hold) cannot: the completion-queue half of the look, which on a kernel
// whose completions are task work (DEFER_TASKRUN, COOP_TASKRUN) is never the one
// that finds the request (its bytes are still in the socket), and only a base-tier
// ring, or any ring that posts at once, reaches; the count of a conn the gate has
// marked; and the floor. They arm the recv themselves, so they do not depend on
// the ring's tier, and the base-tier CI job (which runs this whole package) runs
// them as well.

const req1208 = "GET /x HTTP/1.1\r\nHost: x\r\n\r\n"

func inq1208(t *testing.T, fd int) int {
	t.Helper()
	q, err := unix.IoctlGetInt(fd, unix.TIOCINQ)
	if err != nil {
		t.Fatalf("TIOCINQ: %v", err)
	}
	return q
}

func TestGateH1UnreadInKernel1208(t *testing.T) {
	t.Run("an idle conn holds nothing", func(t *testing.T) {
		w, _, _ := gateRig884(t, 2)
		if w.h1UnreadInKernel() {
			t.Errorf("h1UnreadInKernel: true for conns with nothing in the kernel")
		}
	})
	t.Run("bytes in the socket queue", func(t *testing.T) {
		w, _, peers := gateRig884(t, 2)
		if _, err := unix.Write(peers[1], []byte(req1208)); err != nil {
			t.Fatal(err)
		}
		if !w.h1UnreadInKernel() {
			t.Errorf("h1UnreadInKernel: false with a request in a socket's queue")
		}
	})
	t.Run("a recv CQE with bytes, unreaped, the socket queue empty (the CQ half)", func(t *testing.T) {
		w, css, peers := gateRig884(t, 2)
		armPostedRecv1104(t, w, css[0], peers[0], req1208)
		if q := inq1208(t, css[0].fd); q != 0 {
			t.Fatalf("premise: %d bytes still in the socket's queue after the recv completed", q)
		}
		if !w.h1UnreadInKernel() {
			t.Errorf("h1UnreadInKernel: false with the request a posted recv CQE (the base tier's shape)")
		}
		if css[0].gateRead {
			t.Errorf("the look marked the conn (gateRead): it must not, the gate's pass marks")
		}
	})
	t.Run("a recv CQE that is an EOF", func(t *testing.T) {
		w, css, peers := gateRig884(t, 1)
		armPostedRecv1104(t, w, css[0], peers[0], "")
		if w.h1UnreadInKernel() {
			t.Errorf("h1UnreadInKernel: true for a posted recv CQE with no bytes (a FIN is not a request)")
		}
	})
	t.Run("a recv CQE of another generation", func(t *testing.T) {
		w, css, peers := gateRig884(t, 1)
		armPostedRecv1104(t, w, css[0], peers[0], req1208)
		css[0].generation++
		if w.h1UnreadInKernel() {
			t.Errorf("h1UnreadInKernel: true on the strength of a CQE of another generation")
		}
	})
	t.Run("a recv CQE of an HTTP/2 conn", func(t *testing.T) {
		w, css, peers := gateRig884(t, 1)
		armPostedRecv1104(t, w, css[0], peers[0], req1208)
		css[0].protocol.Store(int32(engine.H2C))
		if w.h1UnreadInKernel() {
			t.Errorf("h1UnreadInKernel: true for an HTTP/2 conn's recv CQE")
		}
	})
	for _, tc := range []struct {
		name string
		mark func(cs *connState)
	}{
		{"a closing conn", func(cs *connState) { cs.closing = true }},
		{"an ended detached stream", func(cs *connState) { cs.writeEnded.Store(true) }},
		{"an HTTP/2 conn", func(cs *connState) { cs.protocol.Store(int32(engine.H2C)) }},
		{"a running detached stream", func(cs *connState) { cs.h1State.Detached.Store(true) }},
	} {
		t.Run(tc.name+" is not asked", func(t *testing.T) {
			w, css, peers := gateRig884(t, 1)
			if _, err := unix.Write(peers[0], []byte(req1208)); err != nil {
				t.Fatal(err)
			}
			tc.mark(css[0])
			if w.h1UnreadInKernel() {
				t.Errorf("h1UnreadInKernel: true for %s with bytes in its socket", tc.name)
			}
		})
	}
}

// The gate's first pass marks a conn it finds with a request in the kernel
// (gateRead): the worker must wait for that request to be read and answered, not
// exit between the mark and the read.
func TestGateH1RequestInProgressCountsAMarkedConn1208(t *testing.T) {
	w, css, peers := gateRig884(t, 1)
	if w.h1RequestInProgress() {
		t.Fatal("premise: an idle conn is a request in progress")
	}
	if _, err := unix.Write(peers[0], []byte(req1208)); err != nil {
		t.Fatal(err)
	}
	w.refuseRecv = true
	w.closeIdleH1() // the first pass marks the conn
	if !css[0].gateRead {
		t.Fatal("premise: the first pass did not mark the conn")
	}
	if !w.h1RequestInProgress() {
		t.Errorf("h1RequestInProgress: false for a conn the gate marked to be read on: the worker would exit with its request unanswered")
	}
	css[0].writeEnded.Store(true)
	if w.h1RequestInProgress() {
		t.Errorf("h1RequestInProgress: true for a marked conn whose detached stream was ended")
	}
	css[0].writeEnded.Store(false)
	css[0].closing = true
	if w.h1RequestInProgress() {
		t.Errorf("h1RequestInProgress: true for a marked conn that is closing")
	}
}

// The look is inside the floor only: with a request the worker cannot read in
// the kernel (it has stopped reading past the floor), asking on would hold the
// worker for the whole budget.
func TestGateSendDrainDoneAsksTheKernelInsideTheFloorOnly1208(t *testing.T) {
	w, _, peers := gateRig884(t, 1)
	const start = int64(5_000_000_000)
	w.shutdownDrainStart = start
	if !w.sendDrainDone(start + 1) {
		t.Fatal("control: a worker with nothing to send and nothing in the kernel is not done")
	}
	if _, err := unix.Write(peers[0], []byte(req1208)); err != nil {
		t.Fatal(err)
	}
	if w.sendDrainDone(start + 1) {
		t.Errorf("sendDrainDone: true at the start of the drain with a request in the kernel")
	}
	if w.sendDrainDone(start + shutdownSendDrainNanos) {
		t.Errorf("sendDrainDone: true at the floor itself (not past it) with a request in the kernel")
	}
	if !w.sendDrainDone(start + shutdownSendDrainNanos + 1) {
		t.Errorf("sendDrainDone: false past the floor: the look would keep the worker beyond it")
	}
}
