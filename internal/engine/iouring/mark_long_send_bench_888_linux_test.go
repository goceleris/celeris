//go:build linux

package iouring

import (
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/conn"
)

// BenchmarkMarkLongSend888 is markLongSend for a small SEND, on every send's
// path (flushSend): celeris#888 marks the SENDs of a truly-detached conn
// whatever their size (watchesStall), and the ordinary conn pays the compares
// that tell it apart: a nil detachMu for an inline conn, the WriteTimeout and
// the h1State's Detached flag for an async-mode conn. "detached" is the marked
// case, which also counts the conn (markRemainder; reset every op, as the SEND's
// completion does). The figure to compare against an A/A twin (RULE 74).
func BenchmarkMarkLongSend888(b *testing.B) {
	for _, mode := range []string{"inline", "async", "detached"} {
		b.Run(mode, func(b *testing.B) {
			w := &Worker{}
			w.cfg.WriteTimeout = 60 * time.Second
			cs := &connState{fd: 7, liveIdx: -1, detected: true, h1State: conn.NewH1State()}
			if mode != "inline" {
				cs.detachMu = &sync.Mutex{}
			}
			cs.h1State.Detached.Store(mode == "detached")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				w.markLongSend(cs, 512)
				cs.remainderOut = false
				w.remainderN = 0
			}
		})
	}
}
