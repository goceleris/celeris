//go:build linux

package iouring

import (
	"testing"

	"golang.org/x/sys/unix"
)

// TestSendZCEOPNOTSUPPMarksOnlyThatConn988: both forms of the failure (with and
// without a notification) set noSendZC on the conn whose socket rejected
// SEND_ZC, and on no other conn.
func TestSendZCEOPNOTSUPPMarksOnlyThatConn988(t *testing.T) {
	for name, complete := range map[string]func(*zcConn, *Worker, unix.Errno){
		"Notified":   (*zcConn).completeZCNotified,
		"Unnotified": (*zcConn).completeZCUnnotified,
	} {
		t.Run(name, func(t *testing.T) {
			w, conns := newZCFallbackWorker(t, 2)
			z, sib := conns[0], conns[1]
			z.armZCSend(t, w)
			complete(z, w, unix.EOPNOTSUPP)
			if !z.cs.noSendZC {
				t.Error("the conn does not remember that its socket rejects SEND_ZC")
			}
			if sib.cs.noSendZC {
				t.Error("an unrelated conn was marked noSendZC")
			}
		})
	}
}

// TestReleaseConnStateResetsNoSendZC988: noSendZC describes one socket, so a
// pooled connState must not carry it to the next conn that acquires it.
func TestReleaseConnStateResetsNoSendZC988(t *testing.T) {
	cs := &connState{noSendZC: true}
	releaseConnState(cs)
	if cs.noSendZC {
		t.Error("releaseConnState left noSendZC set; the next conn out of connStatePool never uses SEND_ZC")
	}
}
