//go:build linux

package iouring

import (
	"testing"
	"time"
)

// TestDetachedStallClockEndsWithTheQueue888: a conn whose queue drained ends
// its clock, in completeSend (all sent) and in the sweep that finds nothing
// queued, so the next queue is not charged for the time before it.
func TestDetachedStallClockEndsWithTheQueue888(t *testing.T) {
	s := newStall888(t, stall888Bound)
	s.sweepNow()
	if s.cs.stallSince == 0 {
		t.Fatalf("no stall clock after a sweep that found bytes queued")
	}

	// The last SEND completes whole.
	s.cs.detachMu.Lock()
	n := len(s.cs.sendBuf)
	s.w.completeSend(s.cs, s.cs.fd, n, time.Now().UnixNano(), false)
	s.cs.detachMu.Unlock()
	if s.cs.stallSince != 0 {
		t.Errorf("stallSince=%d after the queue drained in completeSend, want 0", s.cs.stallSince)
	}

	// A queue the sweep finds gone, with the clock left running: the stream's
	// goroutine drained it inline.
	s.cs.sendBuf, s.cs.sending = s.cs.sendBuf[:1], true
	s.sweepNow()
	if s.cs.stallSince == 0 {
		t.Fatalf("no stall clock after a sweep that found bytes queued")
	}
	s.cs.sendBuf, s.cs.sending = s.cs.sendBuf[:0], false
	s.sweepNow()
	if s.cs.stallSince != 0 {
		t.Errorf("stallSince=%d after a sweep that found nothing queued, want 0", s.cs.stallSince)
	}
	if s.cs.closing {
		t.Errorf("a drained conn was closed")
	}
}
