package conn

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// The engines' write-error queries in the shapes they come in, each over the
// flags a conn has; the real ones are in internal/engine/{epoll,iouring}
// (connState.StreamWriteErr, detachedStreamErr).
type (
	// benchPlainQuery reads plain flags without a lock and compares the
	// pending size with a limit (a conn before Detach).
	benchPlainQuery struct {
		refused, closed bool
		pending         int
	}
	// benchLockedQuery takes a mutex to read them (the first version of the
	// detached query, which took detachMu on every call).
	benchLockedQuery struct {
		mu              sync.Mutex
		refused, closed bool
	}
	// benchAtomicQuery reads two atomics and, only once one is set, the flags
	// under the mutex (the detached query now).
	benchAtomicQuery struct {
		ended, async atomic.Bool
		mu           sync.Mutex
		refused      bool
	}
)

func (q *benchPlainQuery) StreamWriteErr(n int) error {
	if q.refused {
		return ErrWriteRefused
	}
	if q.closed {
		return ErrConnClosed
	}
	if q.pending+n > 64<<20 {
		return ErrWriteRefused
	}
	return nil
}

func (q *benchLockedQuery) StreamWriteErr(int) error {
	q.mu.Lock()
	r, c := q.refused, q.closed
	q.mu.Unlock()
	if r {
		return ErrWriteRefused
	}
	if c {
		return ErrConnClosed
	}
	return nil
}

func (q *benchAtomicQuery) StreamWriteErr(int) error {
	if !q.ended.Load() && !q.async.Load() {
		return nil
	}
	q.mu.Lock()
	r := q.refused
	q.mu.Unlock()
	if r {
		return ErrWriteRefused
	}
	return ErrConnClosed
}

// BenchmarkH1StreamWriteQuery884 is BenchmarkH1StreamWrite884's write arm and
// its Write-then-Flush arm (an SSE event) with the engine's write-error query
// installed, in the shapes the engines use: "plain" (a conn before Detach, no
// lock), "locked" (a detached conn's first query, which took the detach lock
// on every call, kept as the reference), and "atomic" (a detached conn's query
// now: two atomic loads). "twin" is the plain arm again, the A/A floor of the
// comparison inside one binary. The Write calls the query twice (the size
// before the chunk is staged, then the state after the hook) and Flush once.
func BenchmarkH1StreamWriteQuery884(b *testing.B) {
	chunk := make([]byte, 1024)
	for _, arm := range []struct {
		name string
		q    WriteErrQuery
	}{
		{"none", nil},
		{"plain", &benchPlainQuery{}},
		{"locked", &benchLockedQuery{}},
		{"atomic", &benchAtomicQuery{}},
		{"twin", &benchPlainQuery{}},
	} {
		for _, flush := range []bool{false, true} {
			name := arm.name + "/write"
			if flush {
				name += "+flush"
			}
			b.Run(name, func(b *testing.B) {
				var buf []byte
				a := &h1ResponseAdapter{write: func(p []byte) { buf = append(buf, p...) }, keepAlive: true}
				if arm.q != nil {
					a.writeErr = arm.q
				}
				var s *stream.Stream
				b.ReportAllocs()
				i := 0
				for b.Loop() {
					if i&255 == 0 {
						buf = buf[:0]
					}
					i++
					_ = a.Write(s, chunk)
					if flush {
						_ = a.Flush(s)
					}
				}
			})
		}
	}
}
