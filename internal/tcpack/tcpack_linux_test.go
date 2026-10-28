//go:build linux

package tcpack

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func reset(probe func() bool) {
	filled.Store(false)
	once = sync.Once{}
	probeFn = probe
}

// This host's stack fills bytes_acked (Linux since 4.1), and the probe sees it.
func TestProbeSeesTheHostStack889(t *testing.T) {
	t.Cleanup(func() { reset(probeLoopback) })
	if !probeLoopback() {
		t.Fatal("the loopback probe found no acknowledged bytes on a Linux stack")
	}
}

// A 0 is a count only once the stack is known to fill the field.
func TestUsableLatch889(t *testing.T) {
	t.Cleanup(func() { reset(probeLoopback) })
	var probes atomic.Int32
	reset(func() bool { probes.Add(1); return false }) // a stack that does not fill it

	if Usable(0, errors.New("getsockopt")) {
		t.Fatal("a failed read was usable")
	}
	if Usable(0, nil) || Usable(0, nil) {
		t.Fatal("0 on a stack that does not fill the field was a count")
	}
	if probes.Load() != 1 {
		t.Fatalf("the probe ran %d times, want once", probes.Load())
	}
	if !Usable(4096, nil) {
		t.Fatal("a count above 0 was not usable")
	}
	if !Usable(0, nil) {
		t.Fatal("after a socket reported a count, 0 stayed unusable (the latch is one-way)")
	}
	// A stack whose probe sees the field filled: 0 is a count at once.
	reset(func() bool { return true })
	if !Usable(0, nil) {
		t.Fatal("0 on a stack that fills the field was not a count")
	}
	if Usable(0, errors.New("x")) {
		t.Fatal("an error is never usable")
	}
}
