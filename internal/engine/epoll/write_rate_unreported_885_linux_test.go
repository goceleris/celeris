//go:build linux

package epoll

import (
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/resource"
)

// celeris#885 item 51, review r3 finding F4: a TCP stack that answers
// getsockopt(TCP_INFO) but does not fill tcpi_bytes_acked (the report is a
// user-space stack such as gVisor's; Linux before 4.1 returns a short struct)
// reads 0 for every conn. The rule must not take that for a peer that
// acknowledges nothing: a parked response is then judged by the flush clock as
// it was before the rule, and the rule is back as soon as the socket reports a
// count. A parked conn has had bytes sent, so a real stack's count is not 0
// when the peer has taken any.
//
// The tests use the Loop's ackedProbe seam as the stack that reports 0.

func zeroAcked885(int) (uint64, error) { return 0, nil }

// TestParkedConnOfAStackThatReportsNoAckedBytesIsJudgedByTheFlushClock885: the
// peer takes 64 KiB every 100 ms and an EPOLLOUT edge (handleWritable) follows
// each read, so the flush clock moves at every one; the count the stack reports
// is 0. WriteTimeout 600 ms: the conn is held for 5 windows, as the flush clock
// holds it (and as the conn of a stack that reports the count and a peer above
// the floor is). Then the peer stops and the flush clock closes it a
// WriteTimeout later.
func TestParkedConnOfAStackThatReportsNoAckedBytesIsJudgedByTheFlushClock885(t *testing.T) {
	const wt = 600 * time.Millisecond
	r := parked885(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: wt})
	r.l.ackedProbe = zeroAcked885
	start := time.Now()
	for time.Since(start) < 5*wt {
		time.Sleep(100 * time.Millisecond)
		if r.read(64<<10) == 0 {
			t.Fatalf("apparatus: the peer read nothing")
		}
		r.l.handleWritable(r.cs)
		r.l.checkTimeouts()
		if !r.open() {
			t.Fatalf("celeris#885 item 51: a stack that reports bytes_acked = 0 turned the rule into a hard cutoff: the conn was reaped %v in, "+
				"the peer taking 64 KiB every 100 ms with an edge at each (the flush clock holds that)", time.Since(start).Round(time.Millisecond))
		}
	}
	stopped := time.Now()
	for time.Since(stopped) < 3*wt {
		time.Sleep(25 * time.Millisecond)
		r.l.checkTimeouts()
		if !r.open() {
			if gone := time.Since(stopped); gone < wt-100*time.Millisecond || gone > wt+300*time.Millisecond {
				t.Errorf("a peer that stopped was reaped %v after its last byte, want about one WriteTimeout (%v): the flush clock's", gone.Round(time.Millisecond), wt)
			}
			return
		}
	}
	t.Fatalf("a peer that stopped reading was not reaped by the flush clock %v after its last byte", 3*wt)
}

// TestParkedConnIsRateGovernedOnceTheStackReportsAckedBytes885: the stack
// reports 0 for the first 300 ms of the response and the real count after (a
// peer that had acknowledged nothing at the first sweeps: a high round trip), and
// the peer reads 4 times the floor with no edge: the rule takes the conn from
// there and it is held 4 windows past the point where the flush clock would
// have reaped it (ReadTimeout and IdleTimeout off); then the peer stops and the
// rule closes it within two windows.
func TestParkedConnIsRateGovernedOnceTheStackReportsAckedBytes885(t *testing.T) {
	const wt = 600 * time.Millisecond
	r := parked885(t, resource.Config{ReadTimeout: -1, IdleTimeout: -1, WriteTimeout: wt})
	start := time.Now()
	r.l.ackedProbe = func(fd int) (uint64, error) {
		if time.Since(start) < 300*time.Millisecond {
			return 0, nil
		}
		r.l.ackedProbe = nil // the real read from here (the loop thread is this one)
		return r.l.bytesAcked(fd)
	}
	rate := 4 * floorRate885(wt)
	var took float64
	for time.Since(start) < 4*wt {
		time.Sleep(20 * time.Millisecond)
		el := time.Since(start)
		for due := rate*el.Seconds() - took; due >= 4096; {
			n := r.read(min(int(due), 16<<10))
			if n == 0 {
				break
			}
			took += float64(n)
			due -= float64(n)
		}
		_ = r.l.flushWrites(r.cs, true)
		r.l.checkTimeouts()
		if !r.open() {
			t.Fatalf("reaped %v in, the peer taking 4x the floor with no edge once the stack reports the count (from 300 ms)", el.Round(time.Millisecond))
		}
	}
	stopped := time.Now()
	for time.Since(stopped) < 4*wt {
		time.Sleep(25 * time.Millisecond)
		r.l.checkTimeouts()
		if !r.open() {
			if gone := time.Since(stopped); gone > 2*wt+250*time.Millisecond {
				t.Errorf("a peer that stopped was reaped %v after its last byte, want within two windows (%v)", gone.Round(time.Millisecond), 2*wt)
			}
			return
		}
	}
	t.Fatalf("a peer that stopped reading was not reaped %v after its last byte", 4*wt)
}

// TestParkedConnOfAStackThatReportsNoAckedBytesAndATakesNothingIsReaped885
// (guard, holds before the fix too): nothing taken, a count of 0, WriteTimeout
// 600 ms: reaped between one and two windows in, by the flush clock.
func TestParkedConnOfAStackThatReportsNoAckedBytesAndATakesNothingIsReaped885(t *testing.T) {
	const wt = 600 * time.Millisecond
	r := parked885(t, resource.Config{ReadTimeout: -1, IdleTimeout: -1, WriteTimeout: wt})
	r.l.ackedProbe = zeroAcked885
	start := time.Now()
	for time.Since(start) < 4*wt {
		time.Sleep(25 * time.Millisecond)
		r.l.checkTimeouts()
		if !r.open() {
			if gone := time.Since(start); gone < wt || gone > 2*wt {
				t.Errorf("reaped %v in, want between one and two windows (%v)", gone.Round(time.Millisecond), wt)
			}
			return
		}
	}
	t.Fatalf("a peer that takes nothing was not reaped %v in", 4*wt)
}
