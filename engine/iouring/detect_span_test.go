//go:build linux

package iouring

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/frame"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// TestDetectSpanningH2Preface is the regression guard for v1.5.0 review 2.8:
// when the 24-byte HTTP/2 client preface is delivered one byte per recv, the
// engine must accumulate the bytes across recvs and still detect H2C. Before
// the fix the single-shot re-arm into cs.buf at offset 0 overwrote the partial
// prefix, so detection never converged.
func TestDetectSpanningH2Preface(t *testing.T) {
	ring := newTestRing(t)

	noopHandler := stream.HandlerFunc(func(_ context.Context, _ *stream.Stream) error { return nil })
	w := &Worker{
		ring:        ring,
		conns:       make([]*connState, 1024),
		liveConns:   make([]int, 0, 4),
		handler:     noopHandler,
		h2EventFD:   -1,
		errCount:    &atomic.Uint64{},
		reqCount:    &atomic.Uint64{},
		activeConns: &atomic.Int64{},
		cfg:         resource.Config{Protocol: engine.Auto},
	}

	const fd = 9
	cs := &connState{
		fd:      fd,
		liveIdx: -1,
		ctx:     context.Background(),
		buf:     make([]byte, 4096),
	}
	cs.protocol.Store(int32(engine.Auto))
	cs.writeFn = func([]byte) {} // swallow H2 server SETTINGS
	w.conns[fd] = cs
	w.addLiveConn(cs)
	w.maxFD = fd

	preface := []byte(frame.ClientPreface)
	if len(preface) != 24 {
		t.Fatalf("unexpected preface length %d", len(preface))
	}

	for i := 0; i < len(preface); i++ {
		// Single-shot recv writes the next byte at cs.buf offset 0.
		cs.buf[0] = preface[i]
		w.handleRecv(&completionEntry{Res: 1, Flags: 0}, fd, int64(i))

		if i < len(preface)-1 {
			if cs.detected {
				t.Fatalf("detected after only %d/%d preface bytes", i+1, len(preface))
			}
			if got := len(cs.detectAccum); got != i+1 {
				t.Fatalf("after %d bytes: detectAccum len = %d, want %d (bytes lost across recvs)", i+1, got, i+1)
			}
		}
	}

	if !cs.detected {
		t.Fatalf("preface not detected after all %d bytes fed one at a time", len(preface))
	}
	if engine.Protocol(cs.protocol.Load()) != engine.H2C {
		t.Fatalf("detected protocol = %v, want H2C", engine.Protocol(cs.protocol.Load()))
	}
	// Accumulator is reset once detection converges.
	if len(cs.detectAccum) != 0 {
		t.Errorf("detectAccum not reset after detection: len = %d", len(cs.detectAccum))
	}
}
