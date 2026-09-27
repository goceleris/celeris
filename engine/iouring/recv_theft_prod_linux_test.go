//go:build linux && !validation

package iouring

import (
	"testing"
	"unsafe"

	"github.com/goceleris/celeris/internal/recvtheft"
)

// TestRecvTheftWitnessCompilesAway pins that the celeris#715 witness costs a
// production build nothing: recvtheft.Enabled is false, so its call sites
// compile away, and connState.recvArmSeq is zero-size and shares its offset
// with the field after it, so the connState layout is unchanged.
func TestRecvTheftWitnessCompilesAway(t *testing.T) {
	if recvtheft.Enabled {
		t.Fatal("recvtheft.Enabled is true in a build without the validation tag")
	}
	var cs connState
	if n := unsafe.Sizeof(cs.recvArmSeq); n != 0 {
		t.Fatalf("connState.recvArmSeq is %d bytes in production, want 0", n)
	}
	if a, b := unsafe.Offsetof(cs.recvArmSeq), unsafe.Offsetof(cs.kernelInflight); a != b {
		t.Fatalf("connState.recvArmSeq at offset %d, kernelInflight at %d: the zero-size field must not add padding", a, b)
	}
}
