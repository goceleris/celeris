//go:build linux

package epoll

import (
	"context"
	"testing"
)

// celeris#870: the bytes held for protocol detection are counted in
// connState.detectN, and a pooled connState must not carry that count into the
// next connection it serves, whose first read would land after bytes that are
// not its own.
func TestReleaseConnStateClearsHeldDetectBytes870(t *testing.T) {
	cs := acquireConnState(context.Background(), 9, 4096, false)
	cs.detectN = 7
	releaseConnState(cs)
	if cs.detectN != 0 {
		t.Fatalf("celeris870: releaseConnState left detectN = %d, want 0", cs.detectN)
	}
}
