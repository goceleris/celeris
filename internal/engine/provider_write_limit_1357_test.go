package engine

import (
	"errors"
	"testing"
)

func TestWriteTooLargeError1357(t *testing.T) {
	var err error = &WriteTooLargeError{Size: 70 << 20, Limit: DriverWriteLimit}
	if want := "celeris/engine: write of 73400320 bytes exceeds the per-write limit of 64 MiB"; err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err, want)
	}
	if !errors.Is(err, ErrQueueFull) {
		t.Fatal("a *WriteTooLargeError does not match ErrQueueFull")
	}
	if DriverWriteLimit != 64<<20 {
		t.Fatalf("DriverWriteLimit = %d, want 64 MiB", DriverWriteLimit)
	}
}
