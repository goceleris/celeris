//go:build linux

package iouring

import "testing"

// TestShutdownDriversRetainsConnsWithOpsInFlight pins celeris#545.
//
// shutdownDrivers drops w.driverConns, which is the only thing keeping the
// driverConns — and their dc.buf backing arrays — reachable. It issues no
// ASYNC_CANCEL and does not wait for inflightOps to reach zero, unlike
// UnregisterConn, so the kernel may still own a RECV writing into dc.buf when
// those references go away. That is the celeris#256 class: GC repurposes the
// memory and the kernel writes into it. What actually cancels the ops is
// closing the ring, which happens later in shutdown(), so the references have
// to survive until then.
func TestShutdownDriversRetainsConnsWithOpsInFlight(t *testing.T) {
	w := &Worker{driverConns: make(map[int]*driverConn)}

	inflight := &driverConn{fd: 41, w: w, buf: make([]byte, 4096), recvArmed: true, inflightOps: 1}
	idle := &driverConn{fd: 42, w: w, buf: make([]byte, 4096)}
	var closed int
	for _, dc := range []*driverConn{inflight, idle} {
		dc.onClose = func(error) { closed++ }
		w.driverConns[dc.fd] = dc
	}
	w.hasDriverConns.Store(true)

	w.shutdownDrivers()

	if closed != 2 {
		t.Errorf("onClose fired %d times, want 2 — shutdown must still report to every driver", closed)
	}
	if w.driverConns != nil {
		t.Error("driverConns should be cleared")
	}

	held := make(map[*driverConn]bool, len(w.shutdownDriverHold))
	for _, dc := range w.shutdownDriverHold {
		held[dc] = true
	}
	if !held[inflight] {
		t.Error("driverConn with a RECV in flight was not retained: dc.buf can be " +
			"collected while the kernel is still writing into it")
	}
	// The idle one is retained too. Distinguishing them would mean trusting
	// inflightOps at a moment when no CQE will ever update it again, and the
	// cost of holding a handful of buffers until the Worker dies is nil.
	if !held[idle] {
		t.Error("idle driverConn was not retained")
	}
}
