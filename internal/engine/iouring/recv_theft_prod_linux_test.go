//go:build linux && !validation

package iouring

import (
	"testing"
	"unsafe"

	"github.com/goceleris/celeris/internal/recvtheft"
)

// TestRecvTheftWitnessCompilesAway pins that the celeris#715 witness costs a
// production build nothing: recvtheft.Enabled is false, so its call sites
// compile away, and connState.recvArmSeq is zero-size and not the last field
// (a trailing zero-size field is the one kind Go may pad for), so
// kernelInflight, the field after it, sits where its own alignment puts it and
// the connState layout is unchanged. Measured at the time of writing:
// linux/amd64 and linux/arm64 both 600 bytes, with and without the field.
func TestRecvTheftWitnessCompilesAway(t *testing.T) {
	if recvtheft.Enabled {
		t.Fatal("recvtheft.Enabled is true in a build without the validation tag")
	}
	var cs connState
	if n := unsafe.Sizeof(cs.recvArmSeq); n != 0 {
		t.Fatalf("connState.recvArmSeq is %d bytes in production, want 0", n)
	}
	at, next, align := unsafe.Offsetof(cs.recvArmSeq), unsafe.Offsetof(cs.kernelInflight), unsafe.Alignof(cs.kernelInflight)
	if want := (at + align - 1) &^ (align - 1); next != want {
		t.Fatalf("connState.recvArmSeq at offset %d moves kernelInflight to %d, want %d (its alignment, %d, alone)", at, next, want, align)
	}
	if last := unsafe.Offsetof(cs.recvOutstanding); at >= last {
		t.Fatalf("connState.recvArmSeq (offset %d) must not be the last field (recvOutstanding is at %d): a trailing zero-size field can add padding", at, last)
	}
}
