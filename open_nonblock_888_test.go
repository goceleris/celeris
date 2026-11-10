//go:build !windows

package celeris

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// openNonblock does not wait in open(2) for the writer of a FIFO: a regular file
// swapped for a FIFO between the stat and the open is then caught by the check
// on the open descriptor, instead of holding the request.
func TestOpenNonblockDoesNotWaitOnAFIFO888(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Skipf("no mkfifo here: %v", err)
	}
	done := make(chan *os.File, 1)
	go func() {
		f, _ := openNonblock(p)
		done <- f
	}()
	select {
	case f := <-done:
		if f == nil {
			t.Fatal("openNonblock of a FIFO failed")
		}
		_ = f.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("openNonblock of a FIFO is waiting for a writer")
	}
	// a regular file still reads
	reg := filepath.Join(t.TempDir(), "reg")
	if err := os.WriteFile(reg, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := openNonblock(reg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, 8)
	if n, _ := f.Read(b); string(b[:n]) != "data" {
		t.Errorf("a regular file opened non-blocking read %q, want data", b[:n])
	}
}
