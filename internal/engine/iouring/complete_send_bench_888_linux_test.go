//go:build linux

package iouring

import (
	"testing"
	"time"
)

// BenchmarkCompleteSend888 is completeSend's all-sent path for an ordinary
// (not detached) conn, the common one: a response that went out whole. The
// stall clock of a detached conn (celeris#888) adds a load and a branch to it (the
// store only when the clock is set);
// this is the figure to compare against an A/A twin (RULE 74).
func BenchmarkCompleteSend888(b *testing.B) {
	w := &Worker{}
	cs := &connState{fd: 7, liveIdx: -1, detected: true}
	buf := make([]byte, 128)
	now := time.Now().UnixNano()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cs.sendBuf = buf
		cs.sending = true
		if w.completeSend(cs, cs.fd, len(buf), now, false) {
			b.Fatal("closeAfter")
		}
	}
}
