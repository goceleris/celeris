package conn

import (
	"sync"
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// BenchmarkH1StreamWriteQuery884 is BenchmarkH1StreamWrite884's write arm with
// the engine's write-error query installed, in the two shapes the engines use:
// "plain", which reads the conn's flags without a lock (a conn before Detach),
// and "locked", which takes the detach lock to read them (after Detach, behind
// the guarded write hook, which takes the same lock). "twin" is the plain arm
// again, the A/A floor of the comparison inside one binary.
func BenchmarkH1StreamWriteQuery884(b *testing.B) {
	chunk := make([]byte, 1024)
	type flags struct {
		refused, closed bool
	}
	var fl flags
	var mu sync.Mutex
	plain := func() error {
		if fl.refused {
			return ErrWriteRefused
		}
		if fl.closed {
			return ErrConnClosed
		}
		return nil
	}
	locked := func() error {
		mu.Lock()
		r, c := fl.refused, fl.closed
		mu.Unlock()
		if r {
			return ErrWriteRefused
		}
		if c {
			return ErrConnClosed
		}
		return nil
	}
	for _, arm := range []struct {
		name string
		fn   func() error
	}{{"none", nil}, {"plain", plain}, {"locked", locked}, {"twin", plain}} {
		b.Run(arm.name, func(b *testing.B) {
			var buf []byte
			a := &h1ResponseAdapter{write: func(p []byte) { buf = append(buf, p...) }, keepAlive: true}
			a.writeErr = arm.fn
			var s *stream.Stream
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				if i&255 == 0 {
					buf = buf[:0]
				}
				i++
				_ = a.Write(s, chunk)
			}
		})
	}
}
