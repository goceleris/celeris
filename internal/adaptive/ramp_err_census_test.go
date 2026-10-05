//go:build linux

package adaptive

import (
	"fmt"
	"testing"
)

// TestRampErrCensusCountsPastTheExemplarCap pins the ramp census the
// celeris#657 gates read (W3). recordErr used to guard the whole tally on the
// size of a map whose keys embed the client's ephemeral port, so it stopped
// counting after ~200 connections — repeats of a key already held included —
// and a "0 of class X" was not concludable. Here every error is a distinct
// key, as it is under load, and there are far more of them than the exemplar
// cap: the total and the per-class counts must still be exact, a repeat of a
// retained exemplar must still count, and only the exemplar set may stop
// growing.
func TestRampErrCensusCountsPastTheExemplarCap(t *testing.T) {
	errSampleMu.Lock()
	resetErrCensus662()
	errSampleMu.Unlock()
	t.Cleanup(func() {
		errSampleMu.Lock()
		resetErrCensus662()
		errSampleMu.Unlock()
	})

	const timeouts, resets = 3 * errExemplarCap, errExemplarCap
	for i := range timeouts {
		recordErr(fmt.Sprintf("read: read tcp 127.0.0.1:%d->127.0.0.1:8080: i/o timeout", 10000+i))
	}
	for i := range resets {
		recordErr(fmt.Sprintf("read: read tcp 127.0.0.1:%d->127.0.0.1:8080: read: connection reset by peer", 20000+i))
	}
	// The first key recorded is one of the retained exemplars; its repeat
	// must keep counting after the cap is reached.
	first := "read: read tcp 127.0.0.1:10000->127.0.0.1:8080: i/o timeout"
	recordErr(first)

	errSampleMu.Lock()
	defer errSampleMu.Unlock()
	if want := timeouts + resets + 1; errTotalCount != want {
		t.Errorf("errTotalCount = %d, want %d — the census stopped counting", errTotalCount, want)
	}
	if got := errClassCount["read: timeout"]; got != timeouts+1 {
		t.Errorf(`errClassCount["read: timeout"] = %d, want %d`, got, timeouts+1)
	}
	if got := errClassCount["read: reset"]; got != resets {
		t.Errorf(`errClassCount["read: reset"] = %d, want %d`, got, resets)
	}
	if got := len(errSamples); got != errExemplarCap {
		t.Errorf("len(errSamples) = %d, want the cap %d — only the exemplar set is bounded", got, errExemplarCap)
	}
	if got := errSamples[first]; got != 2 {
		t.Errorf("errSamples[first] = %d, want 2 — a retained exemplar stopped counting at the cap", got)
	}
}
