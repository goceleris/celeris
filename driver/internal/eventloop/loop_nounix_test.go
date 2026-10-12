//go:build !unix

package eventloop

import (
	"errors"
	"testing"
)

// A descriptor number cannot become a net.Conn without unix descriptors, so
// RegisterConn says so instead of guessing; the drivers register the net.Conn.
func TestRegisterConnIsUnsupportedWithoutUnixDescriptors(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	err = l.WorkerLoop(0).RegisterConn(3, func([]byte) {}, func(error) {})
	if !errors.Is(err, ErrNoFDRegistration) {
		t.Fatalf("RegisterConn: got %v, want ErrNoFDRegistration", err)
	}
}
